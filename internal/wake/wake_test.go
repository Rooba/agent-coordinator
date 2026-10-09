package wake

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// scriptedPeek hands out snapshots in order and repeats the last one forever.
// A nil entry stands for a daemon-unreachable error.
type scriptedPeek struct {
	snaps    []*Info
	calls    int
	afterIDs []int64
}

func (p *scriptedPeek) peek(_ context.Context, afterID int64) (Info, error) {
	i := min(p.calls, len(p.snaps)-1)
	p.calls++
	p.afterIDs = append(p.afterIDs, afterID)
	if p.snaps[i] == nil {
		return Info{}, errors.New("dial: connection refused")
	}
	return *p.snaps[i], nil
}

type memCursor struct {
	id    int64
	ok    bool
	saves []int64
}

func (c *memCursor) Load() (int64, bool) { return c.id, c.ok }
func (c *memCursor) Save(id int64) error {
	c.id, c.ok = id, true
	c.saves = append(c.saves, id)
	return nil
}

// drive runs Watch on a logical clock: each Sleep advances the clock by its
// duration and the context is cancelled once the sleep budget is spent.
func drive(peek *scriptedPeek, cursor *memCursor, out io.Writer, quiet time.Duration, sleeps int) ([]time.Duration, []string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Unix(100, 0)
	var slept []time.Duration
	var logs []string
	err := Watch(ctx, peek.peek, cursor, out, Options{
		Interval: time.Second,
		Quiet:    quiet,
		Now:      func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) {
			now = now.Add(d)
			slept = append(slept, d)
			if len(slept) >= sleeps {
				cancel()
			}
		},
		Log: func(format string, args ...any) { logs = append(logs, format) },
	})
	return slept, logs, err
}

func runWatch(t *testing.T, peek *scriptedPeek, cursor *memCursor, sleeps int) (string, []time.Duration, []string) {
	t.Helper()
	var out strings.Builder
	slept, logs, err := drive(peek, cursor, &out, time.Second, sleeps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch returned %v, want context.Canceled", err)
	}
	return out.String(), slept, logs
}

func unread(hw int64, ids []int64, froms ...string) *Info {
	return &Info{Unread: len(ids), IDs: ids, Froms: froms, HighWater: hw}
}

func consumed(hw int64) *Info { return &Info{HighWater: hw} }

func TestWakeLineMatchesWaitFormat(t *testing.T) {
	got := Line(Info{Unread: 3, IDs: []int64{4, 5, 6}, Froms: []string{"a", "b", "a"}})
	if want := "mail from=a,b count=3 ids=4,5,6"; got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
	if got := Line(Info{Unread: 1, IDs: []int64{9}}); got != "mail from=? count=1 ids=9" {
		t.Fatalf("Line without senders = %q", got)
	}
}

func TestWakeRepeatSnapshotIsSilent(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{unread(5, []int64{5}, "a")}}
	out, slept, _ := runWatch(t, peek, &memCursor{id: 4, ok: true}, 6)
	if want := "mail from=a count=1 ids=5\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if len(slept) != 6 || slept[0] != time.Second {
		t.Fatalf("sleeps = %v, want six poll intervals", slept)
	}
}

func TestWakeNewIDAfterConsumeWakesAgain(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{
		unread(5, []int64{5}, "a"),
		unread(5, []int64{5}, "a"),
		consumed(5),
		unread(6, []int64{6}, "b"),
	}}
	cursor := &memCursor{id: 4, ok: true}
	out, _, _ := runWatch(t, peek, cursor, 8)
	if want := "mail from=a count=1 ids=5\nmail from=b count=1 ids=6\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if cursor.id != 5 || len(cursor.saves) != 1 {
		t.Fatalf("cursor = %d saves = %v, want 5 saved once and 6 left unread", cursor.id, cursor.saves)
	}
}

