package hostbroker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func validHostConfig(t *testing.T) HostConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return HostConfig{
		Version: HostConfigVersion, Addr: DefaultAddr, Provider: "claude", Executable: executable,
		WorkingDir: t.TempDir(), ConfigDir: t.TempDir(), BrowserReady: true,
	}
}

func TestFileConfigStoreProtectedRoundTrip(t *testing.T) {
	config := validHostConfig(t)
	path := filepath.Join(t.TempDir(), "config")
	store, err := NewFileConfigStore(path, testProtector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(config.Executable)) || bytes.Contains(raw, []byte(config.ConfigDir)) {
		t.Fatal("host configuration was stored in plaintext")
	}
	if got, err := store.Load(context.Background()); err != nil || got != config {
		t.Fatalf("config round trip = (%+v, %v)", got, err)
	}
}

// Configs sealed before the Claude model and credentials fields existed must
// still load, keeping the provider defaults.
func TestFileConfigStoreLoadsConfigWithoutClaudeModel(t *testing.T) {
	config := validHostConfig(t)
	sealed, err := testProtector{}.Seal([]byte(`{"version":1,"addr":"` + config.Addr + `","provider":"claude","executable":"` +
		filepath.ToSlash(config.Executable) + `","working_dir":"` + filepath.ToSlash(config.WorkingDir) + `","config_dir":"` +
		filepath.ToSlash(config.ConfigDir) + `","browser_ready":true}`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileConfigStore(path, testProtector{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background())
	if err != nil || got.ClaudeModel != "" || got.ClaudeCredentials != "" {
		t.Fatalf("legacy config load = (%+v, %v)", got, err)
	}
}

func TestHostConfigRejectsUnsafeValues(t *testing.T) {
	valid := validHostConfig(t)
	for _, mutate := range []func(*HostConfig){
		func(config *HostConfig) { config.Version++ },
		func(config *HostConfig) { config.Provider = "codex" },
		func(config *HostConfig) { config.BrowserReady = false },
		func(config *HostConfig) { config.Addr = "localhost:7400" },
		func(config *HostConfig) { config.Executable = "claude" },
		func(config *HostConfig) { config.ConfigDir = config.WorkingDir },
		func(config *HostConfig) { config.ConfigDir = filepath.Join(config.WorkingDir, "nested") },
		func(config *HostConfig) { config.ClaudeModel = "sonnet --dangerously-skip-permissions" },
		func(config *HostConfig) { config.ClaudeCredentials = ".claude/.credentials.json" },
	} {
		config := valid
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Fatalf("unsafe config accepted: %+v", config)
		}
	}
}
