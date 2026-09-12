package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/Rooba/agent-coordinator/internal/installer"
)

func splitEyesFlag(args []string) (bool, []string) {
	eyes := false
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--eyes" {
			eyes = true
			continue
		}
		rest = append(rest, arg)
	}
	return eyes, rest
}

type installIntent struct {
	uninstall bool
	dryRun    bool
	setup     *eyesSetup
}

func parseInstallArgs(args []string) (installIntent, error) {
	var in installIntent
	eyes, rest := splitEyesFlag(args)
	uninstall := false
	other := make([]string, 0, len(rest))
	for _, arg := range rest {
		if arg == "--uninstall" {
			uninstall = true
			continue
		}
		other = append(other, arg)
	}
	if uninstall && eyes {
		fmt.Fprintln(os.Stderr, "install: --eyes and --uninstall cannot be combined")
		return in, errors.New("conflicting flags")
	}
	if uninstall {
		if len(other) != 0 {
			fmt.Fprintln(os.Stderr, "usage: agent-coordinator install --uninstall")
			return in, errors.New("unexpected arguments")
		}
		in.uninstall = true
		return in, nil
	}
	if !eyes {
		if len(other) != 0 {
			fmt.Fprintln(os.Stderr, "usage: agent-coordinator install [--uninstall] [--eyes ...]")
			return in, errors.New("unexpected arguments")
		}
		return in, nil
	}
	setup, err := ParseEyesSetup("agent-coordinator install --eyes", other, os.Stderr)
	if err != nil {
		return in, err
	}
	in.setup = setup
	in.dryRun = setup.dryRun
	return in, nil
}

func runInstall(args []string) {
	in, err := parseInstallArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
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
	if in.uninstall {
		if err := installer.Uninstall(bin, home, run); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("uninstalled")
		return
	}
	if in.dryRun {
		fmt.Println("dry-run: would install harness hooks and MCP registrations")
		executeEyesSetup(in.setup)
		return
	}
	if err := installer.Install(bin, home, run); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("installed: harness hooks and MCP registrations; daemon starts on demand (systemd socket unit when available)")
	if in.setup != nil {
		executeEyesSetup(in.setup)
	}
}
