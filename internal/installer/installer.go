package installer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

var hookEvents = []struct{ event, matcher string }{
	{"SessionStart", "startup|resume|clear|compact"},
	{"PostToolUse", ""},
	{"Stop", ""},
	{"SessionEnd", ""},
}

func hookCommand(binPath string) string { return binPath + " hook" }

// marshalSettings renders settings without HTML escaping so foreign commands
// containing & < > survive byte-identical. Output ends with a newline.
func marshalSettings(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MergeHooks ensures our hook entries exist; preserves everything else byte-for-byte
// semantically (round-trips through map[string]any, 2-space indent). It refuses to
// proceed (returns an error) if "hooks" or an event list has an unexpected shape,
// rather than clobbering foreign values.
func MergeHooks(settings []byte, binPath string) ([]byte, bool, error) {
	if len(settings) == 0 {
		settings = []byte("{}")
	}
	var root map[string]any
	if err := json.Unmarshal(settings, &root); err != nil {
		return nil, false, fmt.Errorf("settings.json is not valid JSON: %w", err)
	}
	hooks := map[string]any{}
	if v, exists := root["hooks"]; exists {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("settings.json: \"hooks\" is not an object; refusing to overwrite")
		}
		hooks = m
	}
	changed := false
	cmd := hookCommand(binPath)
	for _, he := range hookEvents {
		var entries []any
		if v, exists := hooks[he.event]; exists {
			list, ok := v.([]any)
			if !ok {
				return nil, false, fmt.Errorf("settings.json: hooks.%s is not an array; refusing to overwrite", he.event)
			}
			entries = list
		}
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
	out, err := marshalSettings(root)
	return out, changed, err
}

// RemoveHooks strips only inner hooks whose command is exactly "<binPath> hook",
// dropping a matcher-group or event list only when it was entirely ours. Foreign
// or oddly-shaped values are always kept as-is.
func RemoveHooks(settings []byte, binPath string) ([]byte, bool, error) {
	if binPath == "" {
		// Defense in depth: an empty binPath must never match anything.
		return settings, false, nil
	}
	var root map[string]any
	if err := json.Unmarshal(settings, &root); err != nil {
		return nil, false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return settings, false, nil
	}
	cmd := hookCommand(binPath)
	changed := false
	for event, v := range hooks {
		entries, ok := v.([]any)
		if !ok || len(entries) == 0 {
			continue // not a list we manage, or nothing to strip; keep as-is
		}
		var kept []any
		for _, e := range entries {
			group, ok := e.(map[string]any)
			if !ok {
				kept = append(kept, e)
				continue
			}
			inner, ok := group["hooks"].([]any)
			if !ok || len(inner) == 0 {
				kept = append(kept, e)
				continue
			}
			var keptInner []any
			for _, h := range inner {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); c == cmd {
					changed = true
					continue
				}
				keptInner = append(keptInner, h)
			}
			if len(keptInner) == 0 {
				continue // group was entirely ours
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
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	out, err := marshalSettings(root)
	return out, changed, err
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
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			return err
		}
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

func Uninstall(binPath, home string, run func(string, ...string) error) error {
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
