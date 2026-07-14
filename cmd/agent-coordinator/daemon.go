package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Rooba/agent-coordinator/internal/daemon"
	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/store"
)

func runDaemon() {
	// Listener first: binding is the single-daemon lock, so a duplicate
	// spawn exits before ever touching the store.
	l, _, err := daemon.Listener()
	if errors.Is(err, daemon.ErrAlreadyServing) {
		return // a peer daemon won the socket; nothing to do
	}
	if err != nil {
		fatal(err)
	}
	dbPath, err := paths.DB()
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fatal(err)
	}
	if err := daemon.Serve(l, st, 10*time.Minute); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "daemon:", err)
	os.Exit(1)
}
