package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestMessageHistorySenderAndRecipientSee(t *testing.T) {
	s, nA, nB := twoAgents(t)
	long := "fix landed,\nplease rebase " + strings.Repeat("x", 90)
	if err := s.Send("/r", nA, nB, long); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{nA, nB} {
		h, err := s.MessageHistory("/r", who, "", 0)
		if err != nil || len(h) != 1 {
			t.Fatalf("history for %s: %+v err=%v", who, h, err)
		}
		row := h[0]
		if row.From != nA || row.To != nB || row.ReadAt != 0 || row.Broadcast || row.SentAt == 0 || row.MessageID == 0 {
			t.Fatalf("history row wrong for %s: %+v", who, row)
		}
		if strings.Contains(row.BodyPreview, "\n") || !strings.HasSuffix(row.BodyPreview, "...") ||
			!strings.HasPrefix(row.BodyPreview, "fix landed, please rebase") {
			t.Fatalf("preview must be flattened and clipped: %q", row.BodyPreview)
		}
	}
	// read_at populates after the recipient reads, and history stays intact.
	if _, err := s.Read("/r", nB); err != nil {
		t.Fatal(err)
	}
	h, _ := s.MessageHistory("/r", nA, "", 0)
	if len(h) != 1 || h[0].ReadAt == 0 {
		t.Fatalf("sender must see read_at after read_messages: %+v", h)
	}
}

func TestMessageHistoryPeerFilter(t *testing.T) {
	s, nA, nB := twoAgents(t)
	nC, _ := s.Register("/r", "sess-c", "startup")
	s.Send("/r", nA, nB, "to-b")
	s.Send("/r", nA, nC, "to-c")
	s.Send("/r", nB, nA, "from-b")
	h, err := s.MessageHistory("/r", nA, nB, 0)
	if err != nil || len(h) != 2 {
		t.Fatalf("peer-filtered history: %+v err=%v", h, err)
	}
	if h[0].BodyPreview != "from-b" || h[1].BodyPreview != "to-b" {
		t.Fatalf("want newest-first B exchanges only: %+v", h)
	}
	all, _ := s.MessageHistory("/r", nA, "", 0)
	if len(all) != 3 {
		t.Fatalf("unfiltered history must include all exchanges: %+v", all)
	}
}

func TestMessageHistoryLimitAndCap(t *testing.T) {
	s, nA, nB := twoAgents(t)
	for i := 1; i <= 105; i++ {
		if err := s.Send("/r", nA, nB, fmt.Sprintf("m-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.MessageHistory("/r", nB, "", 0)
	if err != nil || len(h) != 20 || h[0].BodyPreview != "m-105" {
		t.Fatalf("default limit must be 20 newest-first: len=%d err=%v", len(h), err)
	}
	if h, _ = s.MessageHistory("/r", nB, "", 5); len(h) != 5 {
		t.Fatalf("explicit limit not respected: %d", len(h))
	}
	if h, _ = s.MessageHistory("/r", nB, "", 1000); len(h) != 100 {
		t.Fatalf("limit must cap at 100: %d", len(h))
	}
}

func TestMessageHistoryBroadcastFlagged(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if err := s.Broadcast("/r", nA, "release at noon"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{nA, nB} {
		h, err := s.MessageHistory("/r", who, "", 0)
		if err != nil || len(h) != 1 || !h[0].Broadcast || h[0].From != nA || h[0].To != nB {
			t.Fatalf("broadcast row for %s: %+v err=%v", who, h, err)
		}
	}
}
