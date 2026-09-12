package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/hostbroker"
	"github.com/Rooba/agent-coordinator/internal/paths"
)

// errReleaseNotFound marks the expected case of a release artifact that is not
// published or not readable (the repository is private).
var errReleaseNotFound = errors.New("release artifact not found")

// windowsReleaseAsset is the Windows build published by the release workflow.
// Only windows/amd64 is published, so there is no architecture to choose.
const windowsReleaseAsset = "agent-coordinator_windows_amd64.exe"

// releaseURL locates an exact release version using the binary's module path.
// User input never reaches it. An empty result means no GitHub release can be
// addressed, so the caller must resolve the binary another way.
func releaseURL(module, v string) string {
	parts := strings.Split(module, "/")
	if len(parts) < 3 || parts[0] != "github.com" || parts[1] == "" || parts[2] == "" || v == "" {
		return ""
	}
	return "https://github.com/" + parts[1] + "/" + parts[2] + "/releases/download/v" + v + "/" + windowsReleaseAsset
}

// mainModulePath is the module this binary was built from, or "" when it was
// built without module information.
func mainModulePath() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Path
}

// State of one setup step as the report prints it.
const (
	stateOK      = "ok"      // already satisfied, nothing to do
	stateDone    = "done"    // this run performed the change
	statePlan    = "plan"    // dry run: this is what would happen
	stateNote    = "note"    // degraded but not fatal; the manual path is printed
	stateBlocked = "blocked" // only a human can clear this
	statePending = "pending" // not attempted because an earlier step is blocked
)

type stepResult struct {
	state  string
	detail string
	next   string // the exact command or action a human must perform
}

func ok(format string, a ...any) (stepResult, error) {
	return stepResult{state: stateOK, detail: fmt.Sprintf(format, a...)}, nil
}

func done(format string, a ...any) (stepResult, error) {
	return stepResult{state: stateDone, detail: fmt.Sprintf(format, a...)}, nil
}

func note(detail, next string) (stepResult, error) {
	return stepResult{state: stateNote, detail: detail, next: next}, nil
}

func blocked(detail, next string) (stepResult, error) {
	return stepResult{state: stateBlocked, detail: detail, next: next}, nil
}

// eyesSetup collapses the hand-run WSL <-> Windows eyes bridge setup into one
// command. Every step detects its own state before acting, so re-running is
// always safe and the command doubles as a diagnostic.
type eyesSetup struct {
	out        io.Writer
	goos       string
	home       string
	getenv     func(string) string
	lookPath   func(string) (string, error)
	run        func(string, ...string) error
	output     func(string, ...string) (string, error)
	tokenPath  func() (string, error)
	executable func() (string, error)
	fetch      func(string) ([]byte, error)
	modulePath func() string
	lstat      func(string) (os.FileInfo, error)
	sleep      func(time.Duration)

	dryRun        bool
	repair        bool
	chromeReady   bool
	addr          string
	addrHost      string
	addrPort      string
	repo          string
	distro        string
	winUser       string
	windowsBinary string
	claudeExe     string
	workDir       string
	configDir     string
	model         string

	// Filled in as the steps discover them.
	appData         string // Windows form of %LOCALAPPDATA%
	appDataWSL      string // the same directory under /mnt/c
	winExe          string // Windows path of the copied agent-coordinator.exe
	token           string // WSL path of relay.token
	unc             string // Windows UNC path of relay.token
	brokerTask      string // scheduled task name of this Windows user's broker
	brokerInstalled bool
	binaryFresh     bool // the broker binary was (re)built or copied this run
}

const eyesSetupFlags = "[--dry-run] [--claude-chrome-ready] [--repair] [--addr IP:PORT] [--windows-binary PATH] [--repo PATH] [--distro NAME] [--windows-user NAME] [--claude-exe PATH] [--claude-workdir PATH] [--claude-config-dir PATH] [--claude-model ALIAS]"

// newEyesSetup wires the real environment into a setup that has done nothing yet.
func newEyesSetup() *eyesSetup {
	return &eyesSetup{
		out:        os.Stdout,
		goos:       runtime.GOOS,
		getenv:     os.Getenv,
		lookPath:   exec.LookPath,
		tokenPath:  paths.RelayTokenPath,
		executable: os.Executable,
		fetch:      httpFetch,
		modulePath: mainModulePath,
		lstat:      os.Lstat,
		sleep:      time.Sleep,
		run: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		},
		output: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).Output()
			return strings.TrimSpace(string(out)), err
		},
	}
}

