package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var hookEvents = []struct{ event, matcher string }{
	{"SessionStart", "startup|resume|clear|compact"},
	{"PostToolUse", ""},
	{"Stop", ""},
	{"SessionEnd", ""},
}

func hookCommand(binPath string) string { return binPath + " hook" }

// MergeHooks ensures our hook entries exist; preserves everything else byte-for-byte
// semantically (round-trips through map[string]any, 2-space indent).
func MergeHooks(settings []byte, binPath string) ([]byte, bool, error) {
	var root map[string]any
	if len(settings) == 0 {
		settings = []byte("{}")
	}
	if err := json.Unmarshal(settings, &root); err != nil {
		return nil, false, fmt.Errorf("settings.json is not valid JSON: %w", err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	cmd := hookCommand(binPath)
	for _, he := range hookEvents {
		entries, _ := hooks[he.event].([]any)
		if containsCommand(entries, cmd) {
			continue
		}
		entries = append(entries, map[string]any{
			"matcher": he.matcher,
			"hooks":   []any{map[string]any{"type": "command", "command": cmd, "timeout": 5}},
		})
		hooks[he.event] = entries
		changed = true
	}
	if !changed {
		return settings, false, nil
	}
	root["hooks"] = hooks
	out, err := json.MarshalIndent(root, "", "  ")
	return out, true, err
}

// RemoveHooks strips matcher-groups whose every inner hook command contains binPath,
// and strips our inner hooks from mixed groups.
func RemoveHooks(settings []byte, binPath string) ([]byte, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(settings, &root); err != nil {
		return nil, false, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return settings, false, nil
	}
	changed := false
	for event, v := range hooks {
		entries, _ := v.([]any)
		var kept []any
		for _, e := range entries {
			group, _ := e.(map[string]any)
			inner, _ := group["hooks"].([]any)
			var keptInner []any
			for _, h := range inner {
				hm, _ := h.(map[string]any)
				c, _ := hm["command"].(string)
				if strings.Contains(c, binPath) {
					changed = true
					continue
				}
				keptInner = append(keptInner, h)
			}
			if len(keptInner) == 0 {
				continue
			}
			group["hooks"] = keptInner
			kept = append(kept, group)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return settings, false, nil
	}
	out, err := json.MarshalIndent(root, "", "  ")
	return out, true, err
}

func containsCommand(entries []any, cmd string) bool {
	for _, e := range entries {
		group, _ := e.(map[string]any)
		inner, _ := group["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); c == cmd {
				return true
			}
		}
	}
	return false
}

func UnitFiles(binPath string) (string, string) {
	socket := `[Unit]
Description=Agent coordinator socket

[Socket]
ListenStream=%t/agent-coordinator.sock

[Install]
WantedBy=sockets.target
`
	service := `[Unit]
Description=Agent coordinator daemon
Requires=agent-coordinator.socket

[Service]
ExecStart=` + binPath + ` daemon
`
	return socket, service
}

func Install(binPath, home string, run func(string, ...string) error) error {
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	sock, svc := UnitFiles(binPath)
	if err := os.WriteFile(filepath.Join(unitDir, "agent-coordinator.socket"), []byte(sock), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(unitDir, "agent-coordinator.service"), []byte(svc), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "enable", "--now", "agent-coordinator.socket"); err != nil {
		return err
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	cur, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, changed, err := MergeHooks(cur, binPath)
	if err != nil {
		return err
	}
	if changed {
		if err := os.WriteFile(settingsPath, merged, 0o644); err != nil {
			return err
		}
	}
	// Idempotent: ignore "already exists".
	if err := run("claude", "mcp", "add", "--scope", "user", "--transport", "stdio",
		"agent-coordinator", "--", binPath, "mcp"); err != nil {
		fmt.Fprintln(os.Stderr, "note: claude mcp add:", err, "(ok if already registered)")
	}
	return nil
}

func Uninstall(home string, run func(string, ...string) error) error {
	binPath, _ := os.Executable()
	run("systemctl", "--user", "disable", "--now", "agent-coordinator.socket")
	run("systemctl", "--user", "stop", "agent-coordinator.service")
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	os.Remove(filepath.Join(unitDir, "agent-coordinator.socket"))
	os.Remove(filepath.Join(unitDir, "agent-coordinator.service"))
	run("systemctl", "--user", "daemon-reload")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if cur, err := os.ReadFile(settingsPath); err == nil {
		if out, changed, err := RemoveHooks(cur, binPath); err == nil && changed {
			os.WriteFile(settingsPath, out, 0o644)
		}
	}
	run("claude", "mcp", "remove", "--scope", "user", "agent-coordinator")
	return nil
}
