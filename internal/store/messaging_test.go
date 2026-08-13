package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
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

func TestUnreadCountIsReadOnly(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if n, err := s.UnreadCount("/r", nB); err != nil || n != 0 {
		t.Fatalf("want 0 unread before send, got %d (%v)", n, err)
	}
	if err := s.Send("/r", nA, nB, "ping"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("/r", nB); n != 1 {
		t.Fatalf("want 1 unread after send, got %d", n)
	}
	if n, _ := s.UnreadCount("/r", nB); n != 1 {
		t.Fatalf("second count must not consume anything, got %d", n)
	}
	// Peeking must not have consumed the once-only notice either.
	if notices, err := s.PendingNotices("/r", "sess-b"); err != nil || len(notices) != 1 {
		t.Fatalf("peek consumed the nudge: %v (%v)", notices, err)
	}
	if _, err := s.Read("/r", nB); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("/r", nB); n != 0 {
		t.Fatalf("want 0 unread after read, got %d", n)
	}
	if _, err := s.UnreadCount("/r", "nobody-here"); err == nil {
		t.Fatal("want error for unknown agent")
	}
}

// Stale backlog must not count as "new mail" once wait arms on HighWater.
func TestPeekMailAfterIDIgnoresStaleUnread(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "old-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("/r", nA, nB, "old-2"); err != nil {
		t.Fatal(err)
	}
	base, err := s.PeekMail("/r", nB, 0)
	if err != nil || base.Unread != 2 || base.HighWater == 0 {
		t.Fatalf("baseline: %+v err=%v", base, err)
	}
	// Arming at HighWater: only messages with id > HighWater should appear.
	stale, err := s.PeekMail("/r", nB, base.HighWater)
	if err != nil || stale.Unread != 0 {
		t.Fatalf("stale after high water must be empty: %+v err=%v", stale, err)
	}
	if err := s.Send("/r", nA, nB, "fresh"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.PeekMail("/r", nB, base.HighWater)
	if err != nil || fresh.Unread != 1 || len(fresh.IDs) != 1 {
		t.Fatalf("want one fresh message: %+v err=%v", fresh, err)
	}
	if fresh.IDs[0] <= base.HighWater {
		t.Fatalf("fresh id %d not above baseline %d", fresh.IDs[0], base.HighWater)
	}
	if len(fresh.Froms) != 1 || fresh.Froms[0] != nA {
		t.Fatalf("want from %s, got %v", nA, fresh.Froms)
	}
	// HighWater advances after the new delivery.
	if fresh.HighWater <= base.HighWater {
		t.Fatalf("high water should advance: base=%d fresh=%d", base.HighWater, fresh.HighWater)
	}
}

func TestNoticeCarriesIDAndPreview(t *testing.T) {
	s, nA, nB := twoAgents(t)
	long := "fix landed on main,\nplease rebase your branch " + strings.Repeat("x", 80)
	if err := s.Send("/r", nA, nB, long); err != nil {
		t.Fatal(err)
	}
	notices, err := s.PendingNotices("/r", "sess-b")
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices: %v err=%v", notices, err)
	}
	n := notices[0]
	var id int64
	s.db.QueryRow(`SELECT MAX(id) FROM messages`).Scan(&id)
	for _, want := range []string{nA, "1 new message", "(ids " + joinIDs([]int64{id}) + ")", `"fix landed on main, please rebase`, `..." - call read_messages`} {
		if !strings.Contains(n, want) {
			t.Fatalf("notice missing %q: %s", want, n)
		}
	}
	if strings.Contains(n, "\n") {
		t.Fatalf("newlines must be flattened: %q", n)
	}
	if strings.Contains(n, strings.Repeat("x", 80)) {
		t.Fatalf("long body must be clipped to ~80 chars: %q", n)
	}
}

func TestNoticeShortBodyUntruncated(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "short and sweet"); err != nil {
		t.Fatal(err)
	}
	notices, _ := s.PendingNotices("/r", "sess-b")
	if len(notices) != 1 || !strings.Contains(notices[0], `"short and sweet"`) || strings.Contains(notices[0], "...") {
		t.Fatalf("short body must appear whole and unclipped: %v", notices)
	}
}

func TestNoticeAggregatesIDsAndPreviewsNewest(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "older body"); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("/r", nA, nB, "newest body"); err != nil {
		t.Fatal(err)
	}
	notices, _ := s.PendingNotices("/r", "sess-b")
	if len(notices) != 1 {
		t.Fatalf("want one aggregated notice: %v", notices)
	}
	var lo, hi int64
	s.db.QueryRow(`SELECT MIN(id), MAX(id) FROM messages`).Scan(&lo, &hi)
	want := "(ids " + joinIDs([]int64{lo, hi}) + ")"
	if !strings.Contains(notices[0], "2 new messages") || !strings.Contains(notices[0], want) ||
		!strings.Contains(notices[0], `"newest body"`) {
		t.Fatalf("aggregate notice wrong (want %s + newest preview): %v", want, notices)
	}
}

func TestBroadcastNoticeCarriesPreview(t *testing.T) {
	s, nA, _ := twoAgents(t)
	if err := s.Broadcast("/r", nA, "release at noon"); err != nil {
		t.Fatal(err)
	}
	notices, _ := s.PendingNotices("/r", "sess-b")
	if len(notices) != 1 || !strings.Contains(notices[0], "broadcast from "+nA) ||
		!strings.Contains(notices[0], `"release at noon"`) {
		t.Fatalf("broadcast notice must carry the preview: %v", notices)
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

// Mail from a purged sender must stay visible everywhere - peek, notices, and
// read - labeled with the raw sender id once the name is gone.
func TestMailFromPurgedSenderSurvives(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "still here"); err != nil {
		t.Fatal(err)
	}
	senderID := agentID("sess-a")
	if _, err := s.db.Exec(`DELETE FROM agents WHERE scope='/r' AND session_id='sess-a'`); err != nil {
		t.Fatal(err)
	}
	peek, err := s.PeekMail("/r", nB, 0)
	if err != nil || peek.Unread != 1 || len(peek.Froms) != 1 || peek.Froms[0] != senderID {
		t.Fatalf("peek after sender purge: %+v err=%v", peek, err)
	}
	notices, err := s.PendingNotices("/r", "sess-b")
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0], senderID) {
		t.Fatalf("notices after sender purge: %v err=%v", notices, err)
	}
	msgs, err := s.Read("/r", nB)
	if err != nil || len(msgs) != 1 || msgs[0].Body != "still here" || msgs[0].From != senderID {
		t.Fatalf("read after sender purge: %+v err=%v", msgs, err)
	}
}

// Housekeep drops delivery rows whose recipient was purged, and only those.
func TestHousekeepPurgesDeliveriesOfPurgedRecipients(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Send("/r", nA, nB, "to-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("/r", nB, nA, "to-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM agents WHERE scope='/r' AND session_id='sess-b'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	count := func(aid string) (n int) {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE agent_id=?`, aid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(agentID("sess-b")); n != 0 {
		t.Fatalf("purged recipient keeps %d deliveries", n)
	}
	if n := count(agentID("sess-a")); n != 1 {
		t.Fatalf("live recipient deliveries: %d", n)
	}
}
