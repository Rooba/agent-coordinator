package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The relay is opt-in, defaults to 127.0.0.1:7400, accepts an explicit
// loopback host:port with a numeric port, and refuses anything reachable from
// the network. "localhost" is normalised to a literal address so the loopback
// guarantee holds against the bind target, not against a resolver.
func TestRelayListen(t *testing.T) {
	cases := []struct {
		env, want string
		wantErr   bool
	}{
		{env: "", want: ""},
		{env: "1", want: "127.0.0.1:7400"},
		{env: "true", want: "127.0.0.1:7400"},
		{env: "TRUE", want: "127.0.0.1:7400"},
		{env: "127.0.0.1:0", want: "127.0.0.1:0"},
		{env: "localhost:7401", want: "127.0.0.1:7401"},
		{env: "LocalHost:7401", want: "127.0.0.1:7401"},
		{env: "[::1]:7402", want: "[::1]:7402"},
		{env: "0.0.0.0:7400", wantErr: true},
		{env: "192.168.1.10:7400", wantErr: true},
		{env: ":7400", wantErr: true},
		{env: "7400", wantErr: true},
		{env: "127.0.0.1:http", wantErr: true},
		{env: "127.0.0.1:-1", wantErr: true},
		{env: "127.0.0.1:65536", wantErr: true},
		{env: "127.0.0.1:99999", wantErr: true},
	}
	for _, c := range cases {
		t.Setenv("AC_RELAY_LISTEN", c.env)
		got, err := RelayListen()
		if c.wantErr {
			if err == nil {
				t.Errorf("AC_RELAY_LISTEN=%q must be refused, got %q", c.env, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("AC_RELAY_LISTEN=%q -> %q, %v; want %q", c.env, got, err, c.want)
		}
	}
}

// The token file is created once with 32 hex-encoded CSPRNG bytes at mode
// 0600, and re-reads return the same value. AC_TOKEN overrides it.
func TestRelayTokenCreateThenReuse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
	t.Setenv("AC_TOKEN", "")
	path, err := RelayTokenPath()
	if err != nil || path != filepath.Join(dir, "relay.token") {
		t.Fatalf("token path %q (%v)", path, err)
	}
	first, err := RelayToken()
	if err != nil || len(first) != 64 {
		t.Fatalf("first token %q (%v), want 64 hex chars", first, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 && perm != 0o666 { // windows reports 0666
		t.Fatalf("token file mode %v, want 0600", perm)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != first {
		t.Fatalf("token file must contain exactly the token: %q (%v)", body, err)
	}
	again, err := RelayToken()
	if err != nil || again != first {
		t.Fatalf("token must be stable: %q then %q (%v)", first, again, err)
	}
	t.Setenv("AC_TOKEN", hexToken)
	if got, _ := RelayToken(); got != hexToken {
		t.Fatalf("AC_TOKEN must win, got %q", got)
	}
}

// Concurrent first uses must settle on ONE token: the file is published
// whole, so a racer never reads a half-written or empty secret, and no
// temporary file is left behind.
func TestRelayTokenConcurrentCreation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
	t.Setenv("AC_TOKEN", "")
	const n = 12
	tokens := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = RelayToken()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil || len(tokens[i]) != 64 {
			t.Fatalf("goroutine %d: token %q (%v)", i, tokens[i], err)
		}
		if tokens[i] != tokens[0] {
			t.Fatalf("goroutine %d got %q, goroutine 0 got %q: one token only", i, tokens[i], tokens[0])
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "relay.token" && e.Name() != "coordinator.db" {
			t.Fatalf("publishing must leave no temporary behind, found %q", e.Name())
		}
	}
}

// ClientAddr is what a client dials: AC_ADDR verbatim when set, otherwise
// today's unix socket.
func TestClientAddr(t *testing.T) {
	t.Setenv("AC_SOCKET", "/tmp/ac-test.sock")
	t.Setenv("AC_ADDR", "")
	if got, want := ClientAddr(), "unix:///tmp/ac-test.sock"; got != want {
		t.Fatalf("unset AC_ADDR -> %q, want %q", got, want)
	}
	for _, c := range []struct{ env, want string }{
		{"tcp://127.0.0.1:7400", "tcp://127.0.0.1:7400"},
		{"unix:///run/other.sock", "unix:///run/other.sock"},
		{"  tcp://127.0.0.1:7400  ", "tcp://127.0.0.1:7400"},
	} {
		t.Setenv("AC_ADDR", c.env)
		if got := ClientAddr(); got != c.want {
			t.Fatalf("AC_ADDR=%q -> %q, want %q", c.env, got, c.want)
		}
	}
}

func TestRelayInsecure(t *testing.T) {
	for env, want := range map[string]bool{"": false, "0": false, "no": false, "1": true, "true": true, "TRUE": true} {
		t.Setenv("AC_RELAY_INSECURE", env)
		if got := RelayInsecure(); got != want {
			t.Errorf("AC_RELAY_INSECURE=%q -> %v, want %v", env, got, want)
		}
	}
}

// An existing but empty token file must fail loudly: a blank shared secret
// would authorize every caller.
func TestRelayTokenEmptyFileIsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
	t.Setenv("AC_TOKEN", "")
	if err := os.WriteFile(filepath.Join(dir, "relay.token"), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := RelayToken(); err == nil {
		t.Fatalf("empty token file must be an error, got %q", got)
	}
}

// hexToken is a well-formed token file body: 32 CSPRNG bytes as lowercase hex.
const hexToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A token file must be exactly what the daemon mints. Anything else is
// reported, never repaired: a short or hand-typed secret guards the whole
// relay, and silently replacing it would break the paired broker instead.
func TestRelayTokenRejectsMalformedFile(t *testing.T) {
	for _, body := range []string{"hunter2\n", hexToken[:63], hexToken + "extra\n",
		strings.ToUpper(hexToken), hexToken[:63] + "\n",
		"zzzz56789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01"} {
		dir := t.TempDir()
		t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
		t.Setenv("AC_TOKEN", "")
		path := filepath.Join(dir, "relay.token")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := RelayToken()
		if err == nil {
			t.Fatalf("token file %q must be refused, got %q", body, got)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("the error must name the file: %v", err)
		}
		if after, _ := os.ReadFile(path); string(after) != body {
			t.Fatalf("a bad token file must be left alone, got %q", after)
		}
	}
}

// Earlier builds wrote the token with a trailing newline. That one line
// ending is forgiven, so upgrading keeps the paired broker working instead of
// silently turning the relay off - and nothing else about the file is.
func TestRelayTokenForgivesOneLegacyNewline(t *testing.T) {
	for body, want := range map[string]string{
		hexToken:             hexToken,
		hexToken + "\n":      hexToken, // minted before this daemon dropped the newline
		hexToken + "\r\n":    hexToken, // the same file copied through Windows
		hexToken + "\n\n":    "",
		hexToken + "\r":      "",
		" " + hexToken:       "",
		"\n" + hexToken:      "",
		hexToken[:63] + "\n": "",
	} {
		dir := t.TempDir()
		t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
		t.Setenv("AC_TOKEN", "")
		if err := os.WriteFile(filepath.Join(dir, "relay.token"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := RelayToken()
		if want == "" {
			if err == nil {
				t.Errorf("token file %q must be refused, got %q", body, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("token file %q -> %q (%v), want %q", body, got, err, want)
		}
	}
}

// A token readable by other local accounts is not a secret: refuse it rather
// than chmod it back, so whoever loosened it finds out.
func TestRelayTokenRejectsLooseFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows reports synthesized permission bits")
	}
	dir := t.TempDir()
	t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
	t.Setenv("AC_TOKEN", "")
	path := filepath.Join(dir, "relay.token")
	if err := os.WriteFile(path, []byte(hexToken), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := RelayToken()
	if err == nil {
		t.Fatalf("a world-readable token file must be refused, got %q", got)
	}
	if !strings.Contains(err.Error(), "must not be readable by group or other") {
		t.Fatalf("the error must say what is wrong: %v", err)
	}
	if err := os.Chmod(path, 0o604); err != nil {
		t.Fatal(err)
	}
	if got, err := RelayToken(); err == nil {
		t.Fatalf("a token others may read must be refused, got %q", got)
	}
	// What matters is that nobody else can reach it, not the owner's own
	// bits: a read-only 0400 token is still the owner's secret.
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if got, err := RelayToken(); err != nil || got != hexToken {
			t.Fatalf("a %04o hex token file reads back: %q (%v)", mode, got, err)
		}
	}
}

// AC_TOKEN is the client's pairing override, so it wins over the file - but
// it guards the same relay and must be the same 64 lowercase hex characters.
// A hand-typed token is refused rather than quietly weakening the boundary.
func TestRelayTokenEnvOverrideMustBeMinted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
	if err := os.WriteFile(filepath.Join(dir, "relay.token"), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_TOKEN", hexToken)
	if got, err := RelayToken(); err != nil || got != hexToken {
		t.Fatalf("a minted AC_TOKEN must win over the file: %q (%v)", got, err)
	}
	for _, bad := range []string{"paired-by-hand", hexToken[:63], hexToken + "0", strings.ToUpper(hexToken),
		"  " + hexToken + "  ", hexToken + "\n"} {
		t.Setenv("AC_TOKEN", bad)
		got, err := RelayToken()
		if err == nil {
			t.Fatalf("AC_TOKEN %q must be refused, got %q", bad, got)
		}
		if !strings.Contains(err.Error(), "AC_TOKEN") {
			t.Fatalf("the error must name the variable: %v", err)
		}
	}
}

// A token is a tiny owner-only regular file, never a link or an unbounded
// stream. Rejecting these shapes before decoding avoids following a replaced
// path or reading attacker-controlled input into memory.
func TestRelayTokenRejectsUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and unix mode semantics are covered on unix")
	}
	for name, setup := range map[string]func(string) error{
		"directory": func(path string) error { return os.Mkdir(path, 0o700) },
		"oversized": func(path string) error {
			return os.WriteFile(path, []byte(strings.Repeat("a", 1024)), 0o600)
		},
		"symlink": func(path string) error {
			target := path + ".target"
			if err := os.WriteFile(target, []byte(hexToken), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AC_DB", filepath.Join(dir, "coordinator.db"))
			t.Setenv("AC_TOKEN", "")
			if err := setup(filepath.Join(dir, "relay.token")); err != nil {
				t.Fatal(err)
			}
			if token, err := RelayToken(); err == nil {
				t.Fatalf("unsafe token file must be refused, got %q", token)
			}
		})
	}
}
