package store

import (
	"database/sql"
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

// registerBroker registers a host broker the way the relay gate will: caps
// included, because a broker earns runtime-qualified briefs by advertising
// providers, and holding the secret it minted for itself. The store answers a
// register with no secret, so the helper hands back the one the broker kept.
func registerBroker(t *testing.T, s *Store, scope, session string) RelayResult {
	t.Helper()
	r, err := s.RegisterRelay(RelayRegistration{Scope: scope, SessionID: session,
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Secret: brokerSecret(session), Capabilities: []string{"browser.chrome", "provider.claude"}})
	if err != nil {
		t.Fatal(err)
	}
	r.Secret = brokerSecret(session)
	return r
}

// brokerSecret is a test client's preminted credential: 64 lowercase hex,
// stable per session so a retry presents the same one.
func brokerSecret(session string) string { return sha256Hex("secret:" + session) }

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
	var acked int
	if err := s.db.QueryRow(`SELECT state, runtime, cancel_acked FROM eyes_tasks WHERE task_id='task-abc'`).
		Scan(&state, &runtime, &acked); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || runtime != "" || acked != 0 {
		t.Fatalf("eyes_tasks defaults: state=%q runtime=%q cancel_acked=%d, want queued, \"\" and 0",
			state, runtime, acked)
	}
}

// The workspace directory counts live agents by kind and hides the reserved
// host: scopes - those are broker plumbing, not workspaces.
func TestListWorkspacesCountsByKindAndHidesHostScopes(t *testing.T) {
	s := open(t)
	s.Register("/repo-a", "s-a1", "hook")
	s.Register("/repo-a", "s-a2", "hook")
	s.Register("/repo-b", "s-b1", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	seedTaskIn(t, s, "/repo-a", "task-1", "broker-1", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-1", "broker-1", broker.Secret); err != nil {
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
		Secret: brokerSecret("broker-1"), Capabilities: []string{"browser.chrome"}}); err != nil {
		t.Fatal(err)
	}
	seedTaskIn(t, s, "/repo-a", "task-1", "broker-1", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-1", "broker-1", brokerSecret("broker-1")); err != nil {
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
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	seedTask(t, s, "task-9", "broker-1", "queued", s.Now().Unix())
	child, err := s.ReissueEyesChild("task-9", "broker-1", broker.Secret)
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

// The client mints its own secret before the first register; the store keeps
// only its sha256 and answers with nothing but the name. Verification is by
// secret, not by session id alone.
func TestRegisterRelayBindsThePremintedSecret(t *testing.T) {
	s := open(t)
	secret := brokerSecret("broker-1")
	first, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: secret})
	if err != nil || first.Name == "" || first.Secret != "" {
		t.Fatalf("register must name the row and mint nothing: %+v (%v)", first, err)
	}
	again, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: secret})
	if err != nil || again.Name != first.Name || again.Secret != "" {
		t.Fatalf("an exact retry must return the same identity: %+v (%v)", again, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT relay_secret_hash FROM agents WHERE session_id='broker-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != sha256Hex(secret) {
		t.Fatalf("the store must keep only the hash, got %q", stored)
	}
	id, err := s.VerifyRelaySecret("broker-1", secret)
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

// A relay row is created only on a credential the client already holds, in
// the one shape the wire fixes: 32 CSPRNG bytes as lowercase hex.
func TestRegisterRelayRequiresPremintedSecret(t *testing.T) {
	s := open(t)
	for _, bad := range []string{"", "wrong", strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.ToUpper(brokerSecret("broker-1")), strings.Repeat("g", 64)} {
		if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
			Kind: protocol.KindLauncher, Origin: "relay", Secret: bad}); !errors.Is(err, ErrRelayAuth) {
			t.Fatalf("secret %q must be ErrRelayAuth, got %v", bad, err)
		}
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents`); n != 0 {
		t.Fatalf("a refused registration must leave no row, got %d", n)
	}
	// A local kind-bearing row carries no secret at all: the socket
	// directory's permissions are its trust boundary.
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "local-1",
		Kind: protocol.KindLauncher}); err != nil {
		t.Fatalf("a local launcher needs no secret: %v", err)
	}
}

// An eyes child is minted by the launcher holding its task, never by a
// register: the shared token alone must not be able to create one, and a
// register must not touch one that already exists either.
func TestRegisterRelayNeverTouchesAnEyesRow(t *testing.T) {
	s := open(t)
	for _, origin := range []string{"relay", ""} {
		if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "eyes-task-1",
			Kind: protocol.KindEyes, Origin: origin, Secret: brokerSecret("eyes-task-1")}); !errors.Is(err, ErrForeignSession) {
			t.Fatalf("a new eyes row with origin %q must be ErrForeignSession, got %v", origin, err)
		}
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents`); n != 0 {
		t.Fatalf("a refused eyes register must leave no row, got %d", n)
	}
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	seedTask(t, s, "task-1", "broker-1", "queued", s.Now().Unix())
	child, err := s.ReissueEyesChild("task-1", "broker-1", broker.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "eyes-task-1",
		Kind: protocol.KindEyes, Origin: "relay", Secret: child.Secret,
		Platform: "windows", Capabilities: []string{"browser.chrome"}}); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("re-registering an eyes row must be ErrForeignSession, got %v", err)
	}
	var platform, caps string
	if err := s.db.QueryRow(`SELECT platform, caps FROM agents WHERE session_id='eyes-task-1'`).
		Scan(&platform, &caps); err != nil {
		t.Fatal(err)
	}
	if platform != "" || caps != "[]" {
		t.Fatalf("the refused register must refresh nothing: platform=%q caps=%q", platform, caps)
	}
	if _, err := s.VerifyRelaySecret("eyes-task-1", child.Secret); err != nil {
		t.Fatalf("the reissued child must keep its credential: %v", err)
	}
}

