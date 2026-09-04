package store

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// columns lists a table's column names, sorted.
func columns(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func hasColumn(cols []string, want string) bool {
	i := sort.SearchStrings(cols, want)
	return i < len(cols) && cols[i] == want
}

// count is the one-number probe the store's own tables answer with.
func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedTask writes an eyes_tasks row directly: the busy and staleness rules
// are about the row, not about how it was created.
func seedTask(t *testing.T, s *Store, taskID, launcherSession, state string, updatedAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO eyes_tasks
		(task_id, requester_scope, requester_agent_id, launcher_session, runtime, state, created_at, updated_at)
		VALUES (?,?,?,?,'claude',?,?,?)`, taskID, "/r", "aid-a", launcherSession, state, updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
}

// registerBroker registers a host broker the way the relay gate will.
func registerBroker(t *testing.T, s *Store, scope, session string) RelayResult {
	t.Helper()
	r, err := s.RegisterRelay(RelayRegistration{Scope: scope, SessionID: session,
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A database written before the relay columns existed must migrate in place:
// old rows survive on the new defaults and every column is present.
func TestRelayMigrationsOnLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE agents (
		scope TEXT NOT NULL, session_id TEXT NOT NULL,
		agent_id TEXT NOT NULL, name TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
		PRIMARY KEY (scope, session_id));
		INSERT INTO agents VALUES ('/r','s-old','aid-old','old-agent','active',1,1);
		CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL, from_agent TEXT NOT NULL, to_agent TEXT,
		body TEXT NOT NULL, created_at INTEGER NOT NULL);
		INSERT INTO messages (scope, from_agent, to_agent, body, created_at)
		VALUES ('/r','aid-old',NULL,'legacy mail',1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open must migrate a legacy database: %v", err)
	}
	defer s.Close()
	agentCols := columns(t, s, "agents")
	for _, col := range []string{"kind", "origin", "platform", "caps", "relay_secret_hash"} {
		if !hasColumn(agentCols, col) {
			t.Errorf("agents is missing column %q", col)
		}
	}
	msgCols := columns(t, s, "messages")
	for _, col := range []string{"from_scope", "reply_to", "task_id", "kind"} {
		if !hasColumn(msgCols, col) {
			t.Errorf("messages is missing column %q", col)
		}
	}
	var kind, caps string
	if err := s.db.QueryRow(`SELECT kind, caps FROM agents WHERE session_id='s-old'`).Scan(&kind, &caps); err != nil {
		t.Fatalf("legacy row must survive the migration: %v", err)
	}
	if kind != "" || caps != "[]" {
		t.Fatalf("legacy row defaults: kind=%q caps=%q, want \"\" and []", kind, caps)
	}
	var body, fromScope, replyTo, taskID, mkind string
	if err := s.db.QueryRow(`SELECT body, from_scope, reply_to, task_id, kind FROM messages WHERE from_agent='aid-old'`).
		Scan(&body, &fromScope, &replyTo, &taskID, &mkind); err != nil {
		t.Fatalf("legacy message must survive the migration: %v", err)
	}
	if body != "legacy mail" || fromScope != "" || replyTo != "" || taskID != "" || mkind != "" {
		t.Fatalf("legacy message defaults: body=%q from_scope=%q reply_to=%q task_id=%q kind=%q",
			body, fromScope, replyTo, taskID, mkind)
	}
}

// Reopening a migrated database must not error, and eyes_tasks must accept a
// row with only its required columns set.
func TestMigrationsIdempotentAndEyesTasksTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second open must tolerate already-applied migrations: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO eyes_tasks (task_id, requester_scope, requester_agent_id, launcher_session, created_at, updated_at)
		VALUES ('task-abc','/r','aid-a','broker-1',10,10)`); err != nil {
		t.Fatalf("eyes_tasks insert: %v", err)
	}
	var state, runtime string
	if err := s.db.QueryRow(`SELECT state, runtime FROM eyes_tasks WHERE task_id='task-abc'`).Scan(&state, &runtime); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || runtime != "" {
		t.Fatalf("eyes_tasks defaults: state=%q runtime=%q, want queued and \"\"", state, runtime)
	}
}

