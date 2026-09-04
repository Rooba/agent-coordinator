//go:build !windows

package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

func defaultSocket() string {
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

func defaultDB() (string, error) {
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

// checkTokenPerm refuses a relay token any other local account can reach. The
// file is the whole relay's shared secret, so what the owner's own bits say
// is their business - group and other access is what makes it not a secret.
func checkTokenPerm(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("relay token file %s has mode %04o, want 0600: chmod 600 it or delete it", path, perm)
	}
	return nil
}
