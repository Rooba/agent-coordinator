package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
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

// A database written before the relay columns existed must migrate in place:
// the old row survives on the new defaults and every column is present.
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
		INSERT INTO agents VALUES ('/r','s-old','aid-old','old-agent','active',1,1);`); err != nil {
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

// The workspace directory counts live agents by kind and hides the reserved
// host: scopes - those are broker plumbing, not workspaces.
func TestListWorkspacesCountsByKindAndHidesHostScopes(t *testing.T) {
	s := open(t)
	s.Register("/repo-a", "s-a1", "hook")
	s.Register("/repo-a", "s-a2", "hook")
	s.Register("/repo-b", "s-b1", "hook")
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows"}); err != nil {
		t.Fatal(err)
	}
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
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" || l.Scope != "host:BOX" {
		t.Fatalf("fresh launcher: %+v (%v)", l, err)
	}
	now = now.Add(45 * time.Second) // still "active" for presence, but no longer polling
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("stale launcher must not be picked, got %v", err)
	}
}

// A launcher running a task is busy: the pick skips it, and when every live
// launcher is busy the caller gets ErrEyesBusy rather than "no host launcher".
func TestPickLauncherSkipsBusyLauncher(t *testing.T) {
	s := open(t)
	now := time.Unix(3000000, 0)
	s.Now = func() time.Time { return now }
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second) // broker-2 is the more recently seen launcher
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-2",
		Kind: protocol.KindLauncher, Origin: "relay"}); err != nil {
		t.Fatal(err)
	}
	mk := func(taskID, session string) {
		t.Helper()
		if _, _, err := s.CreateEyesTask(EyesTask{TaskID: taskID, RequesterScope: "/r",
			RequesterAgentID: "aid-a", LauncherSession: session, Runtime: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	mk("task-1", "broker-2")
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" {
		t.Fatalf("a busy launcher must be skipped: %+v (%v)", l, err)
	}
	mk("task-2", "broker-1")
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("every launcher busy must be ErrEyesBusy, got %v", err)
	}
	if err := s.SetEyesTaskState("task-1", "done"); err != nil {
		t.Fatal(err)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-2" {
		t.Fatalf("a finished task frees its launcher: %+v (%v)", l, err)
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
	requester := protocol.AgentRef{Name: nA, AgentID: agentID("s-a"), Scope: "/r"}
	newTask := EyesTask{TaskID: "task-1", RequesterScope: "/r",
		RequesterAgentID: requester.AgentID, LauncherSession: "broker-1", Runtime: "claude"}
	if _, created, err := s.CreateEyesTask(newTask); err != nil || !created {
		t.Fatalf("first create: created=%v (%v)", created, err)
	}
	if live, created, err := s.CreateEyesTask(newTask); err != nil || created || live.State != "queued" {
		t.Fatalf("a repeat create must return the live row, not launch again: %+v created=%v (%v)", live, created, err)
	}
	got, err := s.EyesTask("task-1")
	if err != nil || got.State != "queued" || got.LauncherSession != "broker-1" || got.CreatedAt != now.Unix() {
		t.Fatalf("stored task: %+v (%v)", got, err)
	}
	if _, err := s.EyesTask("task-nope"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	stranger := protocol.AgentRef{Name: nB, AgentID: agentID("s-b"), Scope: "/r"}
	if _, err := s.CancelEyesTask("task-1", stranger); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a stranger must not cancel: %v", err)
	}
	child := protocol.AgentRef{Name: childName, AgentID: agentID(ChildSessionID("s-a", "sub1")), Scope: "/r"}
	cancelled, err := s.CancelEyesTask("task-1", child)
	if err != nil || cancelled.State != "cancelled" || cancelled.LauncherSession != "broker-1" {
		t.Fatalf("a bound child may cancel its parent's task: %+v (%v)", cancelled, err)
	}
	now = now.Add(25 * time.Hour)
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EyesTask("task-1"); !errors.Is(err, ErrUnknownTask) {
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
