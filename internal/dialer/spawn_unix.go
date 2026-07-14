//go:build !windows

package dialer

import "syscall"

// Setsid detaches the daemon into its own session: it survives the client
// and never sees the client's terminal signals.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