// The launcher busy check runs on every pick, so eyes_tasks carries the index
// that backs it.
func TestEyesTasksLauncherStateIndex(t *testing.T) {
	s := open(t)
	var name string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='eyes_tasks'
		AND name='idx_eyes_tasks_launcher_state'`).Scan(&name); err != nil {
		t.Fatalf("eyes_tasks(launcher_session, state) index: %v", err)
	}
}

// The workspace directory counts live agents by kind and hides the reserved
// host: scopes - those are broker plumbing, not workspaces.
func TestListWorkspacesCountsByKindAndHidesHostScopes(t *testing.T) {
	s := open(t)
	s.Register("/repo-a", "s-a1", "hook")
	s.Register("/repo-a", "s-a2", "hook")
	s.Register("/repo-b", "s-b1", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/repo-a", SessionID: "eyes-task-1",
		Kind: protocol.KindEyes, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	ws, err := s.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]protocol.WorkspaceInfo{}
	for _, w := range ws {
		got[w.Scope] = w
	}
	if _, ok := got["host:BOX"]; ok {
		t.Fatalf("host: scopes must not appear as workspaces: %+v", ws)
	}
	if a := got["/repo-a"]; a.LiveAgents != 3 || a.EyesAgents != 1 || a.LauncherAgents != 0 {
		t.Fatalf("/repo-a counts: %+v", a)
	}
	if b := got["/repo-b"]; b.LiveAgents != 1 || b.EyesAgents != 0 {
		t.Fatalf("/repo-b counts: %+v", b)
	}
}

// list_eyes answers "is a broker up?" and "is an eyes child running?" in one
// call, so it crosses scopes and carries kind, scope, platform and caps.
func TestListEyesCrossesScopes(t *testing.T) {
	s := open(t)
	s.Register("/repo-a", "s-a1", "hook")
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Capabilities: []string{"browser.chrome"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/repo-a", SessionID: "eyes-task-1",
		Kind: protocol.KindEyes, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	eyes, err := s.ListEyes()
	if err != nil || len(eyes) != 2 {
		t.Fatalf("want the launcher and the child, got %d (%v)", len(eyes), err)
	}
	byKind := map[string]protocol.AgentInfo{}
	for _, a := range eyes {
		byKind[a.Kind] = a
	}
	l := byKind[protocol.KindLauncher]
	if l.Scope != "host:BOX" || l.Platform != "windows" || len(l.Capabilities) != 1 || l.Capabilities[0] != "browser.chrome" {
		t.Fatalf("launcher row: %+v", l)
	}
	if c := byKind[protocol.KindEyes]; c.Scope != "/repo-a" || c.Origin != "relay" {
		t.Fatalf("eyes row: %+v", c)
	}
}

// A cross-scope send lands in the RECIPIENT's scope so every scope-filtered
// read keeps working, carries from_scope only when the scopes differ, and
// stamps the sender's kind and the daemon's reply_to.
func TestSendToScopeWritesIntoTargetScope(t *testing.T) {
	s := open(t)
	nA, _ := s.Register("/repo-a", "s-a", "hook")
	nB, _ := s.Register("/repo-b", "s-b", "hook")
	from := protocol.AgentRef{Name: nA, AgentID: agentID("s-a"), Scope: "/repo-a"}
	if err := s.SendToScope(Delivery{FromScope: "/repo-a", FromName: nA, ToScope: "/repo-b",
		ToName: nB, Body: "hello over there", ReplyTo: &from}); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.Read("/repo-a", nA); len(msgs) != 0 {
		t.Fatalf("sender's own scope must not see the message: %+v", msgs)
	}
	msgs, err := s.Read("/repo-b", nB)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("recipient read: %d (%v)", len(msgs), err)
	}
	m := msgs[0]
	if m.From != nA || m.FromScope != "/repo-a" || m.ReplyTo == nil || m.ReplyTo.AgentID != from.AgentID {
		t.Fatalf("cross-scope message: %+v (reply_to %+v)", m, m.ReplyTo)
	}
	// Same-scope mail keeps today's shape: a bare name and no from_scope.
	if err := s.SendToScope(Delivery{FromScope: "/repo-b", FromName: nB, ToName: nB, Body: "note to self"}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.Read("/repo-b", nB)
	if len(msgs) != 1 || msgs[0].FromScope != "" {
		t.Fatalf("in-scope message must carry no from_scope: %+v", msgs)
	}
}

// A message and its deliveries are one write: a delivery that cannot be
// inserted must not leave a message row nobody can ever read.
func TestSendToScopeIsOneTransaction(t *testing.T) {
	s := open(t)
	nA, _ := s.Register("/r", "s-a", "hook")
	nB, _ := s.Register("/r", "s-b", "hook")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_delivery BEFORE INSERT ON deliveries
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SendToScope(Delivery{FromScope: "/r", FromName: nA, ToName: nB, Body: "lost"}); err == nil {
		t.Fatal("a failing delivery insert must fail the send")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("an undeliverable message must be rolled back, got %d rows", n)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_delivery`); err != nil {
		t.Fatal(err)
	}
	if err := s.SendToScope(Delivery{FromScope: "/r", FromName: nA, ToName: nB, Body: "delivered"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("/r", nB); n != 1 {
		t.Fatalf("unread after the retry: %d, want 1", n)
	}
}

// An eyes child's mail is labelled with its kind and task so the requester
// can tell a report from a peer's DM.
func TestSendStampsSenderKindAndTaskID(t *testing.T) {
	s := open(t)
	nA, _ := s.Register("/r", "s-a", "hook")
	child, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "eyes-task-9",
		Kind: protocol.KindEyes, Origin: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SendToScope(Delivery{FromScope: "/r", FromName: child.Name, ToName: nA,
		Body: `{"type":"task.result"}`, TaskID: "task-9"}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Read("/r", nA)
	if len(msgs) != 1 || msgs[0].Kind != protocol.KindEyes || msgs[0].TaskID != "task-9" {
		t.Fatalf("want one eyes message for task-9, got %+v", msgs)
	}
}

// A launcher that stopped polling cannot pick up a task: the window is 30s,
// not the 2 minute presence window.
func TestPickLauncherRequiresRecentPoll(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	registerBroker(t, s, "host:BOX", "broker-1")
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" || l.Scope != "host:BOX" {
		t.Fatalf("fresh launcher: %+v (%v)", l, err)
	}
	now = now.Add(45 * time.Second) // still "active" for presence, but no longer polling
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("stale launcher must not be picked, got %v", err)
	}
}

