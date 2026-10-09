// Package wake turns an agent's inbox into stdout wake lines: one line per
// batch of newly seen unread mail, suitable for a harness monitor or `wait`.
package wake

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Info is one inbox probe: unread mail above the cursor and the inbox high-water id.
type Info struct {
	Unread    int
	IDs       []int64  // unread ids above afterID, ascending
	Froms     []string // sender per id (same order), de-duplicated by Line
	HighWater int64    // highest id ever delivered to this inbox
}

// Peek probes the inbox for unread mail with id > afterID. Errors mean "daemon unreachable".
type Peek func(ctx context.Context, afterID int64) (Info, error)

// Cursor persists the consumed high-water id across restarts (the wait cursor file).
type Cursor interface {
	Load() (afterID int64, ok bool)
	Save(afterID int64) error
}

// Options tunes the poll loop; zero values take the defaults noted per field.
type Options struct {
	Interval time.Duration                              // poll period; default 1s
	Quiet    time.Duration                              // coalescing window after first new id; default 1s
	Now      func() time.Time                           // default time.Now
	Sleep    func(ctx context.Context, d time.Duration) // default context-aware sleep
	Log      func(format string, args ...any)           // stderr diagnostics; default discard
}

const (
	defaultInterval = time.Second
	defaultQuiet    = time.Second
	minBackoff      = time.Second
	maxBackoff      = 10 * time.Second
)

// Line formats one announcement exactly like `wait` prints: "mail from=a,b count=N ids=1,2,3".
func Line(info Info) string {
	var froms []string
	seen := map[string]bool{}
	for _, f := range info.Froms {
		if !seen[f] {
			seen[f] = true
			froms = append(froms, f)
		}
	}
	from := strings.Join(froms, ",")
	if from == "" {
		from = "?"
	}
	ids := make([]string, len(info.IDs))
	for i, id := range info.IDs {
		ids[i] = strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("mail from=%s count=%d ids=%s", from, info.Unread, strings.Join(ids, ","))
}

// Watch polls the inbox until ctx is done, writing one Line per batch of newly
// seen unread ids to out; an unchanged batch is never re-announced. A new id
// opens a quiet window; polling continues and the line goes out once the
// window has passed, covering everything unread at that moment. Without a
// stored cursor it starts at 0 so everything still unread is announced; the
// cursor advances to HighWater only once nothing above it is unread. Peek
// failures back off (1s..10s) and print nothing. Returns ctx.Err() on cancel
// or the first write error on out.
func Watch(ctx context.Context, peek Peek, cursor Cursor, out io.Writer, opts Options) error {
	opts = opts.withDefaults()
	afterID, ok := cursor.Load()
	if !ok {
		afterID = 0
		if err := cursor.Save(afterID); err != nil {
			return fmt.Errorf("save arm state: %w", err)
		}
	}
	lastNotified := afterID
	var pendingSince time.Time // first sighting of the batch not yet announced
	var backoff time.Duration
	for ctx.Err() == nil {
		info, err := peek(ctx, afterID)
		if err != nil {
			if backoff == 0 {
				opts.Log("watch: daemon unreachable, retrying: %v", err)
				backoff = minBackoff
			} else {
				backoff = min(2*backoff, maxBackoff)
			}
			opts.Sleep(ctx, backoff)
			continue
		}
		backoff = 0
		switch top := maxID(info.IDs); {
		case top <= lastNotified:
			pendingSince = time.Time{}
		case pendingSince.IsZero():
			pendingSince = opts.Now()
		case opts.Now().Sub(pendingSince) >= opts.Quiet:
			if _, err := fmt.Fprintln(out, Line(info)); err != nil {
				return err
			}
			lastNotified, pendingSince = top, time.Time{}
		}
		if info.Unread == 0 && info.HighWater > afterID {
			afterID = info.HighWater
			if err := cursor.Save(afterID); err != nil {
				return fmt.Errorf("advance consumed state: %w", err)
			}
		}
		opts.Sleep(ctx, opts.Interval)
	}
	return ctx.Err()
}

func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.Quiet <= 0 {
		o.Quiet = defaultQuiet
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	return o
}

// sleep waits for d or until ctx is done, whichever comes first.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func maxID(ids []int64) int64 {
	var top int64
	for _, id := range ids {
		top = max(top, id)
	}
	return top
}
