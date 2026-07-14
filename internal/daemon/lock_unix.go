//go:build !windows

package daemon

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes a non-blocking exclusive flock on f. (false, nil) means a
// live peer holds it; the lock is released when f closes or the process dies.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK {
		return false, nil
	}
	return err == nil, err
}
