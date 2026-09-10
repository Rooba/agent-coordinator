//go:build windows

package paths

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLocalAppDataIsIndependentOfEnvironment(t *testing.T) {
	want, err := localAppData(windows.KnownFolderPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", `C:\different-harness`)
	t.Setenv("TMP", `C:\different-temp`)
	got, err := localAppData(windows.KnownFolderPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("localAppData() changed with the harness environment: got %q, want %q", got, want)
	}
}

func TestLocalAppDataUsesKnownFolder(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\different-harness`)
	t.Setenv("TMP", `C:\different-temp`)
	want := `C:\Users\Ada\AppData\Local`
	calls := 0
	got, err := localAppData(func(id *windows.KNOWNFOLDERID, flags uint32) (string, error) {
		calls++
		if id != windows.FOLDERID_LocalAppData || flags != windows.KF_FLAG_DONT_VERIFY {
			t.Fatalf("knownFolderPath(%p, %#x), want LocalAppData with KF_FLAG_DONT_VERIFY", id, flags)
		}
		return want, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("localAppData() = %q, want %q", got, want)
	}
	if calls != 1 {
		t.Fatalf("knownFolderPath called %d times, want 1", calls)
	}
}

func TestLocalAppDataRejectsInvalidKnownFolderPaths(t *testing.T) {
	for _, path := range []string{"", "relative", `C:relative`} {
		t.Run(path, func(t *testing.T) {
			_, err := localAppData(func(*windows.KNOWNFOLDERID, uint32) (string, error) {
				return path, nil
			})
			if err == nil {
				t.Fatalf("localAppData() accepted non-absolute path %q", path)
			}
		})
	}
}

func TestLocalAppDataReportsLookupFailure(t *testing.T) {
	want := errors.New("known-folder failure")
	_, err := localAppData(func(*windows.KNOWNFOLDERID, uint32) (string, error) {
		return "", want
	})
	if !errors.Is(err, want) {
		t.Fatalf("localAppData() error = %v, want wrapped lookup failure", err)
	}
}

func TestSocketPathNeverFallsBackToTemp(t *testing.T) {
	if os.Getenv("AC_TEST_SOCKET_PATH_FAILURE") == "1" {
		socketPath("", errors.New("no per-user directory"))
		t.Fatal("socketPath returned after an unrecoverable resolution failure")
	}

	dir := `C:\Users\Ada\AppData\Local\agent-coordinator`
	if got, want := socketPath(dir, errors.New("mkdir failed")), filepath.Join(dir, "ac.sock"); got != want {
		t.Fatalf("socketPath() = %q, want %q", got, want)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSocketPathNeverFallsBackToTemp$")
	cmd.Env = append(os.Environ(), "AC_TEST_SOCKET_PATH_FAILURE=1")
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("socketPath failure exit = %v, output %q; want exit 1", err, output)
	}
	want := "agent-coordinator: cannot resolve the Windows per-user socket directory: no per-user directory; set AC_SOCKET explicitly\n"
	if got := string(output); got != want {
		t.Fatalf("socketPath failure output = %q, want %q", got, want)
	}
}

func TestDBPathReportsOverride(t *testing.T) {
	want := errors.New("directory unavailable")
	_, err := dbPath("", want)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "set AC_DB explicitly") {
		t.Fatalf("dbPath() error = %v, want wrapped error with AC_DB guidance", err)
	}
}
