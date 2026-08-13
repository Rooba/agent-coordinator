package hookcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/paths"
)

// Bind links a hook-registered session to the host agent's process tree so a
// sibling MCP server process can adopt the same identity (one session = one
// inbox). Files live at <statedir>/bind/<anchor_pid>.json, anchor = Pids[0].
type Bind struct {
	SessionID string `json:"session_id"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Pids      []int  `json:"pids"`
	TS        int64  `json:"ts"`
}

const bindMaxAge = 48 * time.Hour

// Seams for tests: where bind files live and how the pid chain is read.
var (
	bindDirFn  = paths.BindDir
	ancestryFn = Ancestry
)

// matchDepth caps how far up the caller's ancestry a bind may match: beyond
// the first few ancestors you are matching tmux/systemd, not the session.
const matchDepth = 3

// PidAlive reports whether a pid still exists. A host without /proc reports
// alive, keeping the bind mechanism fail-open off Linux. Var so tests control
// liveness.
var PidAlive = func(pid int) bool {
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		return true
	}
	_, err := os.Stat("/proc")
	return err != nil
}

// Ancestry returns this process's ancestor pids, parent first, walking /proc
// up to 6 levels and stopping before pid 1.
func Ancestry() []int {
	var out []int
	for pid, i := os.Getppid(), 0; pid > 1 && i < 6; i++ {
		out = append(out, pid)
		pid = ppidOf(pid)
	}
	return out
}

func ppidOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// stat format: "pid (comm) state ppid ..."; comm may contain spaces and
	// parentheses, so fields are taken after the last ')'.
	rest := string(b)
	i := strings.LastIndexByte(rest, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(rest[i+1:])
	if len(f) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return ppid
}

// WriteBind writes the bind file for b and opportunistically removes binds
// older than 48h.
func WriteBind(dir string, b Bind) error {
	if len(b.Pids) == 0 {
		return errors.New("no ancestor pids")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cleanStaleBinds(dir)
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", b.Pids[0])), data, 0o600)
}

func cleanStaleBinds(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-bindMaxAge)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		var b Bind
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &b) == nil && b.TS > 0 {
			if time.Unix(b.TS, 0).Before(cutoff) {
				os.Remove(p)
			}
			continue
		}
		// Unreadable or corrupt: fall back to file age.
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(p)
		}
	}
}

// MatchBind picks the bind for scope whose pid set sits nearest the caller in
// its ancestor chain (only the first matchDepth ancestors count), preferring
// the newest TS on ties. Binds whose anchor pid is dead or whose TS is stale
// never match - a sibling or finished session must not be adopted.
func MatchBind(dir, scope string, ancestors []int) (Bind, bool) {
	depth := map[int]int{}
	for i, p := range ancestors {
		if i >= matchDepth {
			break
		}
		if _, seen := depth[p]; p > 1 && !seen {
			depth[p] = i
		}
	}
	if len(depth) == 0 {
		return Bind{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Bind{}, false
	}
	cutoff := time.Now().Add(-bindMaxAge).Unix()
	var best Bind
	bestDepth := -1
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var b Bind
		if json.Unmarshal(data, &b) != nil || b.Scope != scope || b.SessionID == "" ||
			len(b.Pids) == 0 || b.TS < cutoff || !PidAlive(b.Pids[0]) {
			continue
		}
		d := -1
		for _, p := range b.Pids {
			if i, ok := depth[p]; ok && p > 1 && (d < 0 || i < d) {
				d = i
			}
		}
		if d < 0 {
			continue
		}
		if bestDepth < 0 || d < bestDepth || (d == bestDepth && b.TS > best.TS) {
			best, bestDepth = b, d
		}
	}
	return best, bestDepth >= 0
}

// writeBindFile / refreshBindFile / removeBindFile are the fail-open hooks
// into Run: any error is silently dropped so bind upkeep can never block the
// host agent.
func writeBindFile(scope, sessionID, name string) {
	dir, err := bindDirFn()
	if err != nil {
		return
	}
	WriteBind(dir, Bind{SessionID: sessionID, Scope: scope, Name: name,
		Pids: ancestryFn(), TS: time.Now().Unix()})
}

// refreshBindFile rewrites a missing bind (e.g. after a reboot or cleanup);
// an existing one is left alone, keeping the name SessionStart recorded.
func refreshBindFile(scope, sessionID string) {
	dir, err := bindDirFn()
	if err != nil {
		return
	}
	pids := ancestryFn()
	if len(pids) == 0 {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.json", pids[0]))); err == nil {
		return
	}
	WriteBind(dir, Bind{SessionID: sessionID, Scope: scope, Pids: pids, TS: time.Now().Unix()})
}

func removeBindFile() {
	dir, err := bindDirFn()
	if err != nil {
		return
	}
	if pids := ancestryFn(); len(pids) > 0 {
		os.Remove(filepath.Join(dir, fmt.Sprintf("%d.json", pids[0])))
	}
}
