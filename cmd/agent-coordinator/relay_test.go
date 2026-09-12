package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

func TestCLITargetPrefersAgentID(t *testing.T) {
	ref := cliTarget("/home/ra/proj", "someone", "98ffc675471a")
	if ref == nil || ref.AgentID != "98ffc675471a" || ref.Scope != "/home/ra/proj" {
		t.Fatalf("got %+v", ref)
	}
	hex := cliTarget("/s", "98ffc675471a", "")
	if hex == nil || hex.AgentID != "98ffc675471a" || hex.Name != "" {
		t.Fatalf("12-hex to= must be agent_id: %+v", hex)
	}
	named := cliTarget("/s", "brisk-owl", "")
	if named == nil || named.Name != "brisk-owl" || named.AgentID != "" {
		t.Fatalf("name to=: %+v", named)
	}
}

func TestReadBodyLiteral(t *testing.T) {
	got, err := readBody("hello world", "")
	if err != nil || got != "hello world" {
		t.Fatalf("literal: %q %v", got, err)
	}
}

func TestReadBodyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "brief.txt")
	if err := os.WriteFile(p, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readBody("", p)
	if err != nil || got != "from-file" {
		t.Fatalf("file: %q %v", got, err)
	}
}

func TestCallerSessionMissing(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("GROK_SESSION_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("AC_SESSION_ID", "")
	if _, err := callerSession(); err == nil || !strings.Contains(err.Error(), "join") {
		t.Fatalf("want join instruction, got %v", err)
	}
}

func TestJoinTCPDoesNotLeakSecret(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	const secret = "super-secret-session"
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	got := make(chan protocol.Request, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var req protocol.Request
		json.Unmarshal(line, &req)
		got <- req
		resp, _ := json.Marshal(protocol.Response{OK: true, Name: "eyes-host", SessionSecret: secret})
		c.Write(append(resp, '\n'))
	}()
	cred := filepath.Join(t.TempDir(), "cred")
	t.Setenv("AC_TOKEN", token)
	t.Setenv("AC_SESSION_SECRET", "")
	args := []string{
		"-addr", "tcp://" + l.Addr().String(),
		"-cred-file", cred,
		"-kind", "eyes",
		"-scope", "/home/ra/proj",
		"-platform", "windows",
		"-caps", "browser.chrome",
		"-session-id", "join-test-1",
	}
	var stdout, stderr bytes.Buffer
	if err := doJoin(args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	out := stdout.String() + stderr.String() + strings.Join(args, " ")
	if strings.Contains(out, secret) || strings.Contains(out, token) {
		t.Fatalf("secret leaked in argv/output: %s", out)
	}
	if !strings.Contains(stdout.String(), "eyes-host") {
		t.Fatalf("stdout: %s", stdout.String())
	}
	stored, err := loadCred(cred)
	if err != nil || stored.SessionID != "join-test-1" {
		t.Fatalf("cred file: %+v %v", stored, err)
	}
	if _, err := hex.DecodeString(stored.SessionSecret); err != nil || len(stored.SessionSecret) != 64 {
		t.Fatalf("generated secret must be 64 hex: %q", stored.SessionSecret)
	}
	if strings.Contains(out, stored.SessionSecret) {
		t.Fatalf("generated secret leaked: %s", out)
	}
	st, err := os.Stat(cred)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cred mode: %v %v", st.Mode(), err)
	}
	seen := <-got
	if seen.Token != token || seen.Kind != protocol.KindEyes || seen.Platform != "windows" ||
		len(seen.Capabilities) != 1 || seen.Capabilities[0] != "browser.chrome" ||
		seen.SessionSecret != stored.SessionSecret {
		t.Fatalf("register: %+v want secret %s", seen, stored.SessionSecret)
	}
}

func TestTCPJoinRejectsBadACToken(t *testing.T) {
	t.Setenv("AC_TOKEN", "not-hex")
	err := doJoin([]string{
		"-addr", "tcp://127.0.0.1:9",
		"-cred-file", filepath.Join(t.TempDir(), "cred"),
		"-kind", "eyes",
		"-session-id", "s1",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "AC_TOKEN") {
		t.Fatalf("want AC_TOKEN shape error, got %v", err)
	}
}

func TestUnixApplyRelayAuthOmitsCreds(t *testing.T) {
	t.Setenv("AC_TOKEN", "should-not-attach")
	t.Setenv("AC_SESSION_SECRET", "should-not-attach")
	req := protocol.Request{Op: protocol.OpSendWorkspace}
	if err := applyRelayAuth(&req, "unix:///tmp/x.sock", ""); err != nil {
		t.Fatal(err)
	}
	if req.Token != "" || req.SessionSecret != "" {
		t.Fatalf("unix must not attach relay creds: %+v", req)
	}
}

func TestJoinPersistFailBeforeSuccess(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	dereg := make(chan protocol.Request, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req protocol.Request
			json.Unmarshal(line, &req)
			dereg <- req
			resp, _ := json.Marshal(protocol.Response{OK: true, Name: "eyes-host", SessionSecret: "sec"})
			c.Write(append(resp, '\n'))
			c.Close()
		}
	}()
	bad := filepath.Join(t.TempDir(), "not-a-file")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_TOKEN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	var stdout, stderr bytes.Buffer
	err = doJoin([]string{
		"-addr", "tcp://" + l.Addr().String(),
		"-cred-file", bad,
		"-kind", "eyes",
		"-session-id", "join-fail-1",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected cred persist error")
	}
	if stdout.Len() != 0 {
		t.Fatalf("success printed before persist: %s", stdout.String())
	}
	select {
	case req := <-dereg:
		t.Fatalf("persist fail must not register: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestJoinReusesCredSessionID(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan protocol.Request, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var req protocol.Request
		json.Unmarshal(line, &req)
		got <- req
		resp, _ := json.Marshal(protocol.Response{OK: true, Name: "eyes-host", SessionSecret: "new-secret"})
		c.Write(append(resp, '\n'))
	}()
	cred := filepath.Join(t.TempDir(), "cred")
	if err := saveCred(cred, sessionCred{SessionID: "saved-session", SessionSecret: "old-secret"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_TOKEN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("AC_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("GROK_SESSION_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	var stdout, stderr bytes.Buffer
	if err := doJoin([]string{
		"-addr", "tcp://" + l.Addr().String(),
		"-cred-file", cred,
		"-kind", "eyes",
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	seen := <-got
	if seen.SessionID != "saved-session" {
		t.Fatalf("want reused session id, got %s", seen.SessionID)
	}
	stored, err := loadCred(cred)
	if err != nil || stored.SessionID != "saved-session" || stored.SessionSecret != "old-secret" {
		t.Fatalf("existing secret must be reused: %+v %v", stored, err)
	}
}

func TestJoinPreservesSecretWhenDaemonOmitsIt(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		bufio.NewReader(c).ReadBytes('\n')
		resp, _ := json.Marshal(protocol.Response{OK: true, Name: "eyes-host"})
		c.Write(append(resp, '\n'))
	}()
	cred := filepath.Join(t.TempDir(), "cred")
	if err := saveCred(cred, sessionCred{SessionID: "saved-session", SessionSecret: "keep-me"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_TOKEN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("AC_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("GROK_SESSION_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	var stdout, stderr bytes.Buffer
	if err := doJoin([]string{
		"-addr", "tcp://" + l.Addr().String(),
		"-cred-file", cred,
		"-kind", "eyes",
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stored, err := loadCred(cred)
	if err != nil || stored.SessionSecret != "keep-me" || stored.SessionID != "saved-session" {
		t.Fatalf("secret must be preserved: %+v %v", stored, err)
	}
}

func TestJoinRejectsConflictingSessionID(t *testing.T) {
	cred := filepath.Join(t.TempDir(), "cred")
	if err := saveCred(cred, sessionCred{SessionID: "saved-session", SessionSecret: "keep-me"}); err != nil {
		t.Fatal(err)
	}
	err := doJoin([]string{
		"-addr", "tcp://127.0.0.1:9",
		"-cred-file", cred,
		"-session-id", "other-session",
		"-kind", "eyes",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not match cred-file") {
		t.Fatalf("want conflict error, got %v", err)
	}
}

func TestUnixSockAddrIgnoresACAddr(t *testing.T) {
	t.Setenv("AC_ADDR", "tcp://127.0.0.1:9")
	addr := unixSockAddr()
	if !strings.HasPrefix(addr, "unix://") || strings.Contains(addr, "127.0.0.1:9") {
		t.Fatalf("unix-only helper routed to AC_ADDR: %s", addr)
	}
	req := protocol.Request{Op: protocol.OpSendWorkspace}
	if err := applyRelayAuth(&req, addr, ""); err != nil {
		t.Fatal(err)
	}
	if req.Token != "" || req.SessionSecret != "" {
		t.Fatalf("unix helper must not attach creds: %+v", req)
	}
}

// humanDur is what makes a second count readable at a glance, in both
// directions: a deadline that has passed is rendered by its size, and how far
// past it is said in words next to it.
func TestHumanDur(t *testing.T) {
	for _, c := range []struct {
		secs int
		want string
	}{
		{secs: 0, want: "0s"},
		{secs: 59, want: "59s"},
		{secs: 60, want: "1m00s"},
		{secs: 782, want: "13m02s"},
		{secs: 3599, want: "59m59s"},
		{secs: 3600, want: "1h00m"},
		{secs: 7380, want: "2h03m"},
		{secs: -480, want: "8m00s"},
	} {
		if got := humanDur(c.secs); got != c.want {
			t.Errorf("humanDur(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// The task row has to answer "is it alive and how long has it got?" without
// the reader doing arithmetic, and it must say outright when a task is past
// its deadline with nothing running behind it.
func TestEyesTaskLineShowsDeadlineState(t *testing.T) {
	row := func(state string, elapsed, remaining int, overdue bool) protocol.EyesTaskInfo {
		return protocol.EyesTaskInfo{TaskID: "task-0123456789ab", Runtime: "claude", State: state,
			RequesterScope: "/r", RequesterAgentID: "aid-a", DeadlineS: 300,
			ElapsedS: elapsed, RemainingS: remaining, Overdue: overdue}
	}
	for _, c := range []struct {
		name   string
		task   protocol.EyesTaskInfo
		want   []string
		absent string
	}{
		{name: "queued", task: row("queued", 12, 288, false),
			want: []string{"task-0123456789ab", "claude", "queued", "elapsed=12s",
				"remaining=4m48s", "aid-a@/r"}},
		{name: "accepted", task: row("accepted", 780, 1020, false),
			want: []string{"accepted", "elapsed=13m00s", "remaining=17m00s"}},
		{name: "overdue accepted", task: row("accepted", 780, -480, true),
			want: []string{"accepted", "elapsed=13m00s", "OVERDUE by 8m00s"}, absent: "remaining="},
		{name: "terminal", task: row("done", 900, -600, false),
			want: []string{"done", "elapsed=15m00s", "remaining=-"}, absent: "OVERDUE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := eyesTaskLine(c.task)
			for _, want := range c.want {
				if !strings.Contains(line, want) {
					t.Fatalf("line %q missing %q", line, want)
				}
			}
			if c.absent != "" && strings.Contains(line, c.absent) {
				t.Fatalf("line %q must not contain %q", line, c.absent)
			}
		})
	}
}

// A stalled task deliberately sends no mail, so the row is where a reader
// finds out it has gone quiet - and it must never show what a tool was
// looking at, only its name.
func TestEyesTaskLineShowsProgressAndStall(t *testing.T) {
	row := func(turns int, tool string, hb int64, age int, stale bool) protocol.EyesTaskInfo {
		return protocol.EyesTaskInfo{TaskID: "task-0123456789ab", Runtime: "claude", State: "accepted",
			RequesterScope: "/r", RequesterAgentID: "aid-a", DeadlineS: 300, ElapsedS: 60,
			RemainingS: 240, Turns: turns, Tool: tool, HeartbeatAt: hb, HeartbeatAgeS: age,
			HeartbeatStale: stale}
	}
	for _, c := range []struct {
		name   string
		task   protocol.EyesTaskInfo
		want   []string
		absent string
	}{
		{name: "moving", task: row(7, "mcp__claude-in-chrome__click", 1000000, 12, false),
			want: []string{"turns=7", "tool=mcp__claude-in-chrome__click", "hb=12s"}, absent: "STALE"},
		{name: "gone quiet", task: row(7, "Read", 1000000, 310, true),
			want: []string{"turns=7", "hb=STALE:5m10s"}},
		{name: "never reported", task: row(0, "", 0, 0, true), want: []string{"hb=STALE:never"},
			absent: "turns="},
		{name: "no sample yet", task: row(0, "", 0, 0, false), want: []string{"hb=-"}, absent: "STALE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := eyesTaskLine(c.task)
			for _, want := range c.want {
				if !strings.Contains(line, want) {
					t.Fatalf("line %q missing %q", line, want)
				}
			}
			if c.absent != "" && strings.Contains(line, c.absent) {
				t.Fatalf("line %q must not contain %q", line, c.absent)
			}
		})
	}
}
