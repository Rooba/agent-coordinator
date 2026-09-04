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
		return hostSchedule(ctx, hostbroker.ScheduleInstall, args[1:], stdout)
	case "uninstall":
		return hostSchedule(ctx, hostbroker.ScheduleUninstall, args[1:], stdout)
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

func hostSchedule(ctx context.Context, action hostbroker.ScheduleAction, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("host "+string(action), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "print the Task Scheduler plan without changing it")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("usage: agent-coordinator host %s [--dry-run]", action)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	plan, err := hostbroker.ManageAutostart(ctx, action, executable, *dryRun)
	if err != nil {
		return err
	}
	if *dryRun {
		for _, step := range plan.Steps {
			fmt.Fprintf(stdout, "%s %s\n", plan.Tool, strings.Join(step, " "))
		}
	}
	return nil
}

func hostRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("host run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", envOr("AC_HOST_ADDR", hostbroker.DefaultAddr), "WSL relay loopback address")
	claudeExe := fs.String("claude-exe", os.Getenv("AC_HOST_CLAUDE_EXE"), "absolute Claude executable")
	claudeDir := fs.String("claude-workdir", os.Getenv("AC_HOST_CLAUDE_WORKDIR"), "absolute Claude working directory")
	claudeConfig := fs.String("claude-config-dir", os.Getenv("AC_HOST_CLAUDE_CONFIG_DIR"), "isolated Claude config directory")
	claudeReady := fs.Bool("claude-chrome-ready", os.Getenv("AC_HOST_CLAUDE_CHROME_READY") == "1", "assert a manual Claude Chrome smoke probe passed")
	codexExe := fs.String("codex-exe", os.Getenv("AC_HOST_CODEX_EXE"), "absolute Codex executable")
	codexDir := fs.String("codex-workdir", os.Getenv("AC_HOST_CODEX_WORKDIR"), "absolute Codex working directory")
	codexConfig := fs.String("codex-config-dir", os.Getenv("AC_HOST_CODEX_CONFIG_DIR"), "isolated Codex config directory")
	codexReady := fs.Bool("codex-browser-ready", os.Getenv("AC_HOST_CODEX_BROWSER_READY") == "1", "assert a manual Codex browser smoke probe passed")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: agent-coordinator host run [provider configuration flags]")
	}

	providers := make([]hostrunner.Provider, 0, 2)
	capabilities := make([]string, 0, 4)
	defaultProvider := ""
	if *claudeReady {
		provider, err := hostrunner.NewClaudeProvider(hostrunner.ClaudeConfig{Executable: *claudeExe, WorkingDir: *claudeDir, ConfigDir: *claudeConfig})
		if err != nil {
			return err
		}
		providers = append(providers, provider)
		capabilities = append(capabilities, "provider.claude", "browser.chrome")
		defaultProvider = "claude"
	}
	if *codexReady {
		provider, err := hostrunner.NewCodexProvider(hostrunner.CodexConfig{Executable: *codexExe, WorkingDir: *codexDir, ConfigDir: *codexConfig})
		if err != nil {
			return err
		}
		providers = append(providers, provider)
		capabilities = append(capabilities, "provider.codex", "browser.chrome")
		if defaultProvider == "" {
			defaultProvider = "codex"
		}
	}
	if len(providers) == 0 {
		return errors.New("no readiness-gated provider configured; complete a manual browser smoke probe first")
	}
	registry, err := hostrunner.NewRegistry(providers...)
	if err != nil {
		return err
	}
	runner, err := hostrunner.NewRunner(registry, hostrunner.Options{})
	if err != nil {
		return err
	}
	relay, err := hostbroker.NewClient(*addr)
	if err != nil {
		return err
	}
	store, journal, err := hostbroker.OpenPlatformState()
	if err != nil {
		return err
	}
	computer := strings.TrimSpace(os.Getenv("COMPUTERNAME"))
	broker, err := hostbroker.New(relay, store, journal, runner,
		func(context.Context) ([]string, error) { return capabilities, nil },
		hostbroker.Options{ComputerName: computer, DefaultProvider: defaultProvider})
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

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