// Only a relay-registered launcher in a reserved host: scope is a broker.
// AC_KIND is plumbed from local callers, so a local agent calling itself a
// launcher must never absorb a brief.
func TestPickLauncherRequiresRelayOriginAndHostScope(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "local-1",
		Kind: protocol.KindLauncher}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("a local kind=launcher row must not be pickable, got %v", err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "relay-1",
		Kind: protocol.KindLauncher, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("a launcher outside a host: scope must not be pickable, got %v", err)
	}
	registerBroker(t, s, "host:BOX", "broker-1")
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("the host broker: %+v (%v)", l, err)
	}
}

// A launcher running a task is busy: the pick skips it, and when every live
// launcher is busy the caller gets ErrEyesBusy rather than "no host launcher".
func TestPickLauncherSkipsBusyLauncher(t *testing.T) {
	s := open(t)
	now := time.Unix(3000000, 0)
	s.Now = func() time.Time { return now }
	registerBroker(t, s, "host:BOX", "broker-1")
	now = now.Add(time.Second) // broker-2 is the more recently seen launcher
	registerBroker(t, s, "host:BOX", "broker-2")
	seedTask(t, s, "task-1", "broker-2", "queued", now.Unix())
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" {
		t.Fatalf("a busy launcher must be skipped: %+v (%v)", l, err)
	}
	seedTask(t, s, "task-2", "broker-1", "accepted", now.Unix())
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("every launcher busy must be ErrEyesBusy, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET state='done' WHERE task_id='task-1'`); err != nil {
		t.Fatal(err)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-2" {
		t.Fatalf("a finished task frees its launcher: %+v (%v)", l, err)
	}
}

