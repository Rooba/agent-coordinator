//go:build windows

package hostrunner

import (
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessGuard struct{ job windows.Handle }

func newProcessGuard() (processGuard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &windowsProcessGuard{job: job}, nil
}

// Go's os/exec does not expose a suspended-create/assign/resume hook. The
// process is assigned immediately after Start, but a malicious executable
// could theoretically spawn outside the job in that narrow interval. Avoid
// advertising unprobed executables; a bespoke Win32 launcher is deferred.
func (g *windowsProcessGuard) Configure(_ *exec.Cmd) {}

func (g *windowsProcessGuard) Attach(process *os.Process) error {
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.AssignProcessToJobObject(g.job, handle)
}

func (g *windowsProcessGuard) Terminate() {
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
}

func (g *windowsProcessGuard) Close() {
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}