// ParseEyesSetup turns eyes-setup flags into a fully validated setup without
// touching the machine, so callers can reject bad input before anything runs.
// name is the command as the user typed it; --help returns flag.ErrHelp.
func ParseEyesSetup(name string, args []string, errOut io.Writer) (*eyesSetup, error) {
	s := newEyesSetup()
	addr := ""
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.BoolVar(&s.dryRun, "dry-run", false, "print the full plan without changing anything")
	fs.BoolVar(&s.chromeReady, "claude-chrome-ready", false, "assert the manual Claude Chrome smoke probe passed")
	fs.BoolVar(&s.repair, "repair", false, "re-pair and re-install even when the broker task already exists")
	fs.StringVar(&addr, "addr", "127.0.0.1:7400", "loopback address the WSL relay listens on")
	fs.StringVar(&s.windowsBinary, "windows-binary", "", "agent-coordinator.exe to install for the Windows broker (default: resolved automatically)")
	fs.StringVar(&s.repo, "repo", ".", "source checkout used to build the Windows binary when nothing else resolves")
	fs.StringVar(&s.distro, "distro", "", "WSL distro name (default $WSL_DISTRO_NAME)")
	fs.StringVar(&s.winUser, "windows-user", "", "Windows user whose LOCALAPPDATA hosts the broker (default the interop user)")
	fs.StringVar(&s.claudeExe, "claude-exe", "", "absolute Windows path of claude.exe (default discovered)")
	fs.StringVar(&s.workDir, "claude-workdir", "", "Windows working directory for eyes turns (default %LOCALAPPDATA%\\agent-coordinator\\eyes-workdir)")
	fs.StringVar(&s.configDir, "claude-config-dir", "", "isolated Windows Claude config directory, outside the work directory (default %LOCALAPPDATA%\\agent-coordinator\\claude-config)")
	fs.StringVar(&s.model, "claude-model", "", "Claude model alias for eyes turns (default the broker default)")
	err := fs.Parse(args)
	if err == nil && fs.NArg() != 0 {
		err = errors.New("unexpected arguments")
	}
	if err == nil {
		err = s.setAddr(addr)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(errOut, "%v\nusage: %s %s\n", err, name, eyesSetupFlags)
		}
		return nil, err
	}
	return s, nil
}

// setAddr records the relay address only once it passes the same loopback rule
// the relay client enforces, so a rejected address changes nothing.
func (s *eyesSetup) setAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		_, err = hostbroker.NewClient(addr)
	}
	if err != nil {
		return fmt.Errorf("--addr %q: %w", addr, err)
	}
	s.addr, s.addrHost, s.addrPort = addr, host, port
	return nil
}

func runEyesSetup(args []string) {
	s, err := ParseEyesSetup("agent-coordinator eyes-setup", args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	executeEyesSetup(s)
}

// executeEyesSetup runs an already validated setup and maps it onto exit codes.
func executeEyesSetup(s *eyesSetup) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "eyes-setup:", err)
		os.Exit(1)
	}
	s.home = home
	blockedSteps, err := s.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "eyes-setup:", err)
		os.Exit(1)
	}
	if blockedSteps > 0 {
		os.Exit(1)
	}
}

