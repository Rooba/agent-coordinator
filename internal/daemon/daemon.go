package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
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

// fromOps are the ops that act AS an agent. Their from is resolved once, up
// front, instead of being trusted case by case.
var fromOps = map[string]bool{
	protocol.OpSend: true, protocol.OpBroadcast: true, protocol.OpRead: true,
	protocol.OpPeek: true, protocol.OpClaim: true, protocol.OpRelease: true,
	protocol.OpHistory: true, protocol.OpSendWorkspace: true,
	protocol.OpRequestEyes: true, protocol.OpCancelEyes: true,
}

// relayReady is a test seam: a test binds 127.0.0.1:0 and needs the port the
// OS picked. Production ignores it.
var relayReady = func(string) {}

// relayListener binds the opt-in loopback TCP relay. Anything that stops it -
// no AC_RELAY_LISTEN, a non-loopback address, a taken port, an unreadable
// token - logs and returns nil: a missing relay must never take down local
// coordination.
func relayListener(st *store.Store) (net.Listener, *relayGate) {
	addr, err := paths.RelayListen()
	if err != nil {
		relayLog("unavailable: %v", err)
		return nil, nil
	}
	if addr == "" {
		return nil, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		relayLog("unavailable: listen %s: %v", addr, err)
		return nil, nil
	}
	// The token is resolved once the relay is really up, so a daemon that
	// never listens leaves no secret lying around.
	gate := &relayGate{st: st, insecure: paths.RelayInsecure()}
	if gate.insecure {
		relayLog("WARNING AC_RELAY_INSECURE is set - the shared token is NOT checked")
	} else if gate.token, err = paths.RelayToken(); err != nil {
		relayLog("unavailable: token: %v", err)
		l.Close()
		return nil, nil
	}
	relayLog("listening on %s", l.Addr())
	relayReady(l.Addr().String())
	return l, gate
}

// accept runs one listener's loop until it closes, handing each connection
// its own goroutine. gate is nil for the unix listener: only relay
// connections are authenticated.
func accept(l net.Listener, st *store.Store, gate *relayGate, lastActivity *atomic.Int64, wg *sync.WaitGroup) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		lastActivity.Store(time.Now().UnixNano())
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(conn, st, gate)
		}()
	}
}

func Serve(l net.Listener, st *store.Store, idleTimeout time.Duration) error {
	st.Housekeep()
	relay, gate := relayListener(st)

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	done := make(chan struct{})         // closed when the accept loops end
	watchdogDone := make(chan struct{}) // closed when the watchdog goroutine exits
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)
	closeListeners := func() {
		l.Close()
		if relay != nil {
			relay.Close()
		}
	}

	go func() {
		defer close(watchdogDone)
		tick := time.NewTicker(idleTimeout / 4)
		house := time.NewTicker(time.Hour)
		defer tick.Stop()
		defer house.Stop()
		for {
			select {
			case <-tick.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > idleTimeout {
					closeListeners()
					return
				}
			case <-house.C:
				st.Housekeep()
			case <-sigC:
				closeListeners()
				return
			case <-done:
				return
			}
		}
	}()

	// Closing the listeners - idle timeout, signal, or an external shutdown
	// (tests, socket handover) - is the one clean way to end the accept loops.
	// The relay loop holds a wait-group token of its own, so its
	// per-connection Add can never race the Wait below.
	var wg sync.WaitGroup
	if relay != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := accept(relay, st, gate, &lastActivity, &wg); err != nil {
				relayLog("accept: %v", err)
			}
		}()
	}
	serveErr := accept(l, st, nil, &lastActivity, &wg)
	// Deterministic teardown: join the watchdog, drain in-flight handlers,
	// then close the store, so no goroutine or handle outlives Serve.
	close(done)
	signal.Stop(sigC)
	closeListeners()
	<-watchdogDone
	wg.Wait()
	st.Close()
	return serveErr
}

