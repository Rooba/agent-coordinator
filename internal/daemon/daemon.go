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

// ErrAlreadyServing means another daemon already owns the socket. The caller
// should exit 0 quietly: spawn-on-miss makes racing daemons routine, and the
// losers must self-resolve harmlessly.
var ErrAlreadyServing = errors.New("another daemon is already serving")

// sockLock pins the daemon's socket lock file open for the process lifetime.
// The OS lock is released only at process exit - strictly after Serve has
// closed (and thereby unlinked) the listener - so there is no window in which
// a successor binds the path and this process then unlinks the live socket.
var sockLock *os.File

// Listener returns the systemd-activated socket (LISTEN_FDS=1, fd 3) or
// binds the unix socket itself. An OS file lock on sock+".lock" (flock /
// LockFileEx, held until the daemon exits) serializes check-remove-bind, so
// racing spawns can never unlink each other's live socket: the lock loser,
// and any winner that still finds a dialable peer (e.g. systemd-activated),
// gets ErrAlreadyServing. Only a dead socket file is removed before binding.
func Listener() (net.Listener, bool, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		l, err := net.FileListener(f)
		f.Close()
		return l, true, err
	}
	sock := paths.Socket()
	lock, err := os.OpenFile(sock+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err // unusable socket dir: surface, do not swallow
	}
	if held, err := tryLock(lock); err != nil || !held {
		lock.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, ErrAlreadyServing // a peer owns the lock (serving or about to)
	}
	if dialable(sock) { // live peer that never took the lock (systemd-activated)
		lock.Close()
		return nil, false, ErrAlreadyServing
	}
	os.Remove(sock) // dead socket from an unclean shutdown; safe under the lock
	l, err := net.Listen("unix", sock)
	if err != nil {
		lock.Close()
		return nil, false, err
	}
	sockLock = lock // hold the lock for the daemon's lifetime
	return l, false, nil
}

func dialable(sock string) bool {
	conn, err := net.DialTimeout("unix", sock, 250*time.Millisecond)
	if err == nil {
		conn.Close()
	}
	return err == nil
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
