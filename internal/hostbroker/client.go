package hostbroker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

const (
	DefaultAddr      = "127.0.0.1:7400"
	defaultWireLimit = 2 << 20
	requestLimit     = 256 << 10
	callTimeout      = 6 * time.Second
)

type Session struct {
	Token  string
	ID     string
	Secret string
}

type Relay interface {
	Call(context.Context, Session, protocol.Request) (protocol.Response, error)
}

type RelayError struct{ message string }

func (e *RelayError) Error() string { return e.message }

type Client struct {
	addr  string
	limit int64
}

func NewClient(addr string) (*Client, error) {
	addr = strings.TrimPrefix(strings.TrimSpace(addr), "tcp://")
	if addr == "" {
		addr = DefaultAddr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid relay address: %w", err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("relay address must be loopback")
	}
	return &Client{addr: addr, limit: defaultWireLimit}, nil
}

func (c *Client) Call(ctx context.Context, session Session, req protocol.Request) (protocol.Response, error) {
	if err := validateToken(session.Token); err != nil {
		return protocol.Response{}, err
	}
	req.Token, req.SessionID, req.SessionSecret = session.Token, session.ID, session.Secret
	frame, err := json.Marshal(req)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("encode relay request: %w", err)
	}
	if len(frame) > requestLimit {
		return protocol.Response{}, errors.New("relay request exceeds size limit")
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(callCtx, "tcp", c.addr)
	if err != nil {
		return protocol.Response{}, callError(callCtx, "relay unavailable", err)
	}
	defer conn.Close()
	deadline, _ := callCtx.Deadline()
	_ = conn.SetDeadline(deadline)
	stopCancel := context.AfterFunc(callCtx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		return protocol.Response{}, callError(callCtx, "relay write", err)
	}

	reader := bufio.NewReader(io.LimitReader(conn, c.limit+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return protocol.Response{}, callError(callCtx, "relay read", err)
	}
	if len(line) == 0 || int64(len(line)) > c.limit {
		return protocol.Response{}, errors.New("relay response is empty or exceeds size limit")
	}
	var response protocol.Response
	decoder := json.NewDecoder(bytes.NewReader(line))
	if err := decoder.Decode(&response); err != nil {
		return protocol.Response{}, fmt.Errorf("decode relay response: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return protocol.Response{}, errors.New("relay response contains trailing data")
	}
	if !response.OK {
		message := strings.TrimSpace(response.Error)
		if message == "" {
			message = "request failed"
		}
		return response, &RelayError{message: redactCredentials(message, session)}
	}
	return response, nil
}

func callError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func retryable(err error) bool {
	var rejection *RelayError
	return !errors.As(err, &rejection)
}

func redactCredentials(message string, session Session) string {
	for _, secret := range []string{session.Token, session.Secret} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}
