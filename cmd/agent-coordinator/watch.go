package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Rooba/agent-coordinator/internal/hookcli"
	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
	"github.com/Rooba/agent-coordinator/internal/wake"
)

// runWatch is the persistent form of wait for harness monitors: it prints one
// mail line per batch of unread mail for the whole session and exits only on
// SIGTERM/SIGINT or when stdout closes. With no name it binds to the hook
// session and probes by session id, so a recycled name can never redirect it.
func runWatch(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator watch [<name>] [-interval <seconds>] [-quiet <seconds>]")
		os.Exit(2)
	}
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	interval := fs.Int("interval", 1, "seconds between polls")
	quiet := fs.Int("quiet", 1, "seconds to let a burst settle before announcing it")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *interval <= 0 || *quiet <= 0 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sc := scope.Here()
	req := protocol.Request{Scope: sc, From: name, Watcher: true}
	cursorKey := name
	if name == "" {
		b, ok := bindSession(ctx, sc)
		if !ok {
			return
		}
		req.SessionID, cursorKey = b.SessionID, b.SessionID
		fmt.Fprintf(os.Stderr, "watch: bound as %s\n", b.Name)
	}
	statePath, err := waitStatePath(sc, cursorKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		os.Exit(2)
	}
	err = watchMail(ctx, paths.Socket(), statePath, req, os.Stdout, wake.Options{
		Interval: time.Duration(*interval) * time.Second,
		Quiet:    time.Duration(*quiet) * time.Second,
		Log:      func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		os.Exit(1)
	}
}

// watchMail runs the wake loop over the daemon socket; req carries the
// identity every probe sends (an explicit name, or a bound session id).
func watchMail(ctx context.Context, socketPath, statePath string, req protocol.Request, out io.Writer, opts wake.Options) error {
	peek := func(_ context.Context, afterID int64) (wake.Info, error) {
		req.AfterID = afterID
		return peekOnce(socketPath, req)
	}
	return wake.Watch(ctx, peek, fileCursor(statePath), out, opts)
}

// bindSession resolves this session's hook identity the way the MCP shim
// does, retrying because a monitor can start before the SessionStart hook has
// written its bind file. Returns false only when ctx ends first.
func bindSession(ctx context.Context, sc string) (hookcli.Bind, bool) {
	for delay := time.Second; ; delay = min(2*delay, 5*time.Second) {
		if dir, err := paths.BindDir(); err == nil {
			if b, ok := hookcli.MatchBind(dir, sc, hookcli.Ancestry()); ok {
				return b, true
			}
		}
		select {
		case <-ctx.Done():
			return hookcli.Bind{}, false
		case <-time.After(delay):
		}
	}
}

// fileCursor exposes a wait cursor file as a wake.Cursor. An explicit-name
// watch shares the wait cursor for that name; an auto-bound watch keeps its
// own cursor keyed by the bound session.
type fileCursor string

func (c fileCursor) Load() (int64, bool) {
	now := time.Now()
	afterID, ok := loadWaitState(string(c), now)
	if ok {
		_ = os.Chtimes(string(c), now, now)
	}
	return afterID, ok
}

func (c fileCursor) Save(afterID int64) error { return saveWaitState(string(c), afterID) }
