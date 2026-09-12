package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/hostbroker"
	"time"
)

type outputRule struct {
	match string
	reply string
	err   error
}

// eyesFake stands in for every external command and for the network, so the
// whole setup flow runs without touching systemd, Windows, or the internet.
type eyesFake struct {
	appData    string
	appDataWSL string
	unc        string
	rules      []outputRule
	missing    map[string]bool // tools absent from PATH
	tasks      map[string]bool // scheduled tasks Windows reports
	blobs      map[string][]byte
	fetchErrs  map[string]error

	token     string
	tokenMode os.FileMode // 0 means the relay token does not exist

	runs    []string
	queried []string
	fetched []string
	slept   int
}

// fakeInfo states a file's type and permissions directly, so the token checks
// are tested the same way on every host filesystem.
type fakeInfo struct {
	name string
	mode os.FileMode
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() os.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

func (f *eyesFake) lstat(path string) (os.FileInfo, error) {
	if path != f.token {
		return os.Lstat(path)
	}
	if f.tokenMode == 0 {
		return nil, os.ErrNotExist
	}
	return fakeInfo{name: filepath.Base(path), mode: f.tokenMode}, nil
}

func (f *eyesFake) sleep(time.Duration) { f.slept++ }

func (f *eyesFake) key(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func (f *eyesFake) run(name string, args ...string) error {
	f.runs = append(f.runs, f.key(name, args...))
	return nil
}

func (f *eyesFake) output(name string, args ...string) (string, error) {
	if name == "wslpath" && len(args) == 2 {
		if args[0] == "-w" {
			return f.unc, nil
		}
		rest := strings.ReplaceAll(strings.TrimPrefix(args[1], f.appData), `\`, "/")
		return filepath.Join(f.appDataWSL, filepath.FromSlash(rest)), nil
	}
	cmd := f.key(name, args...)
	f.queried = append(f.queried, cmd)
	for _, r := range f.rules {
		if strings.Contains(cmd, r.match) {
			return r.reply, r.err
		}
	}
	if strings.Contains(cmd, "Get-ScheduledTask") {
		return f.scheduledTask(cmd), nil
	}
	return "", fmt.Errorf("no fake for %q", cmd)
}

// scheduledTask answers like Task Scheduler, where -TaskName is a pattern: a
// wildcard therefore also finds another Windows user's task.
func (f *eyesFake) scheduledTask(cmd string) string {
	_, rest, found := strings.Cut(cmd, "-TaskName '")
	if !found {
		return "no"
	}
	pattern, _, _ := strings.Cut(rest, "'")
	for task := range f.tasks {
		if matched, _ := path.Match(pattern, task); matched {
			return "yes"
		}
	}
	return "no"
}

func (f *eyesFake) lookPath(name string) (string, error) {
	if f.missing[name] {
		return "", fmt.Errorf("%s not found", name)
	}
	return "/usr/bin/" + name, nil
}

func (f *eyesFake) fetch(url string) ([]byte, error) {
	f.fetched = append(f.fetched, url)
	if err, exists := f.fetchErrs[url]; exists {
		return nil, err
	}
	if b, exists := f.blobs[url]; exists {
		return b, nil
	}
	return nil, errReleaseNotFound
}

func (f *eyesFake) ran(substr string) bool   { return anyContains(f.runs, substr) }
func (f *eyesFake) asked(substr string) bool { return anyContains(f.queried, substr) }

func anyContains(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

type eyesRig struct {
	t          *testing.T
	root       string
	home       string
	appDataWSL string
	brokerDir  string
	token      string
	dropin     string
	selfExe    string
	fake       *eyesFake
	out        *bytes.Buffer
	setup      *eyesSetup
}

// testSID is the Windows account the fake interop layer reports.
const testSID = "S-1-5-21-1111111111-2222222222-3333333333-1001"

// newEyesRig builds a fully satisfied environment; each test removes exactly
// the piece it is about.
func newEyesRig(t *testing.T) *eyesRig {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home", "tester")
	appData := `C:\Users\tester\AppData\Local`
	appDataWSL := filepath.Join(root, "mnt", "c", "Users", "tester", "AppData", "Local")
	brokerDir := filepath.Join(appDataWSL, "agent-coordinator")
	token := filepath.Join(home, ".local", "state", "agent-coordinator", "relay.token")
	dropin := filepath.Join(home, ".config", "systemd", "user", "agent-coordinator.service.d", "relay.conf")
	claudeExe := appData + `\Programs\claude\claude.exe`

	for _, dir := range []string{
		filepath.Dir(token), filepath.Dir(dropin), brokerDir,
		filepath.Join(brokerDir, "eyes-workdir"), filepath.Join(brokerDir, "claude-config"),
		filepath.Join(root, "selfdir"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeRigFile(t, token, strings.Repeat("a", 64), 0o600)
	writeRigFile(t, dropin, "[Service]\nEnvironment=AC_RELAY_LISTEN=127.0.0.1:7400\n", 0o644)
	writeRigFile(t, filepath.Join(brokerDir, "agent-coordinator.exe"), "MZ", 0o755)

	fake := &eyesFake{
		appData:    appData,
		appDataWSL: appDataWSL,
		unc:        `\\wsl.localhost\Test` + strings.ReplaceAll(token, "/", `\`),
		missing:    map[string]bool{},
		tasks:      map[string]bool{hostbroker.ScheduleTaskName(testSID): true},
		blobs:      map[string][]byte{},
		fetchErrs:  map[string]error{},
		token:      token,
		tokenMode:  0o600,
		rules: []outputRule{
			{match: "Get-Command claude.exe", reply: claudeExe},
			{match: "$env:LOCALAPPDATA", reply: appData},
			{match: "WindowsIdentity", reply: testSID},
			{match: "is-active", reply: "active"},
			{match: "Test-NetConnection", reply: "up"},
			{match: "$env:USERPROFILE", reply: "yes"},
			{match: " version", reply: version},
		},
	}
	rig := &eyesRig{
		t: t, root: root, home: home, appDataWSL: appDataWSL, brokerDir: brokerDir,
		token: token, dropin: dropin, selfExe: filepath.Join(root, "selfdir", "agent-coordinator"),
		fake: fake, out: &bytes.Buffer{},
	}
	rig.setup = &eyesSetup{
		out:         rig.out,
		goos:        "linux",
		home:        home,
		getenv:      func(k string) string { return map[string]string{"WSL_DISTRO_NAME": "Test"}[k] },
		lookPath:    fake.lookPath,
		run:         fake.run,
		output:      fake.output,
		tokenPath:   func() (string, error) { return token, nil },
		executable:  func() (string, error) { return rig.selfExe, nil },
		fetch:       fake.fetch,
		modulePath:  func() string { return "github.com/Rooba/agent-coordinator" },
		lstat:       fake.lstat,
		sleep:       fake.sleep,
		repo:        filepath.Join(root, "checkout"),
		distro:      "Test",
		chromeReady: true,
	}
	if err := rig.setup.setAddr("127.0.0.1:7400"); err != nil {
		t.Fatal(err)
	}
	return rig
}

func writeRigFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// rule puts a higher-priority answer in front of the defaults.
func (r *eyesRig) rule(match, reply string, err error) {
	r.fake.rules = append([]outputRule{{match: match, reply: reply, err: err}}, r.fake.rules...)
}

func (r *eyesRig) run() int {
	r.t.Helper()
	count, err := r.setup.Run()
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return count
}

func (r *eyesRig) report() string { return r.out.String() }

func (r *eyesRig) mustContain(substrs ...string) {
	r.t.Helper()
	for _, s := range substrs {
		if !strings.Contains(r.report(), s) {
			r.t.Fatalf("report missing %q:\n%s", s, r.report())
		}
	}
}

func TestEyesSetupSkipsSatisfiedSteps(t *testing.T) {
	rig := newEyesRig(t)
	if blockedCount := rig.run(); blockedCount != 0 {
		t.Fatalf("blocked=%d, want 0:\n%s", blockedCount, rig.report())
	}
	if len(rig.fake.runs) != 0 {
		t.Fatalf("a fully satisfied environment mutated: %v", rig.fake.runs)
	}
	if strings.Contains(rig.report(), "[done") || strings.Contains(rig.report(), "[plan") {
		t.Fatalf("expected every step already satisfied:\n%s", rig.report())
	}
	rig.mustContain("is active with AC_RELAY_LISTEN", "regular 0600 file", "already paired", "already installed")
}

func TestEyesSetupDryRunMutatesNothing(t *testing.T) {
	rig := newEyesRig(t)
	rig.setup.dryRun = true
	os.Remove(rig.dropin)
	os.RemoveAll(rig.brokerDir)
	rig.rule("Get-ScheduledTask", "no", nil)
	rig.run()

	if len(rig.fake.runs) != 0 {
		t.Fatalf("dry run executed commands: %v", rig.fake.runs)
	}
	if len(rig.fake.fetched) != 0 {
		t.Fatalf("dry run hit the network: %v", rig.fake.fetched)
	}
	for _, path := range []string{rig.dropin, rig.brokerDir, filepath.Join(rig.brokerDir, "agent-coordinator.exe")} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("dry run created %s", path)
		}
	}
	rig.mustContain("dry run, nothing is changed", "[plan", "host pair", "host install", "--claude-chrome-ready")
}

func TestEyesSetupWritesRelayDropInAndRestarts(t *testing.T) {
	rig := newEyesRig(t)
	os.Remove(rig.dropin)
	rig.run()
	body, err := os.ReadFile(rig.dropin)
	if err != nil || !strings.Contains(string(body), "Environment=AC_RELAY_LISTEN=127.0.0.1:7400") {
		t.Fatalf("drop-in not written: %v %q", err, body)
	}
	if !rig.fake.ran("systemctl --user daemon-reload") || !rig.fake.ran("restart agent-coordinator.service") {
		t.Fatalf("systemd not reloaded: %v", rig.fake.runs)
	}
}

func TestEyesSetupWithoutSystemdPrintsManualDaemon(t *testing.T) {
	rig := newEyesRig(t)
	os.Remove(rig.dropin)
	rig.fake.missing["systemctl"] = true
	rig.run()
	rig.mustContain("[note", "AC_RELAY_LISTEN=127.0.0.1:7400 agent-coordinator daemon")
	if rig.fake.ran("systemctl") {
		t.Fatal("ran systemctl without systemd")
	}
}

func TestEyesSetupMissingTokenBlocks(t *testing.T) {
	rig := newEyesRig(t)
	rig.fake.tokenMode = 0
	if blockedCount := rig.run(); blockedCount != 1 {
		t.Fatalf("blocked=%d, want 1:\n%s", blockedCount, rig.report())
	}
	rig.mustContain("is missing", "systemctl --user restart agent-coordinator.service", "[pending")
	if rig.fake.ran("host pair") {
		t.Fatal("paired despite a missing token")
	}
}

func TestEyesSetupTokenModeBlocks(t *testing.T) {
	rig := newEyesRig(t)
	rig.fake.tokenMode = 0o644
	rig.run()
	rig.mustContain("want 0600", "chmod 600 "+rig.token)
	if strings.Contains(rig.report(), strings.Repeat("a", 64)) {
		t.Fatal("the token contents were printed")
	}
}

func TestEyesSetupRefusesOutsideWSL(t *testing.T) {
	rig := newEyesRig(t)
	rig.setup.goos = "darwin"
	if _, err := rig.setup.Run(); err == nil || !strings.Contains(err.Error(), "WSL") {
		t.Fatalf("want a WSL refusal, got %v", err)
	}
}

func TestEyesSetupRefusesWithoutInterop(t *testing.T) {
	rig := newEyesRig(t)
	rig.fake.missing["powershell.exe"] = true
	_, err := rig.setup.Run()
	if err == nil || !strings.Contains(err.Error(), "interop") {
		t.Fatalf("want an interop refusal, got %v", err)
	}
	if len(rig.fake.runs) != 0 {
		t.Fatalf("acted without interop: %v", rig.fake.runs)
	}
}

func TestEyesSetupRequiresChromeReadyBeforeHostInstall(t *testing.T) {
	rig := newEyesRig(t)
	rig.setup.chromeReady = false
	rig.rule("Get-ScheduledTask", "no", nil)
	if blockedCount := rig.run(); blockedCount != 1 {
		t.Fatalf("blocked=%d, want 1:\n%s", blockedCount, rig.report())
	}
	if rig.fake.ran("host install") {
		t.Fatal("host install ran without the Chrome assertion")
	}
	rig.mustContain("smoke probe has not been asserted", "eyes-setup --claude-chrome-ready")
}

func TestEyesSetupConnectivityFailurePrintsNetworkGuidance(t *testing.T) {
	rig := newEyesRig(t)
	rig.rule("Test-NetConnection", "down", nil)
	if blockedCount := rig.run(); blockedCount != 1 {
		t.Fatalf("blocked=%d, want 1:\n%s", blockedCount, rig.report())
	}
	rig.mustContain("networkingMode=mirrored", "wsl --shutdown")
}

func TestEyesSetupClaudeLoginIsHumanOnly(t *testing.T) {
	rig := newEyesRig(t)
	rig.rule("$env:USERPROFILE", "no", nil)
	rig.run()
	rig.mustContain(".credentials.json not found", "complete the browser login")
}

func TestEyesSetupPairsAndInstallsWhenBrokerAbsent(t *testing.T) {
	rig := newEyesRig(t)
	rig.rule("Get-ScheduledTask", "no", nil)
	if blockedCount := rig.run(); blockedCount != 0 {
		t.Fatalf("blocked=%d, want 0:\n%s", blockedCount, rig.report())
	}
	if !rig.fake.ran("host pair --file") || !rig.fake.ran("host install --addr") {
		t.Fatalf("pair/install not invoked: %v", rig.fake.runs)
	}
	if !rig.fake.ran("--claude-chrome-ready") {
		t.Fatalf("host install lost the Chrome assertion: %v", rig.fake.runs)
	}
}

func TestEyesSetupWindowsBinaryOverrideWins(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	override := filepath.Join(rig.root, "downloads", "agent-coordinator.exe")
	writeRigFile(t, override, "MZ", 0o755)
	rig.setup.windowsBinary = override
	rig.run()
	rig.mustContain("--windows-binary override")
	if !rig.fake.ran("cp " + override) {
		t.Fatalf("override not installed: %v", rig.fake.runs)
	}
	if len(rig.fake.fetched) != 0 {
		t.Fatalf("fetched despite an override: %v", rig.fake.fetched)
	}
}

func TestEyesSetupReusesExistingBinaryAndWarnsOnVersionMismatch(t *testing.T) {
	rig := newEyesRig(t)
	rig.rule(" version", "0.9.0", nil)
	rig.run()
	rig.mustContain("already present", "WARNING", "0.9.0", version)
	if len(rig.fake.runs) != 0 || len(rig.fake.fetched) != 0 {
		t.Fatalf("a present binary was replaced: %v %v", rig.fake.runs, rig.fake.fetched)
	}
}

func TestEyesSetupUsesSiblingBinary(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	sibling := filepath.Join(rig.root, "selfdir", "agent-coordinator.exe")
	writeRigFile(t, sibling, "MZ", 0o755)
	rig.run()
	rig.mustContain("sibling of the running binary")
	if len(rig.fake.fetched) != 0 {
		t.Fatalf("fetched despite a sibling binary: %v", rig.fake.fetched)
	}
}

func TestEyesSetupFetchesVersionPinnedRelease(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	rig.fake.missing["go"] = true
	artifact := []byte("MZ-release")
	sum := sha256.Sum256(artifact)
	url := releaseURL(rig.setup.modulePath(), version)
	rig.fake.blobs[url] = artifact
	rig.fake.blobs[url+".sha256"] = []byte(hex.EncodeToString(sum[:]) + "  " + windowsReleaseAsset + "\n")
	rig.run()

	rig.mustContain("source: release "+version, "verified by published sha256")
	if len(rig.fake.fetched) == 0 || !strings.Contains(rig.fake.fetched[0], "/v"+version+"/") {
		t.Fatalf("release fetch is not version pinned: %v", rig.fake.fetched)
	}
	installed, err := os.ReadFile(filepath.Join(rig.brokerDir, "agent-coordinator.exe"))
	if err != nil || !bytes.Equal(installed, artifact) {
		t.Fatalf("artifact not installed: %v %q", err, installed)
	}
}

func TestEyesSetupRejectsFetchedVersionMismatch(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	rig.fake.missing["go"] = true
	rig.fake.blobs[releaseURL(rig.setup.modulePath(), version)] = []byte("MZ-wrong")
	rig.rule(" version", "0.0.1", nil)
	if blockedCount := rig.run(); blockedCount != 1 {
		t.Fatalf("blocked=%d, want 1:\n%s", blockedCount, rig.report())
	}
	rig.mustContain("failed verification", "reports version 0.0.1")
	if _, err := os.Stat(filepath.Join(rig.brokerDir, "agent-coordinator.exe")); err == nil {
		t.Fatal("an unverified artifact was installed")
	}
}

func TestEyesSetupMissingReleaseGivesRemediation(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	rig.fake.missing["go"] = true // no toolchain: the build fallback must not run
	if blockedCount := rig.run(); blockedCount != 1 {
		t.Fatalf("blocked=%d, want 1:\n%s", blockedCount, rig.report())
	}
	rig.mustContain("no release artifact for version "+version, "--windows-binary /path/to/agent-coordinator.exe")
	if rig.fake.ran("make") {
		t.Fatal("built from source without a toolchain")
	}
}

func TestEyesSetupBuildsFromCheckoutWhenFetchFails(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	writeRigFile(t, filepath.Join(rig.setup.repo, "Makefile"), "build-windows:\n", 0o644)
	rig.run()
	if !rig.fake.ran("make -C " + rig.setup.repo + " build-windows") {
		t.Fatalf("source fallback not used: %v", rig.fake.runs)
	}
	rig.mustContain("source: make build-windows")
}

func TestEyesSetupRejectsConfigDirInsideWorkDir(t *testing.T) {
	rig := newEyesRig(t)
	rig.setup.workDir = `C:\eyes`
	rig.setup.configDir = `C:\eyes\config`
	rig.run()
	rig.mustContain("config directory is inside the work directory")
}

func TestEyesSetupReleaseURLFollowsModulePath(t *testing.T) {
	for _, tc := range []struct{ module, want string }{
		{"github.com/example-owner/agent-coordinator", "https://github.com/example-owner/agent-coordinator/releases/download/v" + version + "/" + windowsReleaseAsset},
		{"github.com/example-owner/custom-project", "https://github.com/example-owner/custom-project/releases/download/v" + version + "/" + windowsReleaseAsset},
		{"example.invalid/agent-coordinator", ""},
		{"", ""},
	} {
		if got := releaseURL(tc.module, version); got != tc.want {
			t.Fatalf("releaseURL(%q) = %q, want %q", tc.module, got, tc.want)
		}
	}
}

func TestEyesSetupWithoutModuleInfoSkipsFetch(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	rig.setup.modulePath = func() string { return "" }
	rig.fake.missing["go"] = true
	if blockedCount := rig.run(); blockedCount != 1 {
		t.Fatalf("blocked=%d, want 1:\n%s", blockedCount, rig.report())
	}
	if len(rig.fake.fetched) != 0 {
		t.Fatalf("guessed a repository without module info: %v", rig.fake.fetched)
	}
	rig.mustContain("no agent-coordinator.exe could be resolved", "--windows-binary /path/to/agent-coordinator.exe")
}

func TestEyesSetupVerifiesAgainstParentSHA256SUMS(t *testing.T) {
	rig := newEyesRig(t)
	os.RemoveAll(rig.brokerDir)
	rig.fake.missing["go"] = true
	artifact := []byte("MZ-release")
	sum := sha256.Sum256(artifact)
	release := releaseURL(rig.setup.modulePath(), version)
	sums := "https://github.com/Rooba/agent-coordinator/releases/download/v" + version + "/SHA256SUMS"
	rig.fake.blobs[release] = artifact // no <artifact>.sha256 is published
	rig.fake.blobs[sums] = []byte(hex.EncodeToString(sum[:]) + "  " + windowsReleaseAsset + "\n")
	rig.run()

	rig.mustContain("source: release "+version, "verified by published sha256")
	if !anyContains(rig.fake.fetched, sums) {
		t.Fatalf("SHA256SUMS was not fetched from the release directory: %v", rig.fake.fetched)
	}
}

func TestParseEyesSetupValidatesWithoutActing(t *testing.T) {
	s, err := ParseEyesSetup("eyes-setup", []string{"--dry-run", "--repair", "--addr", "127.0.0.1:7500", "--claude-model", "sonnet"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !s.dryRun || !s.repair || s.addr != "127.0.0.1:7500" || s.addrHost != "127.0.0.1" || s.addrPort != "7500" || s.model != "sonnet" {
		t.Fatalf("parsed the flags wrong: %+v", *s)
	}
	if _, err := ParseEyesSetup("eyes-setup", []string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("--help should report flag.ErrHelp, got %v", err)
	}
	if _, err := ParseEyesSetup("eyes-setup", []string{"stray"}, io.Discard); err == nil {
		t.Fatal("stray arguments were accepted")
	}
}

func TestEyesSetupRejectsBadAddrBeforeMutating(t *testing.T) {
	rig := newEyesRig(t)
	os.Remove(rig.dropin)
	for _, addr := range []string{"localhost:7400", "0.0.0.0:7400", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:00x", "127.0.0.1"} {
		s, err := ParseEyesSetup("eyes-setup", []string{"--addr", addr}, io.Discard)
		if err == nil || s != nil {
			t.Fatalf("--addr %q was accepted", addr)
		}
	}
	if len(rig.fake.runs) != 0 {
		t.Fatalf("a rejected address ran commands: %v", rig.fake.runs)
	}
	if _, err := os.Stat(rig.dropin); err == nil {
		t.Fatal("a rejected address wrote the relay drop-in")
	}
}

func TestEyesSetupQuotesConnectivityProbe(t *testing.T) {
	rig := newEyesRig(t)
	rig.run()
	if !rig.fake.asked("Test-NetConnection '127.0.0.1' -Port '7400'") {
		t.Fatalf("the connectivity probe is not quoted: %v", rig.fake.queried)
	}
}

func TestEyesSetupIgnoresAnotherUsersBrokerTask(t *testing.T) {
	rig := newEyesRig(t)
	rig.fake.tasks = map[string]bool{hostbroker.ScheduleTaskName("S-1-5-21-4444444444-5555555555-6666666666-1002"): true}
	if blockedCount := rig.run(); blockedCount != 0 {
		t.Fatalf("blocked=%d, want 0:\n%s", blockedCount, rig.report())
	}
	if !rig.fake.asked(hostbroker.ScheduleTaskName(testSID)) {
		t.Fatalf("this user's broker task was never queried: %v", rig.fake.queried)
	}
	if !rig.fake.ran("host pair --file") || !rig.fake.ran("host install --addr") {
		t.Fatalf("another user's task made setup skip: %v", rig.fake.runs)
	}
}

func TestEyesSetupNamesTheSettingsItDoesNotRecheck(t *testing.T) {
	rig := newEyesRig(t)
	rig.setup.model = "haiku"
	rig.run()
	rig.mustContain("already installed; its stored settings are NOT read back", "ignored until you re-run with --repair")
	if rig.fake.ran("host install") {
		t.Fatalf("reinstalled a settled broker: %v", rig.fake.runs)
	}
}

func TestEyesSetupRelayListenerRejectsLongerAddress(t *testing.T) {
	rig := newEyesRig(t)
	writeRigFile(t, rig.dropin, "[Service]\nEnvironment=AC_RELAY_LISTEN=127.0.0.1:74001\n", 0o644)
	rig.run()
	body, err := os.ReadFile(rig.dropin)
	if err != nil || !strings.Contains(string(body), "AC_RELAY_LISTEN=127.0.0.1:7400\n") {
		t.Fatalf("a longer address counted as a match: %v %q", err, body)
	}
	if !rig.fake.ran("restart agent-coordinator.service") {
		t.Fatalf("the relay was not restarted: %v", rig.fake.runs)
	}
}

func TestEyesSetupRelayListenerRepairRestarts(t *testing.T) {
	rig := newEyesRig(t)
	rig.setup.repair = true
	rig.run()
	if !rig.fake.ran("systemctl --user daemon-reload") || !rig.fake.ran("restart agent-coordinator.service") {
		t.Fatalf("--repair did not restart the relay: %v", rig.fake.runs)
	}
}

func TestEyesSetupRelayListenerRestartsInactiveService(t *testing.T) {
	rig := newEyesRig(t)
	rig.rule("is-active", "failed", nil)
	rig.run()
	if !rig.fake.ran("restart agent-coordinator.service") {
		t.Fatalf("a matching drop-in alone was taken as a live relay: %v", rig.fake.runs)
	}
	rig.mustContain("is not active", "systemctl --user --no-pager status agent-coordinator.service")
}

func TestEyesSetupWaitsForTheRelayTokenAfterRestart(t *testing.T) {
	rig := newEyesRig(t)
	os.Remove(rig.dropin)
	rig.fake.tokenMode = 0
	rig.run()
	rig.mustContain("has not appeared")
	if rig.fake.slept == 0 {
		t.Fatal("did not wait for the token after restarting the relay")
	}
}
