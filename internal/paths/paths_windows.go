//go:build windows

package paths

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type knownFolderPathFunc func(*windows.KNOWNFOLDERID, uint32) (string, error)

// localAppData resolves the current user's canonical non-roaming application
// data via Windows, not the caller's environment. DONT_VERIFY keeps path
// selection independent of whether a particular harness can access the folder;
// baseDir reports any creation/access failure after every peer chooses one path.
func localAppData(knownFolderPath knownFolderPathFunc) (string, error) {
	base, err := knownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DONT_VERIFY)
	if err != nil {
		return "", fmt.Errorf("resolve canonical Windows LocalAppData: %w", err)
	}
	if base == "" {
		return "", fmt.Errorf("resolve canonical Windows LocalAppData: known-folder API returned an empty path")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("resolve canonical Windows LocalAppData: known-folder API returned non-absolute path %q", base)
	}
	return base, nil
}

// baseDir is the current user's LocalAppData\agent-coordinator directory. File
// names inside stay short because AF_UNIX socket paths are capped near 108
// bytes even on Windows.
func baseDir() (string, error) {
	base, err := localAppData(windows.KnownFolderPath)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "agent-coordinator")
	return dir, os.MkdirAll(dir, 0o700)
}

func defaultSocket() string {
	return socketPath(baseDir())
}

func socketPath(dir string, err error) string {
	if dir == "" {
		fmt.Fprintf(os.Stderr, "agent-coordinator: cannot resolve the Windows per-user socket directory: %v; set AC_SOCKET explicitly\n", err)
		os.Exit(1)
		return ""
	}
	// Keep using the one resolved directory even if its creation failed. The
	// listener will report that failure; switching to temp here would form a
	// second mesh for processes with a different environment.
	return filepath.Join(dir, "ac.sock")
}

func defaultDB() (string, error) {
	return dbPath(baseDir())
}

func dbPath(dir string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("cannot prepare the Windows per-user database directory; set AC_DB explicitly: %w", err)
	}
	return filepath.Join(dir, "coordinator.db"), nil
}

// checkTokenPerm is a no-op on Windows: the permission bits reported here are
// synthesized, and the ACL is what actually guards the file.
func checkTokenPerm(string, os.FileInfo) error { return nil }
