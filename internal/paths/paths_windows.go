//go:build windows

package paths

import (
	"os"
	"path/filepath"
)

// baseDir is %LOCALAPPDATA%\agent-coordinator: per-user, non-roaming. File
// names inside stay short because AF_UNIX socket paths are capped near 108
// bytes even on Windows.
func baseDir() (string, error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "agent-coordinator")
	return dir, os.MkdirAll(dir, 0o700)
}

func defaultSocket() string {
	dir, err := baseDir()
	if err != nil {
		dir = os.TempDir() // no %LOCALAPPDATA%: peers still agree via %TMP%
	}
	return filepath.Join(dir, "ac.sock")
}

func defaultDB() (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "coordinator.db"), nil
}
