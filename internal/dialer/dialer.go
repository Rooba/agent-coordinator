// Package dialer connects clients to the coordinator daemon, starting it on
// demand: when nobody is listening on the socket, the client spawns
// "agent-coordinator daemon" detached and redials briefly. A stamp file next
// to the socket (sock+".spawn") throttles spawning across processes - hook
// clients are one process per event, so a process-local throttle alone would
// let a crash-looping daemon be forked on every tool call. The daemon takes
// an OS file lock before binding, so racing spawns self-resolve - the losers
// exit quietly. Any remaining failure is returned as-is, preserving each
// caller's fail-open behavior.
package dialer

import (
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	spawnCooldown = 5 * time.Second       // at most one spawn per cooldown, across all client processes
	retryWindow   = 2 * time.Second       // how long to wait for a spawned daemon to bind
	retryInterval = 50 * time.Millisecond // pause between redials inside the window
)

var (
	mu    sync.Mutex    // serializes in-process stamp check + spawn
	spawn = spawnDaemon // seam: tests fake the daemon instead of re-executing the test binary
)

// Dial connects to the daemon socket, spawning the daemon on a miss and
// redialing while it comes up. When the daemon this call spawned has already
// exited (crash loop), the redial window aborts early instead of stalling.
// On failure it returns the first dial error.
func Dial(socketPath string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err == nil {
		return conn, nil
	}
	retry, died := maybeSpawn(socketPath)
	if !retry {
		return nil, err
	}
	deadline := time.After(retryWindow)
	tick := time.NewTicker(retryInterval)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return nil, err
		case <-died: // our daemon exited; one last dial in case it lost a healthy bind race
			if conn, retryErr := net.DialTimeout("unix", socketPath, timeout); retryErr == nil {
				return conn, nil
			}
			return nil, err
		case <-tick.C:
			if conn, retryErr := net.DialTimeout("unix", socketPath, timeout); retryErr == nil {
				return conn, nil
			}
		}
	}
}

// maybeSpawn reports whether a freshly spawned daemon may be coming up (keep
// redialing) and, when this call did the spawning, a channel that closes when
// that daemon exits. The stamp file sock+".spawn" is the cross-process
// throttle: a fresh mtime means some process started a daemon within the
// cooldown, so redial but do not fork another. A spawn that cannot even start
// leaves no stamp and returns false - nothing is coming up, fail fast.
func maybeSpawn(sock string) (retry bool, died <-chan struct{}) {
	if os.Getenv("AC_NO_SPAWN") != "" {
		return false, nil
	}
	mu.Lock()
	defer mu.Unlock()
	stamp := sock + ".spawn"
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < spawnCooldown {
		return true, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, nil
	}
	if died, err = spawn(exe); err != nil {
		return false, nil
	}
	touch(stamp)
	return true, died
}

// touch creates or freshens the stamp file's mtime.
func touch(path string) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	now := time.Now()
	os.Chtimes(path, now, now)
}

// spawnDaemon starts "<exe> daemon" fully detached: nil stdio means the null
// device (the daemon must not inherit an MCP server's JSON-RPC pipes), and
// sysProcAttr (per-OS) severs the session / console. The returned channel
// closes when the child exits, which both reaps it (no zombie in a long-lived
// mcp client) and lets Dial cut the redial window short on a crash loop.
func spawnDaemon(exe string) (<-chan struct{}, error) {
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	died := make(chan struct{})
	go func() {
		cmd.Wait()
		close(died)
	}()
	return died, nil
}
