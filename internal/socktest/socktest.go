// Package socktest provides short filesystem paths for Unix socket
// binds in tests. t.TempDir embeds the full test name, and on macOS
// runners (TMPDIR under /var/folders/...) the resulting socket path can
// exceed the sun_path limit (104 bytes on darwin, 108 on linux), which
// fails with EINVAL at bind time.
package socktest

import (
	"os"
	"testing"
)

// Dir returns a temp dir with a short random suffix directly under
// TMPDIR, keeping socket paths created inside it under the sun_path
// limit. The dir is removed when the test ends; removal failure fails
// the test so leaked handles (e.g. on windows) stay visible.
func Dir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "acs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("socktest cleanup: %v", err)
		}
	})
	return dir
}
