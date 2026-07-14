package main

import (
	"os"

	"github.com/Rooba/agent-coordinator/internal/hookcli"
	"github.com/Rooba/agent-coordinator/internal/paths"
)

func runHook() {
	hookcli.Run(os.Stdin, os.Stdout, paths.Socket())
	os.Exit(0)
}
