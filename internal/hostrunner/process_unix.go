//go:build !windows

package hostrunner

import (
	"os"
	"os/exec"
	"syscall"
)

type unixProcessGuard struct{ pid int }

func newProcessGuard() (processGuard, error) { return &unixProcessGuard{}, nil }

func (g *unixProcessGuard) Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (g *unixProcessGuard) Attach(process *os.Process) error {
	g.pid = process.Pid
	return nil
}

func (g *unixProcessGuard) Terminate() {
	if g.pid != 0 {
		_ = syscall.Kill(-g.pid, syscall.SIGKILL)
	}
}

func (g *unixProcessGuard) Close() { g.Terminate() }