// A relay session id names one row in one workspace, which is what lets the
// gate authenticate a session before it knows its scope: the right secret
// does not buy the same id in a second scope.
func TestRelaySessionIDIsUniqueAcrossScopes(t *testing.T) {
	s := open(t)
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/repo-a", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: broker.Secret}); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a second scope must be ErrForeignSession even with the right secret, got %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='broker-1'`); n != 1 {
		t.Fatalf("a relay session must own exactly one row, got %d", n)
	}
	if id, err := s.VerifyRelaySecret("broker-1", broker.Secret); err != nil || id.Scope != "host:BOX" {
		t.Fatalf("the original row still answers: %+v (%v)", id, err)
	}
}

// A launcher keeps one session across reconnects, so re-registering it means
// proving the minted secret; a session created locally is never claimable.
func TestRegisterRelayReRegisterNeedsTheSecret(t *testing.T) {
	s := open(t)
	first, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Secret: brokerSecret("broker-1")})
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
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("broker-1"),
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
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("hook-session")}); !errors.Is(err, ErrForeignSession) {
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
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("broker-1")}); err == nil {
		t.Fatal("a failing metadata write must fail the registration")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='broker-1'`); n != 0 {
		t.Fatalf("a failed registration must leave no row, got %d", n)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_meta`); err != nil {
		t.Fatal(err)
	}
	r, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("broker-1")})
	if err != nil || r.Name == "" {
		t.Fatalf("the retry must register cleanly: %+v (%v)", r, err)
	}
}

