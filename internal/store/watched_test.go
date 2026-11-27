package store

import (
	"testing"
	"time"
)

func boardRow(t *testing.T, s *Store, scope, name string) (AgentIdentity, bool) {
	t.Helper()
	board, err := s.Board(scope, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range board {
		if a.Name == name {
			return AgentIdentity{Name: a.Name, Wake: a.Wake, Source: a.Status}, true
		}
	}
	return AgentIdentity{}, false
}

// A session whose mail monitor is still polling is somebody's inbox no matter
// how old its last heartbeat is: it stays on the board as idle/monitor, still
// receives broadcasts, still counts as a live hook agent, and survives the
// two-hour purge. Once the monitor lapses it decays like any other row.
func TestWatchedRowNeverDecaysToGone(t *testing.T) {
	s := open(t)
	now := time.Unix(1_000_000, 0)
	s.Now = func() time.Time { return now }
	watchedName, _ := s.Register("/r", "watched", "hook")
	plainName, _ := s.Register("/r", "plain", "hook")
	senderName, _ := s.Register("/r", "sender", "hook")
	watchedID, _ := s.Identity("/r", "watched")

	now = now.Add(3 * time.Hour)
	s.StampWatcher("/r", watchedID)
	if err := s.Touch("/r", "sender"); err != nil {
		t.Fatal(err)
	}

	if row, ok := boardRow(t, s, "/r", watchedName); !ok || row.Source != "idle" || row.Wake != WakeMonitor {
		t.Fatalf("watched row must show as idle/monitor on the default board: %+v ok=%v", row, ok)
	}
	if _, ok := boardRow(t, s, "/r", plainName); ok {
		t.Fatal("an unwatched 3h-old row must be hidden as gone")
	}
	if err := s.Broadcast("/r", senderName, "all hands"); err != nil {
		t.Fatal(err)
	}
	if info, err := s.PeekMail("/r", watchedName, 0); err != nil || info.Unread != 1 {
		t.Fatalf("broadcast must reach the watched row: %+v %v", info, err)
	}
	if info, _ := s.PeekMail("/r", plainName, 0); info.Unread != 0 {
		t.Fatal("broadcast must skip the gone row")
	}
	if _, err := s.RegisterIfNoLiveHook("/r", "mcp-fallback", "mcp"); err != ErrIdentityUnknown {
		t.Fatalf("a watched hook session must block fallback minting, got %v", err)
	}
	if ws, err := s.ListWorkspaces(); err != nil || len(ws) != 1 || ws[0].Scope != "/r" {
		t.Fatalf("watched scope must stay discoverable: %+v %v", ws, err)
	}

	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identity("/r", "watched"); err != nil {
		t.Fatalf("housekeeping purged a watched row: %v", err)
	}
	if _, err := s.Identity("/r", "plain"); err == nil {
		t.Fatal("housekeeping must still purge an unwatched stale row")
	}

	now = now.Add(WatcherLive + time.Second)
	if _, ok := boardRow(t, s, "/r", watchedName); ok {
		t.Fatal("a lapsed monitor must let the row decay to gone")
	}
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identity("/r", "watched"); err == nil {
		t.Fatal("housekeeping must purge the row once its monitor lapsed")
	}
}

// A live monitor protects exactly its own (scope, agent) row. The same session
// registered in a second workspace with no monitor there still ages out, and
// claims held by the watched row survive housekeeping with it.
func TestWatchedRetentionIsScopedAndKeepsClaims(t *testing.T) {
	s := open(t)
	now := time.Unix(1_000_000, 0)
	s.Now = func() time.Time { return now }
	nameA, _ := s.Register("/a", "sess", "hook")
	if _, err := s.Register("/b", "sess", "hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("/a", nameA, "hub.go", "editing"); err != nil {
		t.Fatal(err)
	}
	idA, _ := s.Identity("/a", "sess")

	now = now.Add(3 * time.Hour)
	s.StampWatcher("/a", idA)
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identity("/a", "sess"); err != nil {
		t.Fatalf("watched row in /a must survive: %v", err)
	}
	if _, err := s.Identity("/b", "sess"); err == nil {
		t.Fatal("the unwatched row for the same session in /b must be purged")
	}
	claims, err := s.ListClaims("/a")
	if err != nil || len(claims) != 1 || claims[0].Path != "hub.go" {
		t.Fatalf("a watched holder keeps its claims: %+v %v", claims, err)
	}
}
