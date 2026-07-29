package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/Rooba/agent-coordinator/internal/installer"
)

func runInstall(args []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	run := func(name string, cargs ...string) error {
		cmd := exec.Command(name, cargs...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if len(args) > 0 && args[0] == "--uninstall" {
		if err := installer.Uninstall(bin, home, run); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("uninstalled")
		return
	}
	if err := installer.Install(bin, home, run); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("installed: harness hooks and MCP registrations; daemon starts on demand (systemd socket unit when available)")
}