// The launcher holding a live task may re-mint its child session: without it,
// one lost register response strands the task with nobody able to report.
func TestReissueEyesChild(t *testing.T) {
	s := open(t)
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	seedTask(t, s, "task-1", "broker-1", "queued", s.Now().Unix())

	child, err := s.ReissueEyesChild("task-1", "broker-1", broker.Secret)
	if err != nil || child.Name == "" || len(child.Secret) != 64 {
		t.Fatalf("first reissue: %+v (%v)", child, err)
	}
	id, err := s.VerifyRelaySecret("eyes-task-1", child.Secret)
	if err != nil || id.Scope != "/r" || id.Kind != protocol.KindEyes || id.Origin != "relay" {
		t.Fatalf("the child must be an eyes row in the requester scope: %+v (%v)", id, err)
	}
	// A second reissue replaces the secret: the lost one stops working.
	again, err := s.ReissueEyesChild("task-1", "broker-1", broker.Secret)
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

	// Minting a credential takes the launcher's own secret, not just its name.
	for _, bad := range []string{"", "wrong"} {
		if _, err := s.ReissueEyesChild("task-1", "broker-1", bad); !errors.Is(err, ErrRelayAuth) {
			t.Fatalf("launcher secret %q must be ErrRelayAuth, got %v", bad, err)
		}
	}
	// A relay row that is not a launcher cannot mint a child even holding its
	// own valid secret - the task's own child included.
	seedTask(t, s, "task-5", "eyes-task-1", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-5", "eyes-task-1", again.Secret); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a non-launcher row must not mint a child, got %v", err)
	}

	seedTask(t, s, "task-2", "broker-2", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-2", "broker-1", broker.Secret); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("another launcher's task must be ErrNotYourTask, got %v", err)
	}
	if _, err := s.ReissueEyesChild("task-nope", "broker-1", broker.Secret); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	seedTask(t, s, "task-3", "broker-1", "done", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-3", "broker-1", broker.Secret); !errors.Is(err, ErrTaskNotLive) {
		t.Fatalf("a settled task must mint no child, got %v", err)
	}
	seedTask(t, s, "task-4", "broker-1", "accepted", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-4", "broker-1", broker.Secret); err != nil {
		t.Fatalf("an accepted task is still live: %v", err)
	}
}

// The child row is the task's own or it is not touched: a session id that
// already belongs to a local agent, or to a row in another workspace, is
// somebody else's identity.
func TestReissueEyesChildRefusesAForeignRow(t *testing.T) {
	s := open(t)
	broker := registerBroker(t, s, "host:BOX", "broker-1")

	// A local agent squatting the child's session id.
	seedTask(t, s, "task-1", "broker-1", "queued", s.Now().Unix())
	s.Register("/r", "eyes-task-1", "hook")
	if _, err := s.ReissueEyesChild("task-1", "broker-1", broker.Secret); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a local row must not be taken over, got %v", err)
	}
	var kind, origin, hash string
	if err := s.db.QueryRow(`SELECT kind, origin, relay_secret_hash FROM agents WHERE session_id='eyes-task-1'`).
		Scan(&kind, &origin, &hash); err != nil {
		t.Fatal(err)
	}
	if kind != "" || origin != "" || hash != "" {
		t.Fatalf("the local row must be untouched: kind=%q origin=%q hash=%q", kind, origin, hash)
	}

	// A relay row wearing the child's session id in another workspace.
	registerBroker(t, s, "host:BOX", "eyes-task-2")
	seedTask(t, s, "task-2", "broker-1", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-2", "broker-1", broker.Secret); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("another scope's row must not be cloned, got %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='eyes-task-2'`); n != 1 {
		t.Fatalf("the reissue must not add a second row, got %d", n)
	}
}