// handle answers one framed request. The gate is nil on the unix socket,
// where the socket directory's permissions are the trust boundary; on the
// relay it authenticates the caller and rewrites the request before dispatch
// ever sees it.
func handle(conn net.Conn, st *store.Store, gate *relayGate) {
	defer func() { _ = recover() }() // a panicking handler must not kill the daemon
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, 1<<20)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req protocol.Request
	var resp protocol.Response
	if bad := json.Unmarshal(line, &req); bad != nil {
		resp = protocol.Response{Error: "bad request: " + bad.Error()}
	} else {
		// Kind and origin are provenance the daemon stamps, never the client's
		// word: whatever the frame carried is dropped here, and only the gate
		// stamps a caller it has authenticated. The kind a relay register asks
		// to be is a claim, so it travels to the gate as one.
		claimedKind := req.Kind
		req.Origin, req.Kind = "", ""
		gated, final := gate.check(&req, claimedKind)
		resp = gated
		if !final {
			resp = dispatch(st, req)
			// A relay answer says only the documented strings, whatever op
			// produced it; the unix socket keeps the store's own text.
			if gate != nil && resp.Error != "" {
				resp.Error = relayErrorText(resp.Error)
			}
		}
	}
	out, _ := json.Marshal(resp)
	conn.Write(append(out, '\n'))
}

// fail is the one place a failure becomes a response, so every op answers an
// error the same way: the store's own text. That is the unix answer, where
// the socket's permissions are the trust boundary; anything reachable over
// the relay is rewritten by relayErrorText on the way out, so only the
// documented strings leave the daemon.
func fail(err error) protocol.Response { return protocol.Response{Error: err.Error()} }

