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
