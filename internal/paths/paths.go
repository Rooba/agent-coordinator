package paths

import (
	"os"
	"path/filepath"
	"strings"
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

// BindDir returns the directory of hook-to-MCP identity bind files, next to
// the state database, creating it.
func BindDir() (string, error) {
	db, err := DB()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(db), "bind")
	return dir, os.MkdirAll(dir, 0o700)
}

// ClientAddr is where a client should reach the daemon: AC_ADDR when set
// ("unix:///path" or "tcp://host:port"), otherwise today's unix socket.
func ClientAddr() string {
	if a := strings.TrimSpace(os.Getenv("AC_ADDR")); a != "" {
		return a
	}
	return "unix://" + Socket()
}
