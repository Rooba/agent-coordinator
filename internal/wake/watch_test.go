package wake

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type contractCursor struct {
	after int64
	valid bool
	saved []int64
	err   error
}

func (c *contractCursor) Load() (int64, bool) { return c.after, c.valid }

func (c *contractCursor) Save(after int64) error {
	if c.err != nil {
		return c.err
	}
	c.after, c.valid = after, true
	c.saved = append(c.saved, after)
	return nil
}

func contractMail(highWater int64, ids ...int64) Info {
	froms := make([]string, len(ids))
	for i := range froms {
		froms[i] = "peer-fox"
	}
	return Info{
		Unread: len(ids), IDs: ids, Froms: froms, HighWater: highWater,
	}
}

// A finite sequence of snapshots drives the real loop with a logical clock.
// Exhausting the sequence cancels the monitor instead of imposing real sleeps.
func contractWatch(t *testing.T, cursor *contractCursor, out io.Writer, quiet time.Duration, probes ...Info) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Unix(100, 0)
	index, sleeps := 0, 0
	peek := func(ctx context.Context, after int64) (Info, error) {
		if index == len(probes) {
			cancel()
			return Info{}, ctx.Err()
		}
		probe := probes[index]
		index++
		for _, id := range probe.IDs {
			if id <= after {
				t.Fatalf("probe %d: cursor advanced past unread id %d: after=%d", index, id, after)
			}
		}
		return probe, nil
	}
	return Watch(ctx, peek, cursor, out, Options{
		Interval: time.Second,
		Quiet:    quiet,
		Now:      func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) {
			sleeps++
			if sleeps > 10*(len(probes)+1) {
				t.Fatal("monitor did not terminate after cancellation")
			}
			now = now.Add(d)
		},
	})
}

func contractCanceled(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watch returned %v, want context cancellation", err)
	}
}

func TestContractWatchReadAndArrivalBetweenPolls(t *testing.T) {
	cursor := &contractCursor{after: 10, valid: true}
	var out strings.Builder
	err := contractWatch(t, cursor, &out, time.Second,
		contractMail(11, 11), contractMail(11, 11), contractMail(11, 11),
		// Message 11 was read and message 12 arrived before the next poll.
		// No empty snapshot separates these two batches.
		contractMail(12, 12), contractMail(12, 12), contractMail(12, 12),
		contractMail(12),
		contractMail(13, 13), contractMail(13, 13),
		contractMail(13),
	)
	contractCanceled(t, err)
	want := "mail from=peer-fox count=1 ids=11\n" +
		"mail from=peer-fox count=1 ids=12\n" +
		"mail from=peer-fox count=1 ids=13\n"
	if out.String() != want {
		t.Fatalf("successive mail batches: got %q, want %q", out.String(), want)
	}
	if cursor.after != 13 {
		t.Fatalf("consumed mail must advance durable cursor to 13, got %d", cursor.after)
	}
	for _, after := range cursor.saved {
		if after != 12 && after != 13 {
			t.Fatalf("checkpointed unread mail: saved cursors %v", cursor.saved)
		}
	}
}

func TestContractWatchRestartRedeliversUnreadOnly(t *testing.T) {
	cursor := &contractCursor{after: 10, valid: true}
	for attempt := 0; attempt < 2; attempt++ {
		var out strings.Builder
		err := contractWatch(t, cursor, &out, time.Second,
			contractMail(11, 11), contractMail(11, 11), contractMail(11, 11),
		)
		contractCanceled(t, err)
		if want := "mail from=peer-fox count=1 ids=11\n"; out.String() != want {
			t.Fatalf("restart %d must redeliver unread mail once: got %q", attempt, out.String())
		}
		if cursor.after != 10 {
			t.Fatalf("restart %d checkpointed unread mail: after=%d", attempt, cursor.after)
		}
	}
	var out strings.Builder
	contractCanceled(t, contractWatch(t, cursor, &out, time.Second, contractMail(11), contractMail(11)))
	if out.Len() != 0 || cursor.after != 11 {
		t.Fatalf("consumed mail must advance quietly: output=%q after=%d", out.String(), cursor.after)
	}
	contractCanceled(t, contractWatch(t, cursor, &out, time.Second, contractMail(11), contractMail(11)))
	if out.Len() != 0 {
		t.Fatalf("restart must not announce consumed mail: %q", out.String())
	}
}

func TestContractWatchCancelPendingBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cursor := &contractCursor{after: 10, valid: true}
	var out strings.Builder
	err := Watch(ctx, func(context.Context, int64) (Info, error) {
		return contractMail(11, 11), nil
	}, cursor, &out, Options{
		Interval: time.Second,
		Quiet:    time.Hour,
		Sleep:    func(context.Context, time.Duration) { cancel() },
	})
	contractCanceled(t, err)
	if out.Len() != 0 || cursor.after != 10 {
		t.Fatalf("cancelled batch must remain unread and unannounced: output=%q after=%d", out.String(), cursor.after)
	}
}

func TestContractWatchCheckpointFailureStops(t *testing.T) {
	saveErr := errors.New("cursor storage unavailable")
	cursor := &contractCursor{after: 10, valid: true, err: saveErr}
	var out strings.Builder
	err := contractWatch(t, cursor, &out, time.Second, contractMail(11))
	if !errors.Is(err, saveErr) {
		t.Fatalf("checkpoint failure must terminate watch: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("checkpoint failure emitted a notification: %q", out.String())
	}
}
