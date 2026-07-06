package main

import (
	"fmt"
	"os"
)

var version = "2.0.0-dev"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "daemon":
		runDaemon()
	case "hook":
		runHook()
	case "mcp":
		runMCP()
	case "install":
		runInstall(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator daemon|hook|mcp|install [--uninstall]|version")
		os.Exit(2)
	}
}

// Stubs, replaced by later tasks. Delete each when its real file lands.
func runHook()                 { os.Exit(0) } // fail-open even as a stub
func runMCP()                  { fmt.Fprintln(os.Stderr, "not implemented"); os.Exit(1) }
func runInstall(args []string) { fmt.Fprintln(os.Stderr, "not implemented"); os.Exit(1) }