// Run executes every step in order and reports each one. A blocked step ends a
// live run - the steps after it depend on it - but a dry run keeps going so the
// whole plan is visible at once.
func (s *eyesSetup) Run() (int, error) {
	steps := []struct {
		name string
		fn   func() (stepResult, error)
	}{
		{"environment", s.stepEnvironment},
		{"relay-listener", s.stepRelayListener},
		{"relay-token", s.stepRelayToken},
		{"windows-binary", s.stepWindowsBinary},
		{"token-unc-path", s.stepTokenUNC},
		{"broker-task", s.stepBrokerTask},
		{"pair", s.stepPair},
		{"connectivity", s.stepConnectivity},
		{"claude-exe", s.stepClaudeExe},
		{"claude-login", s.stepClaudeLogin},
		{"work-dirs", s.stepWorkDirs},
		{"chrome-ready", s.stepChromeReady},
		{"host-install", s.stepHostInstall},
	}
	mode := "applying changes"
	if s.dryRun {
		mode = "dry run, nothing is changed"
	}
	fmt.Fprintf(s.out, "eyes-setup: %s\n", mode)
	count := 0
	for i, step := range steps {
		result, err := step.fn()
		if err != nil {
			return count, err
		}
		s.report(step.name, result)
		if result.state != stateBlocked {
			continue
		}
		count++
		if !s.dryRun {
			for _, pending := range steps[i+1:] {
				s.report(pending.name, stepResult{state: statePending, detail: "waiting on the blocked step above"})
			}
			break
		}
	}
	if count > 0 {
		fmt.Fprintf(s.out, "\n%d step(s) need you; clear them and re-run eyes-setup.\n", count)
	} else if !s.dryRun {
		fmt.Fprintln(s.out, "\neyes bridge ready.")
	}
	return count, nil
}

func (s *eyesSetup) report(name string, r stepResult) {
	fmt.Fprintf(s.out, "  [%-7s] %-15s %s\n", r.state, name, r.detail)
	for _, line := range strings.Split(strings.TrimRight(r.next, "\n"), "\n") {
		if line != "" {
			fmt.Fprintf(s.out, "                            %s\n", line)
		}
	}
}

// act reports what a dry run would do instead of doing it.
func (s *eyesSetup) act(plan string) (stepResult, bool) {
	if s.dryRun {
		return stepResult{state: statePlan, detail: plan}, false
	}
	return stepResult{}, true
}

// --- steps ---------------------------------------------------------------

// stepEnvironment refuses anything that is not WSL with Windows interop: every
// later step drives the Windows side through powershell.exe.
func (s *eyesSetup) stepEnvironment() (stepResult, error) {
	if s.goos != "linux" {
		return stepResult{}, fmt.Errorf("eyes-setup runs in a WSL Linux distro; this is %s", s.goos)
	}
	if s.distro == "" {
		s.distro = s.getenv("WSL_DISTRO_NAME")
	}
	if s.distro == "" {
		if b, err := os.ReadFile("/proc/version"); err != nil || !strings.Contains(strings.ToLower(string(b)), "microsoft") {
			return stepResult{}, errors.New("this does not look like WSL: WSL_DISTRO_NAME is unset and /proc/version has no microsoft marker")
		}
	}
	if _, err := s.lookPath("powershell.exe"); err != nil {
		return stepResult{}, errors.New("powershell.exe is not on PATH: enable WSL interop (/etc/wsl.conf [interop] enabled=true, appendWindowsPath=true) and restart the distro with `wsl --shutdown`")
	}
	distro := s.distro
	if distro == "" {
		distro = "unknown"
	}
	return ok("WSL distro %s, powershell.exe interop available", distro)
}

// stepRelayListener installs the systemd drop-in that turns the opt-in relay
// listener on and makes sure the daemon is actually running with it. Without
// systemd it prints the foreground command instead.
func (s *eyesSetup) stepRelayListener() (stepResult, error) {
	dir := filepath.Join(s.home, ".config", "systemd", "user", "agent-coordinator.service.d")
	conf := filepath.Join(dir, "relay.conf")
	want := "[Service]\nEnvironment=AC_RELAY_LISTEN=" + s.addr + "\n"
	manual := fmt.Sprintf("without systemd, run the daemon in its own terminal:\n  AC_RELAY_LISTEN=%s agent-coordinator daemon", s.addr)
	b, err := os.ReadFile(conf)
	if err != nil && !os.IsNotExist(err) {
		return stepResult{}, err
	}
	if _, err := s.lookPath("systemctl"); err != nil {
		return note("systemctl not found; the drop-in alone has no effect", manual)
	}
	if dropInAddr(string(b)) == s.addr && !s.repair && s.serviceActive() {
		return ok("agent-coordinator.service is active with AC_RELAY_LISTEN=%s (%s)", s.addr, conf)
	}
	if plan, act := s.act(fmt.Sprintf("write %s with AC_RELAY_LISTEN=%s, then systemctl --user daemon-reload + restart agent-coordinator.service", conf, s.addr)); !act {
		return plan, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return stepResult{}, err
	}
	if err := os.WriteFile(conf, []byte(want), 0o644); err != nil {
		return stepResult{}, err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "restart", "agent-coordinator.service"}} {
		if err := s.run("systemctl", args...); err != nil {
			return note("wrote "+conf+" but systemctl failed", manual)
		}
	}
	if !s.serviceActive() {
		return note("wrote "+conf+" but agent-coordinator.service is not active", "read the failure:\n  systemctl --user --no-pager status agent-coordinator.service")
	}
	if path, err := s.tokenPath(); err == nil && !s.waitForToken(path) {
		return note("restarted agent-coordinator.service but "+path+" has not appeared", "confirm the listener bound:\n  systemctl --user --no-pager status agent-coordinator.service")
	}
	return done("wrote %s and restarted agent-coordinator.service on %s", conf, s.addr)
}

