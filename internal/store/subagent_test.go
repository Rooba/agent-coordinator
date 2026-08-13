package store

import (
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

func TestRegisterChildIdempotentAndSuffix(t *testing.T) {
	s := open(t)
	parent, err := s.Register("/r", "parent-sess", "hook")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := s.RegisterChild("/r", "parent-sess", "a1", "Explore")
	if err != nil || c1 != parent+"/explore-1" {
		t.Fatalf("first child: %q err=%v", c1, err)
	}
	// Same (scope, child session) is idempotent.
	again, err := s.RegisterChild("/r", "parent-sess", "a1", "Explore")
	if err != nil || again != c1 {
		t.Fatalf("re-register: %q err=%v", again, err)
	}
	// A second distinct child of the same type takes the next suffix.
	c2, err := s.RegisterChild("/r", "parent-sess", "a2", "Explore")
	if err != nil || c2 != parent+"/explore-2" {
		t.Fatalf("second child: %q err=%v", c2, err)
	}
	// Empty agent type falls back to "sub".
	c3, err := s.RegisterChild("/r", "parent-sess", "a3", "")
	if err != nil || c3 != parent+"/sub-1" {
		t.Fatalf("typeless child: %q err=%v", c3, err)
	}
	id, err := s.Identity("/r", ChildSessionID("parent-sess", "a1"))
	if err != nil || id.Name != c1 || id.Source != "hook-subagent" || id.Parent != parent {
		t.Fatalf("child identity: %+v err=%v", id, err)
	}
}

// A missing parent row is registered on the fly so the child never fails.
func TestRegisterChildCreatesAbsentParent(t *testing.T) {
	s := open(t)
	c, err := s.RegisterChild("/r", "orphan-sess", "a1", "Plan")
	if err != nil || !strings.HasSuffix(c, "/plan-1") {
		t.Fatalf("child: %q err=%v", c, err)
	}
	if id, err := s.Identity("/r", "orphan-sess"); err != nil || id.Name == "" {
		t.Fatalf("parent must exist after child registration: %+v err=%v", id, err)
	}
}

// Retro test case 1: the child identity reading its inbox must not drain the
// parent's unread mail.
func TestChildReadLeavesParentUnread(t *testing.T) {
	s := open(t)
	parent, _ := s.Register("/r", "parent-sess", "hook")
	peer, _ := s.Register("/r", "peer-sess", "hook")
	child, err := s.RegisterChild("/r", "parent-sess", "a1", "Explore")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"for-parent-1", "for-parent-2"} {
		if err := s.Send("/r", peer, parent, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Send("/r", peer, child, "for-child"); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Read("/r", child)
	if err != nil || len(msgs) != 1 || msgs[0].Body != "for-child" {
		t.Fatalf("child read: %v err=%v", msgs, err)
	}
	if n, _ := s.UnreadCount("/r", parent); n != 2 {
		t.Fatalf("parent unread after child read: want 2, got %d", n)
	}
	msgs, _ = s.Read("/r", parent)
	if len(msgs) != 2 {
		t.Fatalf("parent read: %v", msgs)
	}
}

// The parent's Stop-hook notice drain must not consume the child's notices:
// notices key on the (distinct) child session.
func TestParentNoticeDrainLeavesChildNotices(t *testing.T) {
	s := open(t)
	parent, _ := s.Register("/r", "parent-sess", "hook")
	peer, _ := s.Register("/r", "peer-sess", "hook")
	child, _ := s.RegisterChild("/r", "parent-sess", "a1", "Explore")
	if err := s.Send("/r", peer, parent, "for-parent"); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("/r", peer, child, "for-child"); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingNotices("/r", "parent-sess")
	if err != nil || len(got) != 1 || !strings.Contains(got[0], peer) {
		t.Fatalf("parent notices: %v err=%v", got, err)
	}
	got, err = s.PendingNotices("/r", ChildSessionID("parent-sess", "a1"))
	if err != nil || len(got) != 1 || !strings.Contains(got[0], peer) {
		t.Fatalf("child notices must survive the parent drain: %v err=%v", got, err)
	}
}

// Child activity lands on the child row and the board links it to its parent.
func TestBoardShowsChildUnderParent(t *testing.T) {
	s := open(t)
	parent, _ := s.Register("/r", "parent-sess", "hook")
	child, _ := s.RegisterChild("/r", "parent-sess", "a1", "Explore")
	childSess := ChildSessionID("parent-sess", "a1")
	if _, err := s.RecordEvent("/r", childSess, protocol.Request{Tool: "Read", Activity: "Reading x"}); err != nil {
		t.Fatal(err)
	}
	board, err := s.Board("/r", false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]protocol.AgentInfo{}
	for _, a := range board {
		byName[a.Name] = a
	}
	if a := byName[child]; a.Parent != parent || a.Activity != "Reading x" {
		t.Fatalf("child row: %+v", a)
	}
	if a := byName[parent]; a.Parent != "" || a.Activity != "" {
		t.Fatalf("parent row must stay clean: %+v", a)
	}
}
