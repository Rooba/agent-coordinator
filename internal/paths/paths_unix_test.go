//go:build !windows

package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Every process of one uid must resolve the same socket, whether or not its
// launcher passed XDG_RUNTIME_DIR through.
func TestDefaultSocketPrefersRuntimeDir(t *testing.T) {
	realRun := runUserDir
	t.Cleanup(func() { runUserDir = realRun })

	t.Run("xdg set wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		if got, want := defaultSocket(), filepath.Join(dir, sockName); got != want {
			t.Fatalf("defaultSocket() = %q, want %q", got, want)
		}
	})

	t.Run("xdg unset falls back to run user dir", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		runUserDir = realRun
		if !ownedWritableDir(realRun) {
			t.Skipf("%s is not an owned writable directory on this host", realRun)
		}
		if got, want := defaultSocket(), filepath.Join(realRun, sockName); got != want {
			t.Fatalf("defaultSocket() = %q, want %q", got, want)
		}
	})

	t.Run("no run user dir falls back to tmp", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		runUserDir = filepath.Join(t.TempDir(), "absent")
		want := filepath.Join(os.TempDir(), fmt.Sprintf("agent-coordinator-%d", os.Getuid()), sockName)
		if got := defaultSocket(); got != want {
			t.Fatalf("defaultSocket() = %q, want %q", got, want)
		}
	})
}

// ownedWritableDir gates the runtime-dir fallback, so it must reject anything
// that is not a directory we own and can write.
func TestOwnedWritableDir(t *testing.T) {
	dir := t.TempDir()
	if !ownedWritableDir(dir) {
		t.Fatalf("ownedWritableDir(%q) = false, want true", dir)
	}

	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ownedWritableDir(file) {
		t.Fatalf("ownedWritableDir(%q) = true for a regular file", file)
	}
	if missing := filepath.Join(dir, "absent"); ownedWritableDir(missing) {
		t.Fatalf("ownedWritableDir(%q) = true for a missing path", missing)
	}

	if os.Getuid() == 0 {
		t.Skip("root bypasses the write permission check")
	}
	readOnly := filepath.Join(dir, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	if ownedWritableDir(readOnly) {
		t.Fatalf("ownedWritableDir(%q) = true for a non-writable directory", readOnly)
	}
}