// A task nobody has advanced for half an hour is abandoned, not in flight: it
// must not pin its launcher out of service for the 24h ledger retention.
func TestPickLauncherIgnoresStaleTasks(t *testing.T) {
	s := open(t)
	now := time.Unix(4000000, 0)
	s.Now = func() time.Time { return now }
	registerBroker(t, s, "host:BOX", "broker-1")
	seedTask(t, s, "task-fresh", "broker-1", "accepted", now.Unix()-1799)
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("a task touched within 1800s still holds its launcher, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET updated_at=? WHERE task_id='task-fresh'`, now.Unix()-1801); err != nil {
		t.Fatal(err)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("a stale task must not block the launcher: %+v (%v)", l, err)
	}
}

// The secret is minted once, only for relay-origin rows, and only its sha256
// is stored. Verification is by secret, not by session id alone.
func TestRegisterRelayMintsSecretOnceAndBindsIt(t *testing.T) {
	s := open(t)
	first, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay"})
	if err != nil || len(first.Secret) != 64 {
		t.Fatalf("first register must mint 32 hex-encoded bytes: %+v (%v)", first, err)
	}
	again, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: first.Secret})
	if err != nil || again.Name != first.Name || again.Secret != "" {
		t.Fatalf("re-register must be idempotent and mint nothing: %+v (%v)", again, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT relay_secret_hash FROM agents WHERE session_id='broker-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == first.Secret || len(stored) != 64 {
		t.Fatalf("the store must keep only the hash, got %q", stored)
	}
	id, err := s.VerifyRelaySecret("broker-1", first.Secret)
	if err != nil || id.Name != first.Name || id.Scope != "host:BOX" || id.Kind != protocol.KindLauncher {
		t.Fatalf("verify: %+v (%v)", id, err)
	}
	if _, err := s.VerifyRelaySecret("broker-1", "wrong"); !errors.Is(err, ErrRelayAuth) {
		t.Fatalf("wrong secret must be ErrRelayAuth, got %v", err)
	}
	s.Register("/r", "hook-session", "hook")
	if _, err := s.VerifyRelaySecret("hook-session", ""); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a hook session must be ErrForeignSession, got %v", err)
	}
	if _, err := s.VerifyRelaySecret("nobody", ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an unknown session must be ErrNoSession, got %v", err)
	}
}

// A launcher keeps one session across reconnects, so re-registering it means
// proving the minted secret; a session created locally is never claimable.
func TestRegisterRelayReRegisterNeedsTheSecret(t *testing.T) {
	s := open(t)
	first, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "wrong"} {
		if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
			Kind: protocol.KindLauncher, Origin: "relay", Secret: bad}); !errors.Is(err, ErrRelayAuth) {
			t.Fatalf("re-register with secret %q must be ErrRelayAuth, got %v", bad, err)
		}
	}
	again, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: first.Secret,
		Platform: "windows-11", Capabilities: []string{"browser.chrome"}})
	if err != nil || again.Name != first.Name || again.Secret != "" {
		t.Fatalf("re-register with the secret: %+v (%v)", again, err)
	}
	eyes, err := s.ListEyes()
	if err != nil || len(eyes) != 1 || eyes[0].Platform != "windows-11" || len(eyes[0].Capabilities) != 1 {
		t.Fatalf("a re-register must refresh platform and caps: %+v (%v)", eyes, err)
	}
	s.Register("/r", "hook-session", "hook")
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "hook-session",
		Kind: protocol.KindEyes, Origin: "relay"}); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a relay caller must not claim a local session, got %v", err)
	}
}

