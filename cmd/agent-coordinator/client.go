package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/dialer"
	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
)

type sessionCred struct {
	SessionID     string `json:"session_id"`
	SessionSecret string `json:"session_secret"`
}

func coordinatorAddr() string {
	if v := strings.TrimSpace(os.Getenv("AC_ADDR")); v != "" {
		return v
	}
	return unixSockAddr()
}

func unixSockAddr() string {
	return "unix://" + paths.Socket()
}

func isTCPAddr(addr string) bool {
	return strings.HasPrefix(addr, "tcp://")
}

func dialCoordinator(addr string, timeout time.Duration) (net.Conn, error) {
	if addr == "" {
		addr = coordinatorAddr()
	}
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		return net.DialTimeout("tcp", strings.TrimPrefix(addr, "tcp://"), timeout)
	case strings.HasPrefix(addr, "unix://"):
		return dialer.Dial(strings.TrimPrefix(addr, "unix://"), timeout)
	default:
		return dialer.Dial(addr, timeout)
	}
}

func applyRelayAuth(req *protocol.Request, addr, credFile string) error {
	if !isTCPAddr(addr) {
		return nil
	}
	token := strings.TrimSpace(os.Getenv("AC_TOKEN"))
	secret := strings.TrimSpace(os.Getenv("AC_SESSION_SECRET"))
	if secret == "" && credFile != "" {
		if fi, err := os.Lstat(credFile); err == nil && fi.Mode().IsRegular() {
			c, err := loadCred(credFile)
			if err != nil {
				return err
			}
			secret = c.SessionSecret
			if req.SessionID == "" {
				req.SessionID = c.SessionID
			}
		}
	}
	req.Token = token
	req.SessionSecret = secret
	return nil
}

func loadCred(path string) (sessionCred, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sessionCred{}, err
	}
	raw := bytes.TrimSpace(b)
	var c sessionCred
	if json.Unmarshal(raw, &c) == nil && (c.SessionSecret != "" || c.SessionID != "") {
		return c, nil
	}
	return sessionCred{SessionSecret: string(raw)}, nil // legacy secret-only files
}

func saveCred(path string, c sessionCred) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("cred file must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ac-cred-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	enc, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func callerSession() (string, error) {
	id := sessionIDFromEnv()
	if id == "" {
		return "", fmt.Errorf("no session id: run agent-coordinator join or set CLAUDE_CODE_SESSION_ID, GROK_SESSION_ID, CODEX_SESSION_ID, or AC_SESSION_ID")
	}
	return id, nil
}

func once(addr string, req protocol.Request) (protocol.Response, error) {
	if addr == "" {
		addr = coordinatorAddr()
	}
	conn, err := dialCoordinator(addr, time.Second)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := json.Marshal(req)
	if err != nil {
		return protocol.Response{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return protocol.Response{}, fmt.Errorf("write: %w", err)
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return protocol.Response{}, fmt.Errorf("decode: %w", err)
	}
	if !resp.OK {
		errMsg := resp.Error
		if errMsg == "" {
			errMsg = "request failed"
		}
		return resp, fmt.Errorf("%s", errMsg)
	}
	return resp, nil
}
