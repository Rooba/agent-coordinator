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
	// /tmp fallback: a private per-uid directory so another local user cannot
	// squat the socket path. If the dir exists with foreign ownership/perms,
	// the daemon's Listen fails on its own - no extra checks here.
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("agent-coordinator-%d", os.Getuid()))
	os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "agent-coordinator.sock")
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
