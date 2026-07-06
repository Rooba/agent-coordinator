package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agent-coordinator/go/internal/paths"
	"github.com/agent-coordinator/go/internal/protocol"
	"github.com/agent-coordinator/go/internal/store"
)

// Listener returns the systemd-activated socket (LISTEN_FDS=1, fd 3) or
// creates the fallback unix socket for dev runs.
func Listener() (net.Listener, bool, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		l, err := net.FileListener(f)
		f.Close()
		return l, true, err
	}
	sock := paths.Socket()
	os.Remove(sock) // stale socket from an unclean dev shutdown
	l, err := net.Listen("unix", sock)
	return l, false, err
}

func Serve(l net.Listener, st *store.Store, idleTimeout time.Duration) error {
	st.Housekeep()

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		tick := time.NewTicker(idleTimeout / 4)
		house := time.NewTicker(time.Hour)
		defer tick.Stop()
		defer house.Stop()
		for {
			select {
			case <-tick.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > idleTimeout {
					close(stop)
					l.Close()
					return
				}
			case <-house.C:
				st.Housekeep()
			case <-sigC:
				close(stop)
				l.Close()
				return
			}
		}
	}()

	// Drain on shutdown: whatever ends the accept loop, close the listener,
	// wait for in-flight handlers, and only then close the store.
	var wg sync.WaitGroup
	var serveErr error
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-stop: // clean idle-exit or signal
			default:
				serveErr = err
			}
			break
		}
		lastActivity.Store(time.Now().UnixNano())
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(conn, st)
		}()
	}
	l.Close()
	wg.Wait()
	st.Close()
	return serveErr
}

func handle(conn net.Conn, st *store.Store) {
	defer func() { _ = recover() }() // a panicking handler must not kill the daemon
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, 1<<20)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req protocol.Request
	var resp protocol.Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp = protocol.Response{Error: "bad request: " + err.Error()}
	} else {
		resp = dispatch(st, req)
	}
	out, _ := json.Marshal(resp)
	conn.Write(append(out, '\n'))
}

func dispatch(st *store.Store, req protocol.Request) protocol.Response {
	fail := func(err error) protocol.Response { return protocol.Response{Error: err.Error()} }
	switch req.Op {
	case protocol.OpRegister:
		name, err := st.Register(req.Scope, req.SessionID, req.Source)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Name: name}
	case protocol.OpDeregister:
		if err := st.SetStatus(req.Scope, req.SessionID, "gone"); err != nil {
			return fail(err)
		}
	case protocol.OpIdle:
		if err := st.SetStatus(req.Scope, req.SessionID, "idle"); err != nil {
			return fail(err)
		}
		// Drain pending notices at turn end so the Stop hook can nudge the
		// model. notice_sent_at makes this once-only: a repeat idle with
		// unread-but-noticed mail returns nothing (no Stop loop).
		notices, err := st.PendingNotices(req.Scope, req.SessionID)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Notices: notices}
	case protocol.OpEvent:
		notices, err := st.RecordEvent(req.Scope, req.SessionID, req)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Notices: notices}
	case protocol.OpAgents:
		agents, err := st.Agents(req.Scope)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Agents: agents}
	case protocol.OpBoard:
		agents, err := st.Board(req.Scope)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Agents: agents}
	case protocol.OpSend:
		if err := st.Send(req.Scope, req.From, req.To, req.Body); err != nil {
			return fail(err)
		}
	case protocol.OpRead:
		msgs, err := st.Read(req.Scope, req.From)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Messages: msgs}
	case protocol.OpPeek:
		n, err := st.UnreadCount(req.Scope, req.From)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Unread: n}
	case protocol.OpBroadcast:
		if err := st.Broadcast(req.Scope, req.From, req.Body); err != nil {
			return fail(err)
		}
	default:
		return fail(errors.New("unknown op " + req.Op))
	}
	return protocol.Response{OK: true}
}
