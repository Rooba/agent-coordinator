package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-coordinator/go/internal/protocol"
)

func twoAgents(t *testing.T) (*Store, string, string) {
	t.Helper()
	s := open(t)
	nA, _ := s.Register("/r", "sess-a", "startup")
	nB, _ := s.Register("/r", "sess-b", "startup")
	return s, nA, nB
}

func TestSendNoticeOnceThenRead(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "ping"); err != nil {
		t.Fatal(err)
	}
	// B's next event carries the notice exactly once.
	notices, _ := s.RecordEvent("/r", "sess-b", protocol.Request{Tool: "Read", Activity: "Reading x"})
	if len(notices) != 1 || !strings.Contains(notices[0], "1 new message") || !strings.Contains(notices[0], nA) {
		t.Fatalf("want one notice naming %s, got %v", nA, notices)
	}
	notices, _ = s.RecordEvent("/r", "sess-b", protocol.Request{Tool: "Read", Activity: "Reading x"})
	if len(notices) != 0 {
		t.Fatalf("notice must be delivered once, got %v", notices)
	}
	msgs, err := s.Read("/r", nB)
	if err != nil || len(msgs) != 1 || msgs[0].Body != "ping" || msgs[0].From != nA || msgs[0].Broadcast {
		t.Fatalf("read: %v %v", msgs, err)
	}
	if msgs, _ = s.Read("/r", nB); len(msgs) != 0 {
		t.Fatal("read must mark messages read")
	}
}

// Regression: the notice collect+mark in noticesFor must be atomic - two
// concurrent RecordEvent calls for one recipient must emit exactly one notice.
func TestConcurrentRecordEventEmitsNoticeOnce(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "ping"); err != nil {
		t.Fatal(err)
	}
	var barrier, done sync.WaitGroup
	barrier.Add(1)
	notices := make([][]string, 2)
	errs := make([]error, 2)
	done.Add(2)
	for i := range notices {
		go func(i int) {
			defer done.Done()
			barrier.Wait()
			notices[i], errs[i] = s.RecordEvent("/r", "sess-b", protocol.Request{Tool: "Read", Activity: "r"})
		}(i)
	}
	barrier.Done()
	done.Wait()
	total := 0
	for i := range notices {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		total += len(notices[i])
	}
	if total != 1 {
		t.Fatalf("want exactly one notice across both events, got %d: %v", total, notices)
	}
}

// Regression: the unread collect+mark in Read must be atomic - two concurrent
// Read calls must return the pending message exactly once between them.
func TestConcurrentReadDeliversOnce(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "ping"); err != nil {
		t.Fatal(err)
	}
	var barrier, done sync.WaitGroup
	barrier.Add(1)
	msgs := make([][]protocol.Message, 2)
	errs := make([]error, 2)
	done.Add(2)
	for i := range msgs {
		go func(i int) {
			defer done.Done()
			barrier.Wait()
			msgs[i], errs[i] = s.Read("/r", nB)
		}(i)
	}
	barrier.Done()
	done.Wait()
	total := 0
	for i := range msgs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		total += len(msgs[i])
	}
	if total != 1 {
		t.Fatalf("want the message delivered exactly once across both reads, got %d: %v", total, msgs)
	}
}

func TestSendToUnknownAgentFails(t *testing.T) {
	s, nA, _ := twoAgents(t)
	if err := s.Send("/r", nA, "nobody-here", "x"); err == nil {
		t.Fatal("want error for unknown recipient")
	}
}

func TestBroadcastReachesAllButSender(t *testing.T) {
	s, nA, _ := twoAgents(t)
	nC, _ := s.Register("/r", "sess-c", "startup")
	if err := s.Broadcast("/r", nA, "release at noon"); err != nil {
		t.Fatal(err)
	}
	noticesA, _ := s.RecordEvent("/r", "sess-a", protocol.Request{Tool: "Read", Activity: "r"})
	if len(noticesA) != 0 {
		t.Fatalf("sender must not be notified of own broadcast: %v", noticesA)
	}
	noticesC, _ := s.RecordEvent("/r", "sess-c", protocol.Request{Tool: "Read", Activity: "r"})
	if len(noticesC) != 1 || !strings.Contains(noticesC[0], "broadcast from "+nA) {
		t.Fatalf("got %v", noticesC)
	}
	msgs, _ := s.Read("/r", nC)
	if len(msgs) != 1 || !msgs[0].Broadcast {
		t.Fatalf("got %v", msgs)
	}
}

func TestConflictNotice(t *testing.T) {
	s, _, nB := twoAgents(t)
	s.RecordEvent("/r", "sess-b", protocol.Request{Tool: "Edit", Activity: "Editing a.go",
		Files: []string{"/r/a.go"}, Writes: []string{"/r/a.go"}})
	notices, _ := s.RecordEvent("/r", "sess-a", protocol.Request{Tool: "Edit", Activity: "Editing a.go",
		Files: []string{"/r/a.go"}, Writes: []string{"/r/a.go"}})
	found := false
	for _, n := range notices {
		if strings.Contains(n, "heads-up") && strings.Contains(n, nB) && strings.Contains(n, "/r/a.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want conflict notice naming %s, got %v", nB, notices)
	}
	// No self-conflict on the next write by the same agent.
	notices, _ = s.RecordEvent("/r", "sess-a", protocol.Request{Tool: "Edit", Activity: "Editing a.go",
		Files: []string{"/r/a.go"}, Writes: []string{"/r/a.go"}})
	for _, n := range notices {
		if strings.Contains(n, "heads-up") && strings.Contains(n, "sess-a") {
			t.Fatalf("self-conflict: %v", notices)
		}
	}
}

func TestHousekeepPrunes(t *testing.T) {
	s := open(t)
	now := time.Unix(2000000, 0)
	s.Now = func() time.Time { return now }
	s.Register("/r", "s1", "startup")
	s.RecordEvent("/r", "s1", protocol.Request{Tool: "Edit", Activity: "e", Writes: []string{"/r/x"}})
	now = now.Add(8 * 24 * time.Hour) // past the 7-day event window
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 0 {
		t.Fatalf("events not pruned: %d", n)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM file_touches`).Scan(&n)
	if n != 0 {
		t.Fatalf("file_touches not pruned: %d", n)
	}
}