// A relay session id names exactly one row: holding its secret does not let a
// caller move it to another scope, change what it is, or shed its origin.
func TestRegisterRelayHoldsIdentityImmutable(t *testing.T) {
	s := open(t)
	first := registerBroker(t, s, "host:BOX", "broker-1")
	for _, c := range []struct {
		what string
		reg  RelayRegistration
	}{
		{"a second scope", RelayRegistration{Scope: "/repo-a", SessionID: "broker-1",
			Kind: protocol.KindLauncher, Origin: "relay", Secret: first.Secret}},
		{"a new kind", RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
			Kind: protocol.KindEyes, Origin: "relay", Secret: first.Secret}},
		{"a dropped origin", RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
			Kind: protocol.KindLauncher, Secret: first.Secret}},
	} {
		if _, err := s.RegisterRelay(c.reg); !errors.Is(err, ErrForeignSession) {
			t.Fatalf("%s must be ErrForeignSession, got %v", c.what, err)
		}
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='broker-1'`); n != 1 {
		t.Fatalf("a relay session must own exactly one row, got %d", n)
	}
	var kind, origin string
	if err := s.db.QueryRow(`SELECT kind, origin FROM agents WHERE session_id='broker-1'`).Scan(&kind, &origin); err != nil {
		t.Fatal(err)
	}
	if kind != protocol.KindLauncher || origin != "relay" {
		t.Fatalf("row after the refused re-registers: kind=%q origin=%q", kind, origin)
	}
}

// Registration is one transaction: a failed metadata write must leave no row
// at all, because a half-registered row can never be re-claimed.
func TestRegisterRelayIsOneTransaction(t *testing.T) {
	s := open(t)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_meta BEFORE UPDATE OF relay_secret_hash ON agents
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay"}); err == nil {
		t.Fatal("a failing metadata write must fail the registration")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='broker-1'`); n != 0 {
		t.Fatalf("a failed registration must leave no row, got %d", n)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_meta`); err != nil {
		t.Fatal(err)
	}
	r, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay"})
	if err != nil || len(r.Secret) != 64 {
		t.Fatalf("the retry must register cleanly: %+v (%v)", r, err)
	}
}

// The launcher holding a live task may re-mint its child session: without it,
// one lost register response strands the task with nobody able to report.
func TestReissueEyesChild(t *testing.T) {
	s := open(t)
	registerBroker(t, s, "host:BOX", "broker-1")
	seedTask(t, s, "task-1", "broker-1", "queued", s.Now().Unix())

	child, err := s.ReissueEyesChild("task-1", "broker-1")
	if err != nil || child.Name == "" || len(child.Secret) != 64 {
		t.Fatalf("first reissue: %+v (%v)", child, err)
	}
	id, err := s.VerifyRelaySecret("eyes-task-1", child.Secret)
	if err != nil || id.Scope != "/r" || id.Kind != protocol.KindEyes || id.Origin != "relay" {
		t.Fatalf("the child must be an eyes row in the requester scope: %+v (%v)", id, err)
	}
	// A second reissue replaces the secret: the lost one stops working.
	again, err := s.ReissueEyesChild("task-1", "broker-1")
	if err != nil || again.Name != child.Name || again.Secret == child.Secret {
		t.Fatalf("second reissue: %+v (%v)", again, err)
	}
	if _, err := s.VerifyRelaySecret("eyes-task-1", child.Secret); !errors.Is(err, ErrRelayAuth) {
		t.Fatalf("the replaced secret must stop working, got %v", err)
	}
	if _, err := s.VerifyRelaySecret("eyes-task-1", again.Secret); err != nil {
		t.Fatalf("the fresh secret must work: %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='eyes-task-1'`); n != 1 {
		t.Fatalf("reissuing must not clone the child row, got %d", n)
	}

	seedTask(t, s, "task-2", "broker-2", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-2", "broker-1"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("another launcher's task must be ErrNotYourTask, got %v", err)
	}
	if _, err := s.ReissueEyesChild("task-nope", "broker-1"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	seedTask(t, s, "task-3", "broker-1", "done", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-3", "broker-1"); !errors.Is(err, ErrTaskNotLive) {
		t.Fatalf("a settled task must mint no child, got %v", err)
	}
	seedTask(t, s, "task-4", "broker-1", "accepted", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-4", "broker-1"); err != nil {
		t.Fatalf("an accepted task is still live: %v", err)
	}
}