func dispatch(st *store.Store, req protocol.Request) protocol.Response {
	// A relay-owned row is usable only through the gate that proved its
	// per-session secret, whatever the op: the unix socket may know a broker's
	// exact scope and session, but that is not authority to act as it, refresh
	// it or retire it. The caller's row is resolved here, before anything acts
	// on it. A session with no row yet is a register joining, and every other
	// op still answers for itself below.
	if req.SessionID != "" && req.Origin != relayOrigin {
		if caller, err := st.Identity(req.Scope, req.SessionID); err == nil && caller.Origin == store.RelayOrigin {
			return fail(store.ErrRelayAuth)
		}
	}
	// Requests carrying the subagent AgentID target the CHILD row derived from
	// the parent SessionID: register/refresh it up front and retarget the op.
	childSession := ""
	if req.AgentID != "" {
		switch req.Op {
		case protocol.OpRegister, protocol.OpEvent, protocol.OpDeregister:
			childSession = store.ChildSessionID(req.SessionID, req.AgentID)
			if req.Op != protocol.OpDeregister {
				name, err := st.RegisterChild(req.Scope, req.SessionID, req.AgentID, req.AgentType)
				if err != nil {
					return fail(err)
				}
				if req.Op == protocol.OpRegister {
					return protocol.Response{OK: true, Name: name}
				}
			}
		}
	}
	// One identity for the whole request: an explicit from must belong to the
	// calling session, so the cases below can trust req.From - and the ops that
	// stamp a return address use this ref, never a client's.
	var actor protocol.AgentRef
	var actorIsBroker bool
	if fromOps[req.Op] {
		id, err := st.ResolveActor(req.Scope, req.SessionID, req.From)
		if err != nil {
			return fail(err)
		}
		// A from may name a row other than the caller's own, so the same relay
		// rule applies again here: knowing a relay row's name is not authority
		// to act as it.
		if id.Origin == store.RelayOrigin && req.Origin != relayOrigin {
			return fail(store.ErrRelayAuth)
		}
		req.From = id.Name
		actor = protocol.AgentRef{Name: id.Name, AgentID: id.AgentID, Scope: req.Scope}
		// A broker's poll takes its launches and cancels from the ledger
		// instead of its inbox. That is decided by the authenticated row and
		// daemon-stamped provenance, never by the client's wire-level kind.
		actorIsBroker = req.Origin == relayOrigin && id.Origin == store.RelayOrigin &&
			id.Kind == protocol.KindLauncher
	}
	// Any identified call is a heartbeat that keeps the row fresh and lifts
	// sticky idle. The exceptions set their own freshness (register, event,
	// idle, deregister) or must not resurrect a retired row (whoami).
	if req.SessionID != "" {
		switch req.Op {
		case protocol.OpRegister, protocol.OpDeregister, protocol.OpIdle, protocol.OpEvent, protocol.OpWhoami:
		default:
			if err := st.Touch(req.Scope, req.SessionID); err != nil {
				return fail(err)
			}
		}
	}
	// A task.* body is the eyes lifecycle reporting in, not mail: the store
	// moves the task and delivers the requester's copy in one transaction, so a
	// state change never exists without the message that announces it.
	if resp, isReport := taskReport(st, req, actor); isReport {
		return resp
	}
	switch req.Op {
	case protocol.OpRegister:
		register := st.Register
		if req.OnlyIfNoHook {
			register = st.RegisterIfNoLiveHook
		}
		name, err := register(req.Scope, req.SessionID, req.Source)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Name: name}
	case protocol.OpWhoami:
		id, err := st.Identity(req.Scope, req.SessionID)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Name: id.Name, AgentID: id.AgentID, Source: id.Source, Parent: id.Parent}
	case protocol.OpDeregister:
		sess := req.SessionID
		if childSession != "" {
			sess = childSession // SubagentStop retires the child, never the parent
		}
		if err := st.SetStatus(req.Scope, sess, "gone"); err != nil {
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
		sess := req.SessionID
		if childSession != "" {
			sess = childSession // subagent activity lands on the child row
		}
		notices, err := st.RecordEvent(req.Scope, sess, req)
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
		agents, err := st.Board(req.Scope, req.IncludeGone)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Agents: agents}
	case protocol.OpSend:
		if err := st.Send(req.Scope, req.From, req.To, req.Body); err != nil {
			return fail(err)
		}
	case protocol.OpRead:
		// The raw ledger rows are consumed by a broker's poll without being
		// handed back; what it still owes comes from the queue instead.
		read := st.Read
		if actorIsBroker {
			read = st.ReadBroker
		}
		msgs, err := read(req.Scope, req.From)
		if err != nil {
			return fail(err)
		}
		return withPending(st, req, actorIsBroker, protocol.Response{OK: true, Messages: msgs})
	case protocol.OpPeek:
		peek := st.PeekMail
		if actorIsBroker {
			peek = st.PeekBrokerMail
		}
		info, err := peek(req.Scope, req.From, req.AfterID)
		if err != nil {
			return fail(err)
		}
		return withPending(st, req, actorIsBroker, protocol.Response{OK: true, Unread: info.Unread,
			HighWater: info.HighWater, PeekIDs: info.IDs, PeekFroms: info.Froms})
	case protocol.OpBroadcast:
		if err := st.Broadcast(req.Scope, req.From, req.Body); err != nil {
			return fail(err)
		}
	case protocol.OpClaim:
		res, err := st.Claim(req.Scope, req.From, req.Path, req.Note)
		if err != nil {
			return fail(err)
		}
		resp := protocol.Response{OK: true}
		if res.Stolen {
			resp.Notices = []string{fmt.Sprintf("takeover: previous holder %s was gone", res.PrevName)}
		}
		return resp
	case protocol.OpRelease:
		if err := st.Release(req.Scope, req.From, req.Path); err != nil {
			return fail(err)
		}
	case protocol.OpClaims:
		claims, err := st.ListClaims(req.Scope)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Claims: claims}
	case protocol.OpHistory:
		hist, err := st.MessageHistory(req.Scope, req.From, req.Peer, req.Limit)
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, History: hist}
	case protocol.OpListWorkspaces:
		ws, err := st.ListWorkspaces()
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Workspaces: ws}
	case protocol.OpListEyes:
		eyes, err := st.ListEyes()
		if err != nil {
			return fail(err)
		}
		return protocol.Response{OK: true, Agents: eyes}
	case protocol.OpSendWorkspace:
		// The sender and the return address are the actor resolved above, so a
		// reply can never be redirected by whatever the caller put in reply_to.
		d := store.Delivery{FromScope: req.Scope, FromName: actor.Name, Body: req.Body, ReplyTo: &actor}
		if t := req.Target; t != nil {
			d.ToScope, d.ToName = t.Scope, t.Name
			if t.AgentID != "" {
				d.ToName = t.AgentID // an id beats a name: names are per-scope labels
			}
		}
		if err := st.SendToScope(d); err != nil {
			return fail(err)
		}
	case protocol.OpRequestEyes:
		return requestEyes(st, req, actor)
	case protocol.OpCancelEyes:
		return cancelEyes(st, req, actor)
	default:
		return fail(errors.New("unknown op " + req.Op))
	}
	return protocol.Response{OK: true}
}
