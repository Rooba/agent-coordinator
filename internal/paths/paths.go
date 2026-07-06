package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

func Socket() string {
	if p := os.Getenv("AC_SOCKET"); p != "" {
		return p
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "agent-coordinator.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-coordinator-%d.sock", os.Getuid()))
}

func DB() (string, error) {
	if p := os.Getenv("AC_DB"); p != "" {
		return p, os.MkdirAll(filepath.Dir(p), 0o700)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "agent-coordinator")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "coordinator.db"), nil
}