func TestWakeNewIDWhilePriorUnreadPrintsOnce(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{
		unread(5, []int64{5}, "a"),
		unread(5, []int64{5}, "a"),
		unread(7, []int64{5, 6, 7}, "a", "b", "a"),
	}}
	out, _, _ := runWatch(t, peek, &memCursor{id: 4, ok: true}, 8)
	if want := "mail from=a count=1 ids=5\nmail from=a,b count=3 ids=5,6,7\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestWakeQuietWindowCoalescesBurst(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{
		unread(5, []int64{5}, "a"),
		unread(6, []int64{5, 6}, "a", "a"),
		unread(7, []int64{5, 6, 7}, "a", "a", "b"),
	}}
	var out strings.Builder
	if _, _, err := drive(peek, &memCursor{id: 4, ok: true}, &out, 2*time.Second, 6); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if want := "mail from=a,b count=3 ids=5,6,7\n"; out.String() != want {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
}

func TestWakeMailReadDuringQuietWindowStaysSilent(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{unread(5, []int64{5}, "a"), consumed(5)}}
	cursor := &memCursor{id: 4, ok: true}
	out, _, _ := runWatch(t, peek, cursor, 4)
	if out != "" || cursor.id != 5 {
		t.Fatalf("out = %q cursor = %d, want silence and cursor 5", out, cursor.id)
	}
}

func TestWakeRestartReannouncesUnreadAboveCursor(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{unread(9, []int64{8, 9}, "a", "b")}}
	cursor := &memCursor{id: 7, ok: true}
	out, _, _ := runWatch(t, peek, cursor, 4)
	if want := "mail from=a,b count=2 ids=8,9\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if peek.afterIDs[0] != 7 || cursor.id != 7 {
		t.Fatalf("afterIDs = %v cursor = %d", peek.afterIDs, cursor.id)
	}
}

func TestWakeMissingCursorAnnouncesEverythingUnread(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{unread(3, []int64{2, 3}, "a", "a")}}
	cursor := &memCursor{}
	out, _, _ := runWatch(t, peek, cursor, 4)
	if want := "mail from=a count=2 ids=2,3\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if peek.afterIDs[0] != 0 || len(cursor.saves) != 1 || cursor.saves[0] != 0 {
		t.Fatalf("afterIDs = %v saves = %v, want a zero baseline", peek.afterIDs, cursor.saves)
	}
}

func TestWakeCursorAdvancesOnlyWhenNothingUnread(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{
		unread(5, []int64{5}, "a"),
		unread(5, []int64{5}, "a"),
		unread(5, []int64{5}, "a"),
		consumed(5),
	}}
	cursor := &memCursor{id: 4, ok: true}
	runWatch(t, peek, cursor, 6)
	if len(cursor.saves) != 1 || cursor.saves[0] != 5 {
		t.Fatalf("saves = %v, want one save of 5 after the mail was consumed", cursor.saves)
	}
	if peek.afterIDs[len(peek.afterIDs)-1] != 5 {
		t.Fatalf("afterIDs = %v, want later polls above the advanced cursor", peek.afterIDs)
	}
}

func TestWakePeekErrorsPrintNothingAndBackOff(t *testing.T) {
	peek := &scriptedPeek{snaps: []*Info{nil, nil, nil, nil, nil, unread(5, []int64{5}, "a")}}
	out, slept, logs := runWatch(t, peek, &memCursor{id: 4, ok: true}, 8)
	if want := "mail from=a count=1 ids=5\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, time.Second}
	for i, d := range want {
		if slept[i] != d {
			t.Fatalf("sleeps = %v, want backoff prefix %v", slept, want)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want exactly one note per outage", logs)
	}
}

func TestWakeCancelReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	peek := &scriptedPeek{snaps: []*Info{consumed(0)}}
	err := Watch(ctx, peek.peek, &memCursor{ok: true}, &strings.Builder{}, Options{})
	if !errors.Is(err, context.Canceled) || peek.calls != 0 {
		t.Fatalf("Watch = %v after %d peeks, want context.Canceled before polling", err, peek.calls)
	}
}

type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWakeWriteErrorEndsWatch(t *testing.T) {
	closed := errors.New("stdout closed")
	peek := &scriptedPeek{snaps: []*Info{unread(5, []int64{5}, "a")}}
	cursor := &memCursor{id: 4, ok: true}
	_, _, err := drive(peek, cursor, failWriter{closed}, time.Second, 10)
	if !errors.Is(err, closed) || cursor.id != 4 {
		t.Fatalf("Watch = %v cursor = %d, want the write error with the cursor untouched", err, cursor.id)
	}
}
