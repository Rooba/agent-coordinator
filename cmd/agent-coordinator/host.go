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

	"github.com/Rooba/agent-coordinator/internal/hostbroker"
	"github.com/Rooba/agent-coordinator/internal/hostrunner"
)

var openHostCredentialStore = hostbroker.OpenPlatformCredentialStore
var openHostConfigStore = hostbroker.OpenPlatformConfigStore
var openHostState = hostbroker.OpenPlatformState
var manageHostAutostart = hostbroker.ManageAutostart
var currentExecutable = os.Executable

func runHost(args []string) {
	if err := hostCommand(context.Background(), args, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "host: %v\n", err)
		os.Exit(1)
	}
}

func hostCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: agent-coordinator host run|pair|install|uninstall")
	}
	switch args[0] {
	case "pair":
		return hostPair(ctx, args[1:], stdin)
	case "install":
		return hostInstall(ctx, args[1:], stdout)
	case "uninstall":
		return hostUninstall(ctx, args[1:], stdout)
	case "run":
		return hostRun(ctx, args[1:])
	default:
		return fmt.Errorf("unknown host command %q", args[0])
	}
}

func hostPair(ctx context.Context, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("host pair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("file", "", "protected relay-token file; default stdin")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: agent-coordinator host pair [--file PATH]")
	}
	input := stdin
	if *path != "" {
		info, err := os.Lstat(*path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("relay token file must be a regular file")
		}
		file, err := os.Open(*path)
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	}
	store, err := openHostCredentialStore()
	if err != nil {
		return err
	}
	return hostbroker.Pair(ctx, store, input)
}

func hostInstall(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("host install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print the Task Scheduler plan without changing it")
	addr := fs.String("addr", hostbroker.DefaultAddr, "WSL relay numeric loopback address")
	claudeExe := fs.String("claude-exe", "", "absolute Claude executable")
	claudeDir := fs.String("claude-workdir", "", "absolute Claude working directory")
	claudeConfig := fs.String("claude-config-dir", "", "isolated Claude config directory")
	claudeReady := fs.Bool("claude-chrome-ready", false, "assert a manual Claude Chrome smoke probe passed")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: agent-coordinator host install --claude-exe PATH --claude-workdir PATH --claude-config-dir PATH --claude-chrome-ready [--addr IP:PORT] [--dry-run]")
	}
	if !*claudeReady {
		return errors.New("install requires a completed manual Claude Chrome smoke probe")
	}
	config := hostbroker.HostConfig{Version: hostbroker.HostConfigVersion, Addr: *addr, Provider: "claude", Executable: *claudeExe,
		WorkingDir: *claudeDir, ConfigDir: *claudeConfig, BrowserReady: true}
	if err := config.Validate(); err != nil {
		return err
	}
	store, err := openHostCredentialStore()
	if err != nil {
		return err
	}
	if _, err := store.Load(ctx); err != nil {
		return fmt.Errorf("host must be paired before install: %w", err)
	}
	if !*dryRun {
		configs, err := openHostConfigStore()
		if err != nil {
			return err
		}
		if err := configs.Save(ctx, config); err != nil {
			return err
		}
	}
	return scheduleHost(ctx, hostbroker.ScheduleInstall, *dryRun, stdout)
}

func hostUninstall(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("host uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print the Task Scheduler plan without changing it")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: agent-coordinator host uninstall [--dry-run]")
	}
	return scheduleHost(ctx, hostbroker.ScheduleUninstall, *dryRun, stdout)
}

func scheduleHost(ctx context.Context, action hostbroker.ScheduleAction, dryRun bool, stdout io.Writer) error {
	executable, err := currentExecutable()
	if err != nil {
		return err
	}
	plan, err := manageHostAutostart(ctx, action, executable, dryRun)
	if err != nil {
		return err
	}
	if dryRun {
		for _, step := range plan.Steps {
			fmt.Fprintf(stdout, "%s %s\n", plan.Tool, strings.Join(step, " "))
		}
	}
	return nil
}

func hostRun(ctx context.Context, args []string) error {
	store, journal, err := openHostState()
	if err != nil {
		return err
	}
	configs, err := openHostConfigStore()
	if err != nil {
		return err
	}
	config, err := configs.Load(ctx)
	if err != nil {
		return err
	}
	config, err = parseHostRunConfig(args, config)
	if err != nil {
		return err
	}
	provider, err := hostrunner.NewClaudeProvider(hostrunner.ClaudeConfig{
		Executable: config.Executable, WorkingDir: config.WorkingDir, ConfigDir: config.ConfigDir,
	})
	if err != nil {
		return err
	}
	registry, err := hostrunner.NewRegistry(provider)
	if err != nil {
		return err
	}
	runner, err := hostrunner.NewRunner(registry, hostrunner.Options{})
	if err != nil {
		return err
	}
	relay, err := hostbroker.NewClient(config.Addr)
	if err != nil {
		return err
	}
	computer := strings.TrimSpace(os.Getenv("COMPUTERNAME"))
	broker, err := hostbroker.New(relay, store, journal, runner,
		func(context.Context) ([]string, error) { return []string{"provider.claude", "browser.chrome"}, nil },
		hostbroker.Options{ComputerName: computer, DefaultProvider: "claude", AcquireLock: hostbroker.AcquirePlatformLock})
	if err != nil {
		return err
	}
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	err = broker.Run(runCtx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func parseHostRunConfig(args []string, config hostbroker.HostConfig) (hostbroker.HostConfig, error) {
	fs := flag.NewFlagSet("host run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", config.Addr, "WSL relay numeric loopback address")
	claudeExe := fs.String("claude-exe", config.Executable, "absolute Claude executable")
	claudeDir := fs.String("claude-workdir", config.WorkingDir, "absolute Claude working directory")
	claudeConfig := fs.String("claude-config-dir", config.ConfigDir, "isolated Claude config directory")
	claudeReady := fs.Bool("claude-chrome-ready", config.BrowserReady, "assert a manual Claude Chrome smoke probe passed")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return hostbroker.HostConfig{}, errors.New("usage: agent-coordinator host run [Claude provider configuration flags]")
	}
	if !*claudeReady {
		return hostbroker.HostConfig{}, errors.New("no readiness-gated provider configured; complete a manual Claude Chrome smoke probe first")
	}
	config = hostbroker.HostConfig{Version: hostbroker.HostConfigVersion, Addr: *addr, Provider: "claude", Executable: *claudeExe,
		WorkingDir: *claudeDir, ConfigDir: *claudeConfig, BrowserReady: *claudeReady}
	if err := config.Validate(); err != nil {
		return hostbroker.HostConfig{}, err
	}
	return config, nil
}
