package hostbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

func TestClientSendsOneAuthenticatedFrameAndRedactsErrors(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seen := make(chan protocol.Request, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		var request protocol.Request
		_ = json.Unmarshal(line, &request)
		seen <- request
		response, _ := json.Marshal(protocol.Response{Error: "rejected " + testToken + " child-secret"})
		_, _ = conn.Write(append(response, '\n'))
	}()
	client, err := NewClient(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	session := Session{Token: testToken, ID: "launcher", Secret: "child-secret"}
	_, err = client.Call(context.Background(), session, protocol.Request{Op: protocol.OpRead, Scope: "host:test"})
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), session.Secret) {
		t.Fatalf("unsafe relay error = %v", err)
	}
	request := <-seen
	if request.Token != testToken || request.SessionID != session.ID || request.SessionSecret != session.Secret || request.Op != protocol.OpRead {
		t.Fatalf("wire request = %+v", request)
	}
}

func TestClientCancellationInterruptsStalledRead(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestRead := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		close(requestRead)
		_, _ = io.Copy(io.Discard, conn)
	}()
	client, _ := NewClient(listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-requestRead
		cancel()
	}()
	started := time.Now()
	_, err = client.Call(ctx, Session{Token: testToken}, protocol.Request{Op: protocol.OpRead})
	if !errors.Is(err, context.Canceled) || time.Since(started) > time.Second {
		t.Fatalf("stalled call cancellation = %v after %s", err, time.Since(started))
	}
}

func TestClientRejectsUnsafeAddressTokenAndOversizedResponse(t *testing.T) {
	if _, err := NewClient("192.0.2.10:7400"); err == nil {
		t.Fatal("non-loopback relay address accepted")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write(append([]byte(`{"ok":true,"error":"`), append([]byte(strings.Repeat("x", defaultWireLimit)), []byte(`"}\n`)...)...))
	}()
	client, _ := NewClient(listener.Addr().String())
	if _, err := client.Call(context.Background(), Session{Token: "short"}, protocol.Request{Op: protocol.OpRead}); err == nil {
		t.Fatal("short token accepted")
	}
	if _, err := client.Call(context.Background(), Session{Token: testToken}, protocol.Request{Op: protocol.OpRead}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v", err)
	}
}
