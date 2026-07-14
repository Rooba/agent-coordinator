package main

import (
	"fmt"
	"os"

	"github.com/Rooba/agent-coordinator/internal/mcpserv"
	"github.com/Rooba/agent-coordinator/internal/paths"
)

func runMCP() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	if err := mcpserv.Serve(os.Stdin, os.Stdout, paths.Socket(), cwd); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}
