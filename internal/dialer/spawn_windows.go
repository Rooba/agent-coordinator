//go:build windows

package dialer

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// The daemon must survive its parent and own no console: DETACHED_PROCESS
// (absent from stdlib syscall, hence x/sys/windows) plus a new process group
// so console ctrl events never propagate to it.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