// One request_eyes is one transaction: reserve the launcher, queue the task
// and deliver task.launch, or do none of it.
func TestAssignEyesTaskQueuesAndDelivers(t *testing.T) {
	s := open(t)
	now := time.Unix(5000000, 0)
	s.Now = func() time.Time { return now }
	requester, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}

	task, launcher, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "claude", Brief: "read the page"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(task.TaskID, "task-") || len(task.TaskID) != len("task-")+12 {
		t.Fatalf("task id %q must be task- plus 12 hex", task.TaskID)
	}
	if _, err := hex.DecodeString(task.TaskID[len("task-"):]); err != nil {
		t.Fatalf("task id %q must be hex: %v", task.TaskID, err)
	}
	if task.State != "queued" || task.LauncherSession != "broker-1" || task.RequesterScope != "/r" ||
		task.RequesterAgentID != ref.AgentID || task.Runtime != "claude" || task.UpdatedAt != now.Unix() {
		t.Fatalf("queued task: %+v", task)
	}
	if launcher.Name != broker.Name || launcher.Scope != "host:BOX" {
		t.Fatalf("launcher ref: %+v", launcher)
	}
	msgs, err := s.Read("host:BOX", broker.Name)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("launcher inbox: %d (%v)", len(msgs), err)
	}
	m := msgs[0]
	if m.TaskID != task.TaskID || m.From != requester || m.FromScope != "/r" || m.ReplyTo == nil || m.ReplyTo.AgentID != ref.AgentID {
		t.Fatalf("launch mail: %+v (reply_to %+v)", m, m.ReplyTo)
	}
	var body protocol.TaskLaunchMsg
	if err := json.Unmarshal([]byte(m.Body), &body); err != nil {
		t.Fatalf("launch body %q: %v", m.Body, err)
	}
	if body.Type != protocol.TaskLaunch || body.TaskID != task.TaskID || body.Runtime != "claude" ||
		body.Scope != "/r" || body.Brief != "read the page" || body.DeadlineS != 300 || body.ReplyTo.AgentID != ref.AgentID {
		t.Fatalf("launch body: %+v", body)
	}
	// The launcher now holds a live task, so a second request is refused and
	// writes nothing.
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "again"}); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("a busy launcher must be ErrEyesBusy, got %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 1 {
		t.Fatalf("a refused request must queue nothing, got %d tasks", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("a refused request must send nothing, got %d messages", n)
	}
}

// deadline_s defaults to 300 and is capped at 1800 - the requester's bound on
// a host job, applied where the launch body is built.
func TestAssignEyesTaskCapsDeadline(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b", DeadlineS: 9999}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Read("host:BOX", broker.Name)
	var body protocol.TaskLaunchMsg
	if len(msgs) != 1 || json.Unmarshal([]byte(msgs[0].Body), &body) != nil || body.DeadlineS != 1800 {
		t.Fatalf("deadline must be capped at 1800: %+v", body)
	}
}