// A live eyes child must not make the workspace look hook-occupied: an MCP
// client that self-mints while a child runs would otherwise be refused.
func TestRelayAgentDoesNotBlockSelfMintedIdentity(t *testing.T) {
	s := open(t)
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	seedTask(t, s, "task-1", "broker-1", "queued", s.Now().Unix())
	if _, err := s.ReissueEyesChild("task-1", "broker-1", broker.Secret); err != nil {
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

// Acting as a registered child requires the caller's own row to still be
// here: a child whose parent was purged is nobody's proxy.
func TestResolveActorNeedsTheCallersOwnRow(t *testing.T) {
	s := open(t)
	s.Register("/r", "s-a", "hook")
	child, err := s.RegisterChild("/r", "s-a", "sub1", "Explore")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM agents WHERE scope='/r' AND session_id='s-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveActor("/r", "s-a", child); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a purged parent must not act as its child, got %v", err)
	}
}

// A relay session authenticates against its relay row and nothing else. A
// local row may legitimately share the session id in another workspace, and
// registering in the same second used to sort it ahead of the broker's row -
// which locked the broker out of its own identity.
func TestVerifyRelaySecretIgnoresALocalRowWithTheSameSessionID(t *testing.T) {
	s := open(t)
	now := time.Unix(9100000, 0)
	s.Now = func() time.Time { return now }
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	if _, err := s.Register("/r", "broker-1", "hook"); err != nil {
		t.Fatal(err)
	}
	id, err := s.VerifyRelaySecret("broker-1", brokerSecret("broker-1"))
	if err != nil || id.Scope != "host:BOX" || id.Name != broker.Name || id.Kind != protocol.KindLauncher {
		t.Fatalf("the relay row must answer for a relay session: %+v (%v)", id, err)
	}
	// A session that has no relay row at all is still foreign, not missing.
	if _, err := s.Register("/r", "hook-only", "hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyRelaySecret("hook-only", ""); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("a hook session must stay ErrForeignSession, got %v", err)
	}
}

// secretHashOf reads the stored credential hash of a session's row.
func secretHashOf(t *testing.T, s *Store, session string) string {
	t.Helper()
	var hash string
	if err := s.db.QueryRow(`SELECT relay_secret_hash FROM agents WHERE session_id=?`, session).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// A relay row is bound to the secret it was created with, and an empty hash
// on an existing row is damage, not an invitation: a bare register may never
// bind a credential to a row it did not create.
func TestRegisterRelayNeverBindsAnExistingRow(t *testing.T) {
	s := open(t)
	registerBroker(t, s, "host:BOX", "broker-1")
	if _, err := s.db.Exec(`UPDATE agents SET relay_secret_hash='' WHERE session_id='broker-1'`); err != nil {
		t.Fatal(err)
	}
	_, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "linux",
		Secret: brokerSecret("intruder"), Capabilities: []string{"browser.chrome"}})
	if !errors.Is(err, ErrRelayAuth) {
		t.Fatalf("a tampered relay row must not be claimable, got %v", err)
	}
	if h := secretHashOf(t, s, "broker-1"); h != "" {
		t.Fatalf("a refused register must bind nothing, got %q", h)
	}
	if p := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='broker-1' AND platform='windows'`); p != 1 {
		t.Fatal("a refused register must not rewrite the row")
	}
}

// A sender in another workspace still has a name here: every inbox read
// resolves it in the SENDER's scope, so peek, history and read agree.
func TestCrossScopeSenderResolvesToAName(t *testing.T) {
	s := open(t)
	nA, _ := s.Register("/repo-a", "s-a", "hook")
	nB, _ := s.Register("/repo-b", "s-b", "hook")
	if err := s.SendToScope(Delivery{FromScope: "/repo-a", FromName: nA, ToScope: "/repo-b",
		ToName: nB, Body: "hello over there"}); err != nil {
		t.Fatal(err)
	}
	info, err := s.PeekMail("/repo-b", nB, 0)
	if err != nil || len(info.Froms) != 1 || info.Froms[0] != nA {
		t.Fatalf("peek must name the foreign sender: %+v (%v)", info, err)
	}
	hist, err := s.MessageHistory("/repo-b", nB, "", 0)
	if err != nil || len(hist) != 1 || hist[0].From != nA || hist[0].To != nB {
		t.Fatalf("history must name the foreign sender: %+v (%v)", hist, err)
	}
	msgs, err := s.Read("/repo-b", nB)
	if err != nil || len(msgs) != 1 || msgs[0].From != nA {
		t.Fatalf("read must name the foreign sender: %+v (%v)", msgs, err)
	}
}