// dropInAddr returns the address a drop-in sets AC_RELAY_LISTEN to, or "".
func dropInAddr(dropIn string) string {
	for _, line := range strings.Split(dropIn, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "Environment=AC_RELAY_LISTEN="); found {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// serviceActive asks systemd whether the daemon is running right now.
func (s *eyesSetup) serviceActive() bool {
	out, _ := s.output("systemctl", "--user", "is-active", "agent-coordinator.service")
	return strings.TrimSpace(out) == "active"
}

// waitForToken gives a just-restarted daemon a bounded moment to mint the relay
// token, which systemd does not wait for before returning.
func (s *eyesSetup) waitForToken(path string) bool {
	for range 20 {
		if info, err := s.lstat(path); err == nil && info.Mode().IsRegular() {
			return true
		}
		s.sleep(100 * time.Millisecond)
	}
	return false
}

// stepRelayToken checks the shared secret the daemon mints once the TCP
// listener binds. The file is never read or printed - only its type and mode.
func (s *eyesSetup) stepRelayToken() (stepResult, error) {
	path, err := s.tokenPath()
	if err != nil {
		return stepResult{}, err
	}
	s.token = path
	restart := fmt.Sprintf("the daemon mints the token only after the relay listener binds:\n  systemctl --user restart agent-coordinator.service\n  (or) AC_RELAY_LISTEN=%s agent-coordinator daemon", s.addr)
	info, err := s.lstat(path)
	switch {
	case os.IsNotExist(err):
		return blocked(path+" is missing", restart)
	case err != nil:
		return stepResult{}, err
	case !info.Mode().IsRegular():
		return blocked(path+" is not a regular file", "rm "+path+" and restart the daemon")
	case info.Mode().Perm() != 0o600:
		return blocked(fmt.Sprintf("%s has mode %04o, want 0600", path, info.Mode().Perm()), "chmod 600 "+path)
	}
	return ok("%s is a regular 0600 file (contents never read)", path)
}

// stepWindowsBinary puts agent-coordinator.exe in the Windows user's
// LOCALAPPDATA. Local copies are preferred in order; only when none exists is
// the release for this exact version fetched, and building from source is the
// last resort for a checkout with a Go toolchain.
func (s *eyesSetup) stepWindowsBinary() (stepResult, error) {
	if err := s.resolveAppData(); err != nil {
		return stepResult{}, err
	}
	destDir := filepath.Join(s.appDataWSL, "agent-coordinator")
	dest := filepath.Join(destDir, "agent-coordinator.exe")
	s.winExe = s.appData + `\agent-coordinator\agent-coordinator.exe`

	if s.windowsBinary != "" {
		if info, err := os.Stat(s.windowsBinary); err != nil || !info.Mode().IsRegular() {
			return blocked("--windows-binary "+s.windowsBinary+" is not a regular file", "pass the path of an existing agent-coordinator.exe with --windows-binary")
		}
		return s.copyWindowsBinary(s.windowsBinary, dest, "--windows-binary override")
	}
	if info, err := os.Stat(dest); err == nil && info.Mode().IsRegular() {
		r, _ := ok("%s already present (source: previous install)", s.winExe)
		r.next = s.versionWarning(dest)
		return r, nil
	}
	if sibling := s.siblingExe(); sibling != "" {
		return s.copyWindowsBinary(sibling, dest, "sibling of the running binary")
	}
	if r, handled, err := s.fetchWindowsBinary(dest); handled {
		return r, err
	}
	if src := s.buildableSource(); src != "" {
		if plan, act := s.act(fmt.Sprintf("make -C %s build-windows, then install %s", s.repo, s.winExe)); !act {
			s.binaryFresh = true
			return plan, nil
		}
		if err := s.run("make", "-C", s.repo, "build-windows"); err != nil {
			return blocked("make build-windows failed in "+s.repo, "build it by hand, then re-run:\n  agent-coordinator eyes-setup --windows-binary "+src)
		}
		return s.copyWindowsBinary(src, dest, "make build-windows")
	}
	return blocked("no agent-coordinator.exe could be resolved",
		"supply one of these, then re-run eyes-setup:\n"+
			"  agent-coordinator eyes-setup --windows-binary /path/to/agent-coordinator.exe\n"+
			"  (or) from a source checkout with a Go toolchain: make build-windows")
}

// copyWindowsBinary installs a resolved local binary and names where it came
// from, warning when its version does not match this build.
func (s *eyesSetup) copyWindowsBinary(src, dest, source string) (stepResult, error) {
	warning := s.versionWarning(src)
	if plan, act := s.act(fmt.Sprintf("install %s from %s (%s)", s.winExe, src, source)); !act {
		s.binaryFresh = true
		plan.next = warning
		return plan, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return stepResult{}, err
	}
	if err := s.run("cp", src, dest); err != nil {
		return stepResult{}, err
	}
	s.binaryFresh = true
	r, _ := done("installed %s (source: %s)", s.winExe, source)
	r.next = warning
	return r, nil
}

// fetchWindowsBinary downloads the release built from this exact version. The
// URL is constructed in code from that version alone, never from input, and
// the artifact is verified before it is installed. handled is false when the
// caller should fall through to building from source.
func (s *eyesSetup) fetchWindowsBinary(dest string) (result stepResult, handled bool, err error) {
	artifact := releaseURL(s.modulePath(), version)
	if artifact == "" {
		return stepResult{}, false, nil // no module information: never guess a repository
	}
	if s.dryRun {
		s.binaryFresh = true
		return stepResult{state: statePlan, detail: "fetch " + artifact + " and install " + s.winExe}, true, nil
	}
	body, ferr := s.fetch(artifact)
	if ferr != nil {
		if s.buildableSource() != "" {
			return stepResult{}, false, nil // a checkout can build it instead
		}
		detail := fmt.Sprintf("no release artifact for version %s", version)
		if !errors.Is(ferr, errReleaseNotFound) {
			detail = fmt.Sprintf("fetching %s failed: %v", artifact, ferr)
		}
		r, _ := blocked(detail, "download or build agent-coordinator.exe yourself, then re-run:\n  agent-coordinator eyes-setup --windows-binary /path/to/agent-coordinator.exe")
		return r, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return stepResult{}, true, err
	}
	staged := dest + ".download"
	if err := os.WriteFile(staged, body, 0o755); err != nil {
		return stepResult{}, true, err
	}
	how, verr := s.verifyArtifact(artifact, body, staged)
	if verr != nil {
		os.Remove(staged)
		r, _ := blocked("the fetched artifact failed verification: "+verr.Error(), "install a trusted build yourself:\n  agent-coordinator eyes-setup --windows-binary /path/to/agent-coordinator.exe")
		return r, true, nil
	}
	if err := os.Rename(staged, dest); err != nil {
		os.Remove(staged)
		return stepResult{}, true, err
	}
	s.binaryFresh = true
	r, _ := done("installed %s (source: release %s, %s)", s.winExe, version, how)
	return r, true, nil
}

// verifyArtifact checks a downloaded artifact as far as the release allows: a
// published checksum proves authenticity, while a version self-report only
// proves the build matches - with no checksum, integrity rests on HTTPS alone.
func (s *eyesSetup) verifyArtifact(artifact string, body []byte, staged string) (string, error) {
	sum := hex.EncodeToString(sha256Sum(body))
	for _, sums := range []string{artifact + ".sha256", siblingURL(artifact, "SHA256SUMS")} {
		if sums == "" {
			continue
		}
		published, err := s.fetch(sums)
		if err != nil {
			continue
		}
		if !strings.Contains(strings.ToLower(string(published)), sum) {
			return "", fmt.Errorf("sha256 %s is not listed in %s", sum, sums)
		}
		return "verified by published sha256", nil
	}
	got := s.exeVersion(staged)
	if got == "" {
		return "", errors.New("no published checksum and the artifact would not report its version")
	}
	if got != version {
		return "", fmt.Errorf("artifact reports version %s, want %s", got, version)
	}
	return "version self-report only, no published checksum", nil
}

// siblingExe finds agent-coordinator.exe shipped next to the running binary,
// the usual layout of a downloaded release.
func (s *eyesSetup) siblingExe() string {
	self, err := s.executable()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(self), "agent-coordinator.exe")
	if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
		return candidate
	}
	return ""
}

