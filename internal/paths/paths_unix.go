//go:build !windows

package paths

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const sockName = "agent-coordinator.sock"

// runUserDir is this uid's login runtime directory; a var so tests can aim it
// at a controlled path.
var runUserDir = fmt.Sprintf("/run/user/%d", os.Getuid())

// defaultSocket resolves to the same socket for every process of this uid on
// this host. Launchers that drop XDG_RUNTIME_DIR must not land on a different
// path, or they start a second daemon whose in-memory state never joins the
// first one's.
func defaultSocket() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, sockName)
	}
	if ownedWritableDir(runUserDir) {
		return filepath.Join(runUserDir, sockName)
	}
	// No runtime dir: a private per-uid directory so another local user cannot
	// squat the socket path. If it exists with foreign ownership/perms, the
	// daemon's Listen fails on its own - no extra checks here.
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("agent-coordinator-%d", os.Getuid()))
	os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, sockName)
}

// ownedWritableDir reports whether path is a directory this uid owns and can
// create a socket in.
func ownedWritableDir(path string) bool {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFDIR && int(st.Uid) == os.Getuid() &&
		unix.Access(path, unix.W_OK|unix.X_OK) == nil
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
func checkTokenPerm(path string, fi os.FileInfo) error {
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("relay token file %s has mode %04o: it must not be readable by group or other - chmod 600 it or delete it", path, perm)
	}
	return nil
}
