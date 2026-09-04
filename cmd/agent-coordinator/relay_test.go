package main

import (
	"bufio"
	"bytes"
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
	const token = "super-secret-token"
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
	if err != nil || stored.SessionSecret != secret || stored.SessionID != "join-test-1" {
		t.Fatalf("cred file: %+v %v", stored, err)
	}
	st, err := os.Stat(cred)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cred mode: %v %v", st.Mode(), err)
	}
	seen := <-got
	if seen.Token != token || seen.Kind != protocol.KindEyes || seen.Platform != "windows" ||
		len(seen.Capabilities) != 1 || seen.Capabilities[0] != "browser.chrome" {
		t.Fatalf("register: %+v", seen)
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
	t.Setenv("AC_TOKEN", "tok")
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
	// register then best-effort deregister
	first := <-dereg
	if first.Op != protocol.OpRegister {
		t.Fatalf("first op %s", first.Op)
	}
	select {
	case second := <-dereg:
		if second.Op != protocol.OpDeregister || second.SessionSecret != "sec" || second.SessionID != "join-fail-1" {
			t.Fatalf("deregister must carry session secret: %+v", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected deregister after persist fail")
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
	t.Setenv("AC_TOKEN", "tok")
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
	if err != nil || stored.SessionID != "saved-session" || stored.SessionSecret != "new-secret" {
		t.Fatalf("updated cred: %+v %v", stored, err)
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
	t.Setenv("AC_TOKEN", "tok")
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
