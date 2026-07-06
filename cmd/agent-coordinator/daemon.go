package main

import (
	"fmt"
	"os"
	"time"

	"github.com/agent-coordinator/go/internal/daemon"
	"github.com/agent-coordinator/go/internal/paths"
	"github.com/agent-coordinator/go/internal/store"
)

func runDaemon() {
	dbPath, err := paths.DB()
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fatal(err)
	}
	l, _, err := daemon.Listener()
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
