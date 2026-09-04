package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/hostbroker"
)

type hostTestCredentials struct {
	value hostbroker.Credential
	saved hostbroker.Credential
	err   error
}

func (s *hostTestCredentials) Load(context.Context) (hostbroker.Credential, error) {
	return s.value, s.err
}

func (s *hostTestCredentials) Update(_ context.Context, update func(*hostbroker.Credential) error) error {
	credential := s.value
	if s.err != nil && !errors.Is(s.err, hostbroker.ErrCredentialNotFound) {
		return s.err
	}
	if err := update(&credential); err != nil {
		return err
	}
	s.value, s.saved, s.err = credential, credential, nil
	return nil
}

type hostTestConfigStore struct {
	saved hostbroker.HostConfig
	saves int
}

func (s *hostTestConfigStore) Load(context.Context) (hostbroker.HostConfig, error) {
	return s.saved, nil
}

func (s *hostTestConfigStore) Save(_ context.Context, config hostbroker.HostConfig) error {
	s.saved = config
	s.saves++
	return nil
}

func TestHostRunConfigUsesStoredValuesAndExplicitOverrides(t *testing.T) {
	executable, _ := os.Executable()
	stored := hostbroker.HostConfig{Version: hostbroker.HostConfigVersion, Addr: hostbroker.DefaultAddr, Provider: "claude", Executable: executable,
		WorkingDir: t.TempDir(), ConfigDir: t.TempDir(), BrowserReady: true}
	got, err := parseHostRunConfig(nil, stored)
	if err != nil || got != stored {
		t.Fatalf("stored config = (%+v, %v)", got, err)
	}
	override := t.TempDir()
	got, err = parseHostRunConfig([]string{"--claude-workdir", override}, stored)
	if err != nil || got.WorkingDir != override || got.ConfigDir != stored.ConfigDir || got.Executable != stored.Executable {
		t.Fatalf("overridden config = (%+v, %v)", got, err)
	}
}

func TestHostInstallDryRunAndUnpairedAreSideEffectFree(t *testing.T) {
	executable, _ := os.Executable()
	baseArgs := []string{"install", "--claude-exe", executable, "--claude-workdir", t.TempDir(),
		"--claude-config-dir", t.TempDir(), "--claude-chrome-ready"}
	for _, test := range []struct {
		name       string
		credential error
		dryRun     bool
		wantManage bool
	}{
		{name: "unpaired", credential: hostbroker.ErrCredentialNotFound},
		{name: "dry run", dryRun: true, wantManage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credentials := &hostTestCredentials{value: hostbroker.Credential{Token: strings.Repeat("t", 64), LauncherSession: "launcher-fixed"}, err: test.credential}
			configs := &hostTestConfigStore{}
			previousStore, previousConfigs, previousManage, previousExecutable := openHostCredentialStore, openHostConfigStore, manageHostAutostart, currentExecutable
			openHostCredentialStore = func() (hostbroker.CredentialStore, error) { return credentials, nil }
			openHostConfigStore = func() (hostbroker.ConfigStore, error) { return configs, nil }
			currentExecutable = func() (string, error) { return executable, nil }
			managed := false
			manageHostAutostart = func(context.Context, hostbroker.ScheduleAction, string, bool) (hostbroker.SchedulePlan, error) {
				managed = true
				return hostbroker.SchedulePlan{}, nil
			}
			defer func() {
				openHostCredentialStore, openHostConfigStore, manageHostAutostart, currentExecutable = previousStore, previousConfigs, previousManage, previousExecutable
			}()
			args := append([]string(nil), baseArgs...)
			if test.dryRun {
				args = append(args, "--dry-run")
			}
			err := hostCommand(context.Background(), args, strings.NewReader(""), &bytes.Buffer{})
			if (err != nil) != (test.credential != nil) || managed != test.wantManage || configs.saves != 0 {
				t.Fatalf("install = (err %v, managed %v, config saves %d)", err, managed, configs.saves)
			}
		})
	}
}

func TestHostPairRejectsAndNeverPrintsTokens(t *testing.T) {
	store := &hostTestCredentials{err: hostbroker.ErrCredentialNotFound}
	previous := openHostCredentialStore
	openHostCredentialStore = func() (hostbroker.CredentialStore, error) { return store, nil }
	defer func() { openHostCredentialStore = previous }()

	for _, token := range []string{"secret token", strings.Repeat("x", 64)} {
		var stdout bytes.Buffer
		err := hostCommand(context.Background(), []string{"pair"}, strings.NewReader(token), &stdout)
		if stdout.Len() != 0 || err != nil && strings.Contains(err.Error(), token) {
			t.Fatalf("pair leaked token: stdout=%q err=%v", stdout.String(), err)
		}
		if token == "secret token" && err == nil {
			t.Fatal("invalid token accepted")
		}
		if len(token) == 64 && (err != nil || store.saved.Token != token) {
			t.Fatalf("valid pair = (%+v, %v)", store.saved, err)
		}
	}
}

func TestHostInstallPersistsRunnableClaudeConfigBeforeScheduling(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workingDir, configDir := t.TempDir(), t.TempDir()
	store := &hostTestCredentials{value: hostbroker.Credential{
		Token: strings.Repeat("t", 64), LauncherSession: "launcher-fixed", SessionSecret: strings.Repeat("s", 64),
	}}
	configs := &hostTestConfigStore{}
	previousStore, previousConfigs, previousManage, previousExecutable := openHostCredentialStore, openHostConfigStore, manageHostAutostart, currentExecutable
	openHostCredentialStore = func() (hostbroker.CredentialStore, error) { return store, nil }
	openHostConfigStore = func() (hostbroker.ConfigStore, error) { return configs, nil }
	currentExecutable = func() (string, error) { return executable, nil }
	called := false
	manageHostAutostart = func(_ context.Context, action hostbroker.ScheduleAction, binary string, dryRun bool) (hostbroker.SchedulePlan, error) {
		called = action == hostbroker.ScheduleInstall && binary == executable && !dryRun
		return hostbroker.SchedulePlan{}, nil
	}
	defer func() {
		openHostCredentialStore, openHostConfigStore, manageHostAutostart, currentExecutable = previousStore, previousConfigs, previousManage, previousExecutable
	}()
	args := []string{"install", "--claude-exe", executable, "--claude-workdir", workingDir,
		"--claude-config-dir", configDir, "--claude-chrome-ready"}
	if err := hostCommand(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !called || configs.saved.Executable != executable || configs.saved.WorkingDir != workingDir || configs.saved.ConfigDir != configDir || !configs.saved.BrowserReady {
		t.Fatalf("install did not persist runnable config: called=%v config=%+v", called, configs.saved)
	}
}
