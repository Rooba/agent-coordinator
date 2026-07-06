package main

import (
	"os"

	"github.com/agent-coordinator/go/internal/hookcli"
	"github.com/agent-coordinator/go/internal/paths"
)

func runHook() {
	hookcli.Run(os.Stdin, os.Stdout, paths.Socket())
	os.Exit(0)
}
