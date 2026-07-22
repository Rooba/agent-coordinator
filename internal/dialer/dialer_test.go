package dialer

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/socktest"
)

// resetSpawn installs a stub spawn func, restoring the real one on cleanup.
// Each test uses its own socket path, so the cross-process stamp file
// (sock+".spawn") is naturally isolated per test.
func resetSpawn(t *testing.T, stub func(string) (<-chan struct{}, error)) {
	t.Helper()
	mu.Lock()
	spawn = stub
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		spawn = spawnDaemon
		mu.Unlock()
	})
}

// The core spawn-on-miss promise: the first dial misses, the "spawned daemon"
// (a listener that appears a beat later) binds, and Dial connects to it.
func TestDialConnectsToLateListener(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	lch := make(chan net.Listener, 1)
	resetSpawn(t, func(string) (<-chan struct{}, error) {
		go func() {
			time.Sleep(100 * time.Millisecond) // daemon "startup"
			if l, err := net.Listen("unix", sock); err == nil {
				lch <- l
			}
		}()
		return make(chan struct{}), nil // stays open: the "daemon" keeps running
	})
	conn, err := Dial(sock, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Dial must reach the late listener: %v", err)
	}
	conn.Close()
	(<-lch).Close()
}

// The throttle must hold across processes: hook clients are one process per
// event, so the gate is the stamp file next to the socket, not process state.
// Repeated misses inside the cooldown fork exactly one daemon while still
// telling every caller to keep redialing.
func TestSpawnThrottleIsStampFileBased(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	var attempts atomic.Int32
	resetSpawn(t, func(string) (<-chan struct{}, error) {
		attempts.Add(1)
		return make(chan struct{}), nil
	})
	for i := 0; i < 3; i++ {
		if retry, _ := maybeSpawn(sock); !retry {
			t.Fatalf("call %d: want retry=true while a spawn is in flight", i)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("want exactly 1 spawn within the cooldown, got %d", got)
	}
	if _, err := os.Stat(sock + ".spawn"); err != nil {
		t.Fatalf("stamp file must exist next to the socket: %v", err)
	}
}

// A fresh stamp written by another process throttles this one too: no spawn,
// but the caller still redials (the other process's daemon may be binding).
func TestSpawnThrottleHonorsForeignStamp(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	touch(sock + ".spawn") // "another process" just spawned
	resetSpawn(t, func(string) (<-chan struct{}, error) {
		t.Error("spawned despite a fresh stamp")
		return nil, errors.New("unreachable")
	})
	retry, died := maybeSpawn(sock)
	if !retry || died != nil {
		t.Fatalf("want retry=true with no died channel, got retry=%v died=%v", retry, died)
	}
}

// A spawned daemon that dies immediately (crash loop) must cut the redial
// window short instead of stalling the caller for the full retryWindow.
func TestDialAbortsWhenSpawnedDaemonDies(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	resetSpawn(t, func(string) (<-chan struct{}, error) {
		died := make(chan struct{})
		close(died) // the "daemon" is already dead
		return died, nil
	})
	start := time.Now()
	if _, err := Dial(sock, 10*time.Millisecond); err == nil {
		t.Fatal("dial to absent socket must fail")
	}
	if elapsed := time.Since(start); elapsed > retryWindow/2 {
		t.Fatalf("want early abort well under the %v window, waited %v", retryWindow, elapsed)
	}
}

// A spawn that cannot even start leaves no stamp and fails fast: nothing is
// coming up, so neither this caller nor other processes should sit in the
// redial window on its account.
func TestSpawnStartFailureFailsFastWithoutStamp(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	resetSpawn(t, func(string) (<-chan struct{}, error) { return nil, errors.New("spawn broken") })
	if retry, _ := maybeSpawn(sock); retry {
		t.Fatal("want retry=false when the spawn never started")
	}
	if _, err := os.Stat(sock + ".spawn"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed spawn must not leave a stamp: %v", err)
	}
}

func TestDialNoSpawnEnvDisablesSpawning(t *testing.T) {
	t.Setenv("AC_NO_SPAWN", "1")
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	resetSpawn(t, func(string) (<-chan struct{}, error) {
		t.Error("spawned despite AC_NO_SPAWN")
		return nil, nil
	})
	if _, err := Dial(sock, 10*time.Millisecond); err == nil {
		t.Fatal("dial to absent socket must fail")
	}
}
