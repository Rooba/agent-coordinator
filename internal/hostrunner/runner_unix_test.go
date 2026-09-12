//go:build !windows

package hostrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunnerTerminatesDescendants(t *testing.T) {
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	runner := newHelperRunner(t, &helperProvider{mode: "tree", childPIDPath: childPIDPath}, 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := runner.Run(ctx, helperTask(13), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tree timeout error = %v", err)
	}
	data, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived task termination", pid)
}