// buildableSource returns the build output path when this is a source checkout
// with a Go toolchain - the development-only fallback.
func (s *eyesSetup) buildableSource() string {
	if _, err := os.Stat(filepath.Join(s.repo, "Makefile")); err != nil {
		return ""
	}
	for _, tool := range []string{"go", "make"} {
		if _, err := s.lookPath(tool); err != nil {
			return ""
		}
	}
	return filepath.Join(s.repo, "agent-coordinator.exe")
}

// versionWarning names both versions when a reused Windows binary is not this
// build: a stale broker against a newer daemon fails in confusing ways.
func (s *eyesSetup) versionWarning(path string) string {
	got := s.exeVersion(path)
	switch {
	case got == "":
		return "WARNING: could not read the version of " + path
	case got != version:
		return fmt.Sprintf("WARNING: %s is version %s but this build is %s; replace it with\n  agent-coordinator eyes-setup --windows-binary <matching agent-coordinator.exe>", path, got, version)
	}
	return ""
}

func (s *eyesSetup) exeVersion(path string) string {
	out, err := s.output(path, "version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(out, "\r", ""))
}

// siblingURL addresses a file published next to an artifact, so the checksum
// list is found whatever the artifact itself is named.
func siblingURL(artifact, name string) string {
	u, err := url.Parse(artifact)
	if err != nil || u.Path == "" {
		return ""
	}
	u.Path = path.Dir(u.Path) + "/" + name
	return u.String()
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// httpFetch reads a release artifact. A 404 is an ordinary outcome here - the
// repository may be private - and is reported as such, not as a raw error.
func httpFetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errReleaseNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// stepTokenUNC derives the \\wsl.localhost path the Windows broker reads the
// token from, so the secret never travels through a command line.
func (s *eyesSetup) stepTokenUNC() (stepResult, error) {
	if out, err := s.output("wslpath", "-w", s.token); err == nil && strings.HasPrefix(out, `\\`) {
		s.unc = out
		return ok("%s", s.unc)
	}
	if s.distro == "" {
		return blocked("wslpath did not return a UNC path and the distro name is unknown", "pass --distro <DistroName> (see `wsl -l -q` in PowerShell)")
	}
	s.unc = `\\wsl.localhost\` + s.distro + strings.ReplaceAll(s.token, "/", `\`)
	return ok("%s (derived from $WSL_DISTRO_NAME)", s.unc)
}

// stepBrokerTask looks for this Windows user's own broker task, so another
// user's leftover task never makes pairing and installation skip.
func (s *eyesSetup) stepBrokerTask() (stepResult, error) {
	sid, err := s.powershell(`[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value`)
	if err != nil || !strings.HasPrefix(sid, "S-1-") {
		return note("could not read the current Windows SID; assuming the broker is not installed", "")
	}
	s.brokerTask = hostbroker.ScheduleTaskName(sid)
	out, err := s.powershell(fmt.Sprintf(`if (Get-ScheduledTask -TaskPath '\' -TaskName %s -ErrorAction SilentlyContinue) { 'yes' } else { 'no' }`, psQuote(s.brokerTask)))
	if err != nil {
		return note("could not query Task Scheduler; assuming the broker is not installed", "")
	}
	s.brokerInstalled = out == "yes"
	if s.brokerInstalled {
		return ok("broker task %s exists for this Windows user", s.brokerTask)
	}
	return ok("no broker task %s for this Windows user yet", s.brokerTask)
}

func (s *eyesSetup) stepPair() (stepResult, error) {
	if s.settled() {
		return ok("already paired (broker task %s exists); re-run with --repair to pair again", s.brokerTask)
	}
	script := fmt.Sprintf("& %s host pair --file %s", psQuote(s.winExe), psQuote(s.unc))
	if plan, act := s.act("powershell.exe -Command " + script); !act {
		return plan, nil
	}
	if err := s.runPowershell(script); err != nil {
		return blocked("host pair failed", "run this in a Windows PowerShell window as the Claude user:\n  "+script)
	}
	return done("paired the Windows broker from the protected token file")
}

// stepConnectivity proves Windows can reach the WSL loopback listener, which
// depends on WSL localhost forwarding or mirrored networking.
func (s *eyesSetup) stepConnectivity() (stepResult, error) {
	probe := fmt.Sprintf("Test-NetConnection %s -Port %s", psQuote(s.addrHost), psQuote(s.addrPort))
	guidance := "Windows cannot reach the WSL relay. Fix WSL networking, then re-run:\n" +
		"  add to C:\\Users\\<WindowsUser>\\.wslconfig:\n    [wsl2]\n    networkingMode=mirrored\n" +
		"  then run `wsl --shutdown` in PowerShell and start the distro again.\n" +
		"  verify with: " + probe
	out, err := s.powershell(fmt.Sprintf("if (%s -InformationLevel Quiet) { 'up' } else { 'down' }", probe))
	if err != nil || out != "up" {
		return blocked(probe+" did not succeed", guidance)
	}
	return ok("Windows reaches %s", s.addr)
}

func (s *eyesSetup) stepClaudeExe() (stepResult, error) {
	if s.claudeExe != "" {
		return ok("using --claude-exe %s", s.claudeExe)
	}
	out, err := s.powershell(`$c = Get-Command claude.exe -ErrorAction SilentlyContinue; ` +
		`if ($c) { $c.Source } else { @("$env:LOCALAPPDATA\Programs\claude\claude.exe","$env:LOCALAPPDATA\Programs\Claude\claude.exe") | Where-Object { Test-Path $_ } | Select-Object -First 1 }`)
	if err != nil || out == "" {
		return blocked("claude.exe not found on the Windows side", "install Claude for Windows, or pass --claude-exe 'C:\\absolute\\path\\to\\claude.exe'")
	}
	s.claudeExe = out
	return ok("discovered %s", s.claudeExe)
}

// stepClaudeLogin only detects the isolated broker's source of credentials.
// Signing in is an interactive browser flow and is never automated here.
func (s *eyesSetup) stepClaudeLogin() (stepResult, error) {
	out, err := s.powershell(`if (Test-Path (Join-Path $env:USERPROFILE '.claude\.credentials.json')) { 'yes' } else { 'no' }`)
	if err != nil || out != "yes" {
		return blocked("%USERPROFILE%\\.claude\\.credentials.json not found", "sign in once on Windows (interactive, human only):\n  & "+psQuote(s.claudeExe)+"\n  complete the browser login, then re-run eyes-setup")
	}
	return ok("Windows Claude credentials present for the broker to mirror")
}

// stepWorkDirs creates the eyes working directory and the dedicated Claude
// config directory, which host install requires to live outside it.
func (s *eyesSetup) stepWorkDirs() (stepResult, error) {
	if s.workDir == "" {
		s.workDir = s.appData + `\agent-coordinator\eyes-workdir`
	}
	if s.configDir == "" {
		s.configDir = s.appData + `\agent-coordinator\claude-config`
	}
	if within(s.configDir, s.workDir) {
		return blocked("the Claude config directory is inside the work directory", "pass --claude-config-dir pointing outside "+s.workDir)
	}
	var made, present []string
	for _, dir := range []string{s.workDir, s.configDir} {
		wsl, err := s.toWSLPath(dir)
		if err != nil {
			return blocked("cannot map "+dir+" into WSL", "create it in PowerShell:\n  New-Item -ItemType Directory -Force "+psQuote(dir))
		}
		if info, err := os.Stat(wsl); err == nil && info.IsDir() {
			present = append(present, dir)
			continue
		}
		if s.dryRun {
			made = append(made, dir)
			continue
		}
		if err := os.MkdirAll(wsl, 0o755); err != nil {
			return stepResult{}, err
		}
		made = append(made, dir)
	}
	switch {
	case len(made) == 0:
		return ok("%s and %s exist", present[0], present[1])
	case s.dryRun:
		return stepResult{state: statePlan, detail: "create " + strings.Join(made, " and ")}, nil
	}
	return done("created %s", strings.Join(made, " and "))
}

// stepChromeReady gates host install on the human's explicit assertion: the
// coordinator deliberately never probes Chrome itself.
func (s *eyesSetup) stepChromeReady() (stepResult, error) {
	if s.chromeReady {
		return ok("--claude-chrome-ready asserted by you")
	}
	return blocked("the manual Claude Chrome smoke probe has not been asserted",
		"run this once in a Windows PowerShell window and confirm Claude can inspect the intended Chrome profile:\n"+
			"  $env:CLAUDE_CONFIG_DIR="+psQuote(s.configDir)+"; & "+psQuote(s.claudeExe)+" --chrome\n"+
			"then re-run: agent-coordinator eyes-setup --claude-chrome-ready")
}

func (s *eyesSetup) stepHostInstall() (stepResult, error) {
	if s.settled() {
		r, _ := ok("broker task %s already installed; its stored settings are NOT read back", s.brokerTask)
		r.next = "--addr, --claude-model, --claude-workdir and --claude-config-dir given now are ignored until you re-run with --repair"
		return r, nil
	}
	script := fmt.Sprintf("& %s host install --addr %s --claude-exe %s --claude-workdir %s --claude-config-dir %s",
		psQuote(s.winExe), psQuote(s.addr), psQuote(s.claudeExe), psQuote(s.workDir), psQuote(s.configDir))
	if s.model != "" {
		script += " --claude-model " + psQuote(s.model)
	}
	script += " --claude-chrome-ready"
	if plan, act := s.act("powershell.exe -Command " + script); !act {
		return plan, nil
	}
	if err := s.runPowershell(script); err != nil {
		return blocked("host install failed", "run it yourself in a Windows PowerShell window and read the error:\n  "+script)
	}
	return done("installed and started the Windows broker scheduled task")
}

// --- helpers -------------------------------------------------------------

// settled reports whether pairing and installation can be left alone: the
// broker task exists, the binary did not change, and no repair was asked for.
func (s *eyesSetup) settled() bool {
	return s.brokerInstalled && !s.binaryFresh && !s.repair
}

// resolveAppData finds the Windows LOCALAPPDATA of the broker's user in both
// its Windows and its /mnt/c form.
func (s *eyesSetup) resolveAppData() error {
	if s.appData != "" {
		return nil
	}
	if s.winUser != "" {
		s.appData = `C:\Users\` + s.winUser + `\AppData\Local`
	} else {
		out, err := s.powershell("$env:LOCALAPPDATA")
		if err != nil || out == "" {
			return errors.New("could not read %LOCALAPPDATA% through interop; pass --windows-user <WindowsUser>")
		}
		s.appData = out
	}
	wsl, err := s.toWSLPath(s.appData)
	if err != nil {
		return fmt.Errorf("wslpath -u %s: %w", s.appData, err)
	}
	s.appDataWSL = wsl
	return nil
}

func (s *eyesSetup) toWSLPath(windows string) (string, error) {
	out, err := s.output("wslpath", "-u", windows)
	if err != nil || out == "" {
		return "", fmt.Errorf("wslpath -u %s failed", windows)
	}
	return out, nil
}

func (s *eyesSetup) powershell(script string) (string, error) {
	out, err := s.output("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	return strings.TrimSpace(strings.ReplaceAll(out, "\r", "")), err
}

func (s *eyesSetup) runPowershell(script string) error {
	return s.run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
}

// psQuote wraps a value in a PowerShell single-quoted literal so spaces and
// backslashes in Windows paths survive intact.
func psQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// within reports whether the Windows path child sits inside parent.
func within(child, parent string) bool {
	c := strings.ToLower(strings.TrimRight(child, `\`))
	p := strings.ToLower(strings.TrimRight(parent, `\`))
	return c == p || strings.HasPrefix(c, p+`\`)
}
