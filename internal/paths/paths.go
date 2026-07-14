package paths

import (
	"os"
	"path/filepath"
)

// Socket returns the coordinator socket path: AC_SOCKET wins, then the
// platform default (paths_unix.go / paths_windows.go).
func Socket() string {
	if p := os.Getenv("AC_SOCKET"); p != "" {
		return p
	}
	return defaultSocket()
}

// DB returns the state database path, creating its directory: AC_DB wins,
// then the platform default.
func DB() (string, error) {
	if p := os.Getenv("AC_DB"); p != "" {
		return p, os.MkdirAll(filepath.Dir(p), 0o700)
	}
	return defaultDB()
}