// No broker means no task and no mail: the caller has to say the host is not
// running rather than silently queue work nobody will see.
func TestAssignEyesTaskWithoutLauncherWritesNothing(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"}); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("no launcher: %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 0 {
		t.Fatalf("nothing must be queued, got %d tasks", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("nothing must be sent, got %d messages", n)
	}
}

// Transitions are validated against the actor and the current state, and the
// state change plus the requester's mail are one write - so a re-sent report
// is a no-op, not a second message.
func TestTransitionEyesTaskEdgesActorsAndIdempotency(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	childSession := "eyes-" + task.TaskID
	child, err := s.ReissueEyesChild(task.TaskID, "broker-1")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.TransitionEyesTask("task-nope", "broker-1", "accepted", "{}"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, childSession, "accepted", "{}"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("only the assigned launcher may accept, got %v", err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, "broker-1", "queued", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("queued is not a target state, got %v", err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, "broker-1", "done", "{}"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("only the child reports done, got %v", err)
	}

	accepted, ok, err := s.TransitionEyesTask(task.TaskID, "broker-1", "accepted",
		`{"type":"task.accepted","task_id":"`+task.TaskID+`"}`)
	if err != nil || !ok || accepted.State != "accepted" {
		t.Fatalf("accept: %+v ok=%v (%v)", accepted, ok, err)
	}
	if n, _ := s.UnreadCount("/r", requester); n != 1 {
		t.Fatalf("the requester must be told once, unread=%d", n)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, "broker-1", "accepted", "{}"); err != nil || ok {
		t.Fatalf("a repeated accept must be a quiet no-op: ok=%v (%v)", ok, err)
	}
	if n, _ := s.UnreadCount("/r", requester); n != 1 {
		t.Fatalf("a repeat must not send a second mail, unread=%d", n)
	}

	report := `{"type":"task.result","task_id":"` + task.TaskID + `","status":"ok"}`
	done, ok, err := s.TransitionEyesTask(task.TaskID, childSession, "done", report)
	if err != nil || !ok || done.State != "done" {
		t.Fatalf("done: %+v ok=%v (%v)", done, ok, err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, childSession, "failed", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("done is terminal, got %v", err)
	}
	msgs, err := s.Read("/r", requester)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("requester mail: %d (%v)", len(msgs), err)
	}
	if msgs[0].From != broker.Name || msgs[0].FromScope != "host:BOX" || msgs[0].Kind != protocol.KindLauncher ||
		msgs[0].TaskID != task.TaskID {
		t.Fatalf("the accept mail comes from the launcher: %+v", msgs[0])
	}
	if msgs[1].From != child.Name || msgs[1].Kind != protocol.KindEyes || msgs[1].Body != report ||
		msgs[1].ReplyTo == nil || msgs[1].ReplyTo.Name != child.Name {
		t.Fatalf("the report comes from the child: %+v (reply_to %+v)", msgs[1], msgs[1].ReplyTo)
	}
	// A settled task frees its launcher for the next brief.
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("a finished task frees the launcher: %+v (%v)", l, err)
	}
}

// A launcher that never accepted can still fail its task cleanly.
func TestTransitionEyesTaskLauncherMayFailQueued(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	failed, ok, err := s.TransitionEyesTask(task.TaskID, "broker-1", "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"no provider"}`)
	if err != nil || !ok || failed.State != "failed" {
		t.Fatalf("queued -> failed by the launcher: %+v ok=%v (%v)", failed, ok, err)
	}
	if n, _ := s.UnreadCount("/r", requester); n != 1 {
		t.Fatalf("the requester must hear about the failure, unread=%d", n)
	}
}

// A task is owned by its requester (or one of that agent's bound children),
// and Housekeep sweeps it after a day.
func TestEyesTaskOwnershipAndHousekeep(t *testing.T) {
	s := open(t)
	now := time.Unix(2000000, 0)
	s.Now = func() time.Time { return now }
	nA, _ := s.Register("/r", "s-a", "hook")
	childName, err := s.RegisterChild("/r", "s-a", "sub1", "Explore")
	if err != nil {
		t.Fatal(err)
	}
	nB, _ := s.Register("/r", "s-b", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	requester := protocol.AgentRef{Name: nA, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Runtime: "claude", Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.EyesTask(task.TaskID)
	if err != nil || got.State != "queued" || got.LauncherSession != "broker-1" || got.CreatedAt != now.Unix() {
		t.Fatalf("stored task: %+v (%v)", got, err)
	}
	if _, err := s.EyesTask("task-nope"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	stranger := protocol.AgentRef{Name: nB, AgentID: agentID("s-b"), Scope: "/r"}
	if _, err := s.CancelEyesTask(task.TaskID, stranger); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a stranger must not cancel: %v", err)
	}
	child := protocol.AgentRef{Name: childName, AgentID: agentID(ChildSessionID("s-a", "sub1")), Scope: "/r"}
	cancelled, err := s.CancelEyesTask(task.TaskID, child)
	if err != nil || cancelled.State != "cancelled" || cancelled.LauncherSession != "broker-1" {
		t.Fatalf("a bound child may cancel its parent's task: %+v (%v)", cancelled, err)
	}
	now = now.Add(25 * time.Hour)
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EyesTask(task.TaskID); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("a task older than 24h must be purged, got %v", err)
	}
}

// A live eyes child must not make the workspace look hook-occupied: an MCP
// client that self-mints while a child runs would otherwise be refused.
func TestRelayAgentDoesNotBlockSelfMintedIdentity(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "eyes-task-1",
		Kind: protocol.KindEyes, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterIfNoLiveHook("/r", "mcp-abc", "mcp"); err != nil {
		t.Fatalf("an eyes child must not look like a hook agent: %v", err)
	}
	s.Register("/r", "hook-session", "hook")
	if _, err := s.RegisterIfNoLiveHook("/r", "mcp-def", "mcp"); !errors.Is(err, ErrIdentityUnknown) {
		t.Fatalf("a real hook agent must still block a self-mint, got %v", err)
	}
}

// An explicit from must be the caller itself or one of its registered
// children; a name that merely looks like a child is not one.
func TestResolveActorAcceptsSelfAndRegisteredChildren(t *testing.T) {
	s := open(t)
	parent, _ := s.Register("/r", "s-a", "hook")
	child, err := s.RegisterChild("/r", "s-a", "sub1", "Explore")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.Register("/r", "s-b", "hook")

	if id, err := s.ResolveActor("/r", "s-a", ""); err != nil || id.Name != parent {
		t.Fatalf("no from means the caller: %+v (%v)", id, err)
	}
	if id, err := s.ResolveActor("/r", "s-a", parent); err != nil || id.Name != parent || id.AgentID != agentID("s-a") {
		t.Fatalf("own name: %+v (%v)", id, err)
	}
	if id, err := s.ResolveActor("/r", "s-a", agentID("s-a")); err != nil || id.Name != parent {
		t.Fatalf("own agent id: %+v (%v)", id, err)
	}
	if id, err := s.ResolveActor("/r", "s-a", child); err != nil || id.Name != child || id.Parent != parent {
		t.Fatalf("registered child: %+v (%v)", id, err)
	}
	if _, err := s.ResolveActor("/r", "s-a", other); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("another agent must be refused, got %v", err)
	}
	if _, err := s.ResolveActor("/r", "s-a", parent+"/explore-9"); err == nil {
		t.Fatal("an unregistered child name must be refused")
	}
	// A caller that names no session is a plain unix client (the CLI, wait):
	// the socket directory's permissions are its trust boundary.
	if id, err := s.ResolveActor("/r", "", other); err != nil || id.Name != other {
		t.Fatalf("unidentified caller: %+v (%v)", id, err)
	}
}
