//go:build windows

package hostbroker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type dpapiProtector struct {
	protect func([]byte, bool) ([]byte, error)
}

const (
	credentialTarget   = "Rooba_agent-coordinator_hostbroker_v1"
	credTypeGeneric    = 1
	credPersistLocal   = 2
	credentialMaxBlob  = 2560
	taskNotRunningCode = 0x8004130b
)

var (
	advapi32      = windows.NewLazySystemDLL("advapi32.dll")
	procCredRead  = advapi32.NewProc("CredReadW")
	procCredWrite = advapi32.NewProc("CredWriteW")
	procCredFree  = advapi32.NewProc("CredFree")
)

type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

type windowsCredentialStore struct {
	read  func() ([]byte, error)
	write func([]byte) error
	lock  func(context.Context) (Unlock, error)
}

func (s windowsCredentialStore) Load(context.Context) (Credential, error) {
	data, err := s.read()
	if err != nil {
		return Credential{}, err
	}
	return decodeCredential(data)
}

func readCredentialBlob() ([]byte, error) {
	target, err := windows.UTF16PtrFromString(credentialTarget)
	if err != nil {
		return nil, err
	}
	var raw *credentialW
	ok, _, callErr := procCredRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&raw)))
	runtime.KeepAlive(target)
	if ok == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil, ErrCredentialNotFound
		}
		return nil, fmt.Errorf("read Windows host credential: %w", callErr)
	}
	if raw == nil {
		return nil, errors.New("Windows host credential is empty")
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(raw)))
	if raw.CredentialBlob == nil || raw.CredentialBlobSize == 0 || raw.CredentialBlobSize > credentialMaxBlob {
		return nil, errors.New("Windows host credential is empty or oversized")
	}
	return append([]byte(nil), unsafe.Slice(raw.CredentialBlob, int(raw.CredentialBlobSize))...), nil
}

func (s windowsCredentialStore) save(credential Credential) error {
	data, err := encodeCredential(credential)
	if err != nil {
		return err
	}
	return s.write(data)
}

func (s windowsCredentialStore) Update(ctx context.Context, update func(*Credential) error) error {
	if update == nil {
		return errors.New("credential update is required")
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	credential, err := s.Load(ctx)
	if errors.Is(err, ErrCredentialNotFound) {
		credential, err = Credential{}, nil
	}
	if err == nil {
		err = update(&credential)
	}
	if err != nil {
		return err
	}
	return s.save(credential)
}

func writeCredentialBlob(data []byte) error {
	if len(data) > credentialMaxBlob {
		return errors.New("Windows host credential exceeds Credential Manager limit")
	}
	target, err := windows.UTF16PtrFromString(credentialTarget)
	if err != nil {
		return err
	}
	entry := credentialW{
		Type: credTypeGeneric, TargetName: target, CredentialBlobSize: uint32(len(data)),
		CredentialBlob: &data[0], Persist: credPersistLocal,
	}
	ok, _, callErr := procCredWrite.Call(uintptr(unsafe.Pointer(&entry)), 0)
	runtime.KeepAlive(target)
	runtime.KeepAlive(data)
	if ok == 0 {
		return fmt.Errorf("write Windows host credential: %w", callErr)
	}
	return nil
}

func (p dpapiProtector) Seal(plain []byte) ([]byte, error) {
	return p.protect(plain, true)
}

func (p dpapiProtector) Open(sealed []byte) ([]byte, error) {
	return p.protect(sealed, false)
}

func protectData(data []byte, seal bool) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("DPAPI input is empty")
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	var err error
	if seal {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	runtime.KeepAlive(data)
	if err != nil {
		return nil, err
	}
	if out.Data == nil {
		return nil, errors.New("DPAPI returned empty output")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	if out.Size == 0 {
		return nil, errors.New("DPAPI returned empty output")
	}
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}

func OpenPlatformState() (CredentialStore, Journal, error) {
	credentials, err := OpenPlatformCredentialStore()
	if err != nil {
		return nil, nil, err
	}
	journal, err := OpenPlatformJournal()
	if err != nil {
		return nil, nil, err
	}
	return credentials, journal, nil
}

func OpenPlatformCredentialStore() (CredentialStore, error) {
	return windowsCredentialStore{read: readCredentialBlob, write: writeCredentialBlob, lock: acquireCredentialLock}, nil
}

func acquireCredentialLock(ctx context.Context) (Unlock, error) {
	name, err := currentUserMutexName("credential")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	for {
		event, waitErr := windows.WaitForSingleObject(handle, 100)
		switch event {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			return releaseMutex(handle), nil
		case 258: // WAIT_TIMEOUT
			if err := ctx.Err(); err != nil {
				_ = windows.CloseHandle(handle)
				return nil, err
			}
		default:
			_ = windows.CloseHandle(handle)
			return nil, waitErr
		}
	}
}

func OpenPlatformJournal() (Journal, error) {
	return newPlatformJournal("host.journal")
}

func OpenPlatformConfigStore() (ConfigStore, error) {
	return newPlatformConfigStore("host.config")
}

func newPlatformJournal(name string) (Journal, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "agent-coordinator")
	protector := dpapiProtector{protect: protectData}
	journal, err := NewFileJournal(filepath.Join(dir, name), protector)
	if err != nil {
		return nil, err
	}
	return journal, nil
}

func newPlatformConfigStore(name string) (ConfigStore, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return NewFileConfigStore(filepath.Join(base, "agent-coordinator", name), dpapiProtector{protect: protectData})
}

func AcquirePlatformLock() (Unlock, error) {
	name, err := currentUserMutexName("instance")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, true, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, ErrBrokerAlreadyRunning
	}
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, err
	}
	return releaseMutex(handle), nil
}

func currentUserMutexName(purpose string) (*uint16, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(user.User.Sid.String()))
	return windows.UTF16PtrFromString(fmt.Sprintf(`Global\Rooba_agent-coordinator_hostbroker_%s_%x`, purpose, hash[:8]))
}

func releaseMutex(handle windows.Handle) Unlock {
	return func() error {
		releaseErr := windows.ReleaseMutex(handle)
		closeErr := windows.CloseHandle(handle)
		if releaseErr != nil {
			return releaseErr
		}
		return closeErr
	}
}

func ManageAutostart(ctx context.Context, action ScheduleAction, executable string, dryRun bool) (SchedulePlan, error) {
	if err := validateScheduleExecutable(action, executable); err != nil {
		return SchedulePlan{}, err
	}
	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		return SchedulePlan{}, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return SchedulePlan{}, err
	}
	return applySchedule(ctx, action, executable, user.User.Sid.String(), filepath.Join(systemDir, "schtasks.exe"), dryRun,
		func(ctx context.Context, executable string, args ...string) error {
			output, err := exec.CommandContext(ctx, executable, args...).CombinedOutput()
			if err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) && uint32(exit.ExitCode()) == taskNotRunningCode {
					return errTaskNotRunning
				}
				return fmt.Errorf("Task Scheduler command failed: %w: %s", err, string(output))
			}
			return nil
		})
}
