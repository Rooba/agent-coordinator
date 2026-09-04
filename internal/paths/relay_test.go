package paths

import (
	"os"
	"path/filepath"
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
	again, err := RelayToken()
	if err != nil || again != first {
		t.Fatalf("token must be stable: %q then %q (%v)", first, again, err)
	}
	t.Setenv("AC_TOKEN", "from-env")
	if got, _ := RelayToken(); got != "from-env" {
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
