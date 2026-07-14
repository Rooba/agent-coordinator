//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes a non-blocking exclusive byte-range lock (byte 0) on f via
// LockFileEx. (false, nil) means a live peer holds it; the lock is released
// when f's handle closes or the process dies.
func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	if err == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return err == nil, err
}
