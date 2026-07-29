package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type hookEvent struct{ event, matcher string }

var hookEvents = []hookEvent{
	{"SessionStart", "startup|resume|clear|compact"},
	{"UserPromptSubmit", ""},
	{"PostToolUse", ""},
	{"Stop", ""},
	{"SessionEnd", ""},
}

var codexHookEvents = []hookEvent{
	{"SessionStart", "startup|resume|clear|compact"},
	{"UserPromptSubmit", ""},
	{"PostToolUse", ""},
	{"Stop", ""},
}

// hookCommand is the exact identity key for our entries in settings.json.
// A path with spaces is double-quoted: hook commands run through a shell
// (cmd on Windows), which would otherwise split the path.
func hookCommand(binPath string) string {
	if strings.Contains(binPath, " ") {
		return `"` + binPath + `" hook`
	}
	return binPath + " hook"
}

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
	return mergeHooks(settings, binPath, hookEvents)
}

func MergeCodexHooks(settings []byte, binPath string) ([]byte, bool, error) {
	return mergeHooks(settings, binPath, codexHookEvents)
}

func mergeHooks(settings []byte, binPath string, events []hookEvent) ([]byte, bool, error) {
	if len(settings) == 0 {
		settings = []byte("{}")
	}
	var root map[string]any
	if err := json.Unmarshal(settings, &root); err != nil {
		return nil, false, fmt.Errorf("settings.json is not valid JSON: %w", err)
	}
	if root == nil { // settings.json was the JSON literal null
		root = map[string]any{}
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
	for _, he := range events {
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

func mergeOpenCodeConfig(config []byte, binPath string) ([]byte, bool, error) {
	if len(bytes.TrimSpace(config)) == 0 {
		config = []byte("{}")
	}
	var root map[string]any
	if err := json.Unmarshal(config, &root); err != nil {
		return nil, false, fmt.Errorf("opencode.json is not plain JSON: %w", err)
	}
	if root == nil {
		root = map[string]any{}
	}
	mcp := map[string]any{}
	if v, ok := root["mcp"]; ok {
		var valid bool
		mcp, valid = v.(map[string]any)
		if !valid {
			return nil, false, fmt.Errorf("opencode.json: \"mcp\" is not an object; refusing to overwrite")
		}
	}
	if entry, ok := mcp["agent-coordinator"].(map[string]any); ok && openCodeEntryMatches(entry, binPath) {
		return config, false, nil
	}
	mcp["agent-coordinator"] = map[string]any{
		"type":    "local",
		"command": []string{binPath, "mcp"},
		"enabled": true,
	}
	root["mcp"] = mcp
	out, err := marshalSettings(root)
	return out, true, err
}

func removeOpenCodeConfig(config []byte, binPath string) ([]byte, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(config, &root); err != nil {
		return nil, false, err
	}
	mcp, ok := root["mcp"].(map[string]any)
	if !ok {
		return config, false, nil
	}
	entry, ok := mcp["agent-coordinator"].(map[string]any)
	if !ok || !openCodeEntryMatches(entry, binPath) {
		return config, false, nil
	}
	delete(mcp, "agent-coordinator")
	if len(mcp) == 0 {
		delete(root, "mcp")
	}
	out, err := marshalSettings(root)
	return out, true, err
}

func openCodeEntryMatches(entry map[string]any, binPath string) bool {
	if typ, _ := entry["type"].(string); typ != "local" {
		return false
	}
	switch cmd := entry["command"].(type) {
	case []any:
		if len(cmd) != 2 {
			return false
		}
		first, _ := cmd[0].(string)
		second, _ := cmd[1].(string)
		return first == binPath && second == "mcp"
	case []string:
		return len(cmd) == 2 && cmd[0] == binPath && cmd[1] == "mcp"
	default:
		return false
	}
}

func installOpenCode(binPath, home string) error {
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	config, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out, changed, err := mergeOpenCodeConfig(config, binPath)
	if err != nil || !changed {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o644)
}

func uninstallOpenCode(binPath, home string) error {
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	config, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	out, changed, err := removeOpenCodeConfig(config, binPath)
	if err != nil || !changed {
		return err
	}
	return writeFileAtomic(path, out, 0o644)
}

func registerMCPClients(binPath string, run func(string, ...string) error) error {
	clients := []struct {
		name   string
		remove []string
		add    []string
	}{
		{
			name:   "claude",
			remove: []string{"mcp", "remove", "--scope", "user", "agent-coordinator"},
			add:    []string{"mcp", "add", "--scope", "user", "--transport", "stdio", "agent-coordinator", "--", binPath, "mcp"},
		},
		{
			name:   "codex",
			remove: []string{"mcp", "remove", "agent-coordinator"},
			add:    []string{"mcp", "add", "agent-coordinator", "--", binPath, "mcp"},
		},
		{
			name:   "grok",
			remove: []string{"mcp", "remove", "--scope", "user", "agent-coordinator"},
			add:    []string{"mcp", "add", "--scope", "user", "agent-coordinator", "--", binPath, "mcp"},
		},
	}
	for _, client := range clients {
		// Replacing our named entry is what makes reinstall repair stale paths.
		if err := run(client.name, client.remove...); errors.Is(err, exec.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "note: %s not installed; skipping MCP registration\n", client.name)
			continue
		}
		if err := run(client.name, client.add...); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "note: %s not installed; skipping MCP registration\n", client.name)
				continue
			}
			return fmt.Errorf("%s mcp add: %w", client.name, err)
		}
	}
	return nil
}

func unregisterMCPClients(run func(string, ...string) error) {
	_ = run("claude", "mcp", "remove", "--scope", "user", "agent-coordinator")
	_ = run("codex", "mcp", "remove", "agent-coordinator")
	_ = run("grok", "mcp", "remove", "--scope", "user", "agent-coordinator")
}

func installHookConfig(path, binPath string, merge func([]byte, string) ([]byte, bool, error)) error {
	cur, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, changed, err := merge(cur, binPath)
	if err != nil || !changed {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, merged, 0o644)
}

func uninstallHookConfig(path, binPath string) {
	cur, err := os.ReadFile(path)
	if err != nil {
		return
	}
	out, changed, err := RemoveHooks(cur, binPath)
	if err == nil && changed {
		_ = writeFileAtomic(path, out, 0o644)
	}
}

// writeFileAtomic writes via a temp file in the same directory + rename, so a
// crash mid-write can never leave a truncated settings.json behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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

// installUnits sets up systemd socket activation - an optional Linux nicety
// now that clients spawn the daemon on demand. Any systemctl failure (absent
// binary, WSL without systemd, or a transient error on a real systemd host)
// degrades: the socket is disabled best-effort so no sockets.target.wants
// symlink dangles, the freshly written units are removed, a note is printed,
// and install continues with hooks + MCP.
func installUnits(binPath, home string, run func(string, ...string) error) error {
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	sock, svc := UnitFiles(binPath)
	sockPath := filepath.Join(unitDir, "agent-coordinator.socket")
	svcPath := filepath.Join(unitDir, "agent-coordinator.service")
	if err := os.WriteFile(sockPath, []byte(sock), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(svcPath, []byte(svc), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", "agent-coordinator.socket"},
		// Pick up the new binary on re-install; exits 0 when nothing is running.
		{"--user", "try-restart", "agent-coordinator.service"},
	} {
		if err := run("systemctl", args...); err != nil {
			// Best-effort: on a real systemd host an earlier successful enable
			// left a sockets.target.wants symlink that would dangle once the
			// unit files go; without systemd this is a harmless no-op.
			run("systemctl", "--user", "disable", "--now", "agent-coordinator.socket")
			os.Remove(sockPath)
			os.Remove(svcPath)
			fmt.Fprintln(os.Stderr, "note: systemctl unavailable or failed; skipping socket activation - clients start the daemon on demand")
			return nil
		}
	}
	return nil
}

func Install(binPath, home string, run func(string, ...string) error) error {
	if runtime.GOOS == "linux" {
		if err := installUnits(binPath, home, run); err != nil {
			return err
		}
	}
	if err := installHookConfig(filepath.Join(home, ".claude", "settings.json"), binPath, MergeHooks); err != nil {
		return err
	}
	if err := installHookConfig(filepath.Join(home, ".codex", "hooks.json"), binPath, MergeCodexHooks); err != nil {
		return err
	}
	if err := registerMCPClients(binPath, run); err != nil {
		return err
	}
	if err := installOpenCode(binPath, home); err != nil {
		fmt.Fprintln(os.Stderr, "note: OpenCode MCP config:", err)
	}
	return nil
}

func Uninstall(binPath, home string, run func(string, ...string) error) error {
	// Unit removal mirrors installUnits: Linux-only, and every systemctl
	// error is ignored so a systemd-less host still gets hooks + MCP stripped.
	if runtime.GOOS == "linux" {
		run("systemctl", "--user", "disable", "--now", "agent-coordinator.socket")
		run("systemctl", "--user", "stop", "agent-coordinator.service")
		unitDir := filepath.Join(home, ".config", "systemd", "user")
		os.Remove(filepath.Join(unitDir, "agent-coordinator.socket"))
		os.Remove(filepath.Join(unitDir, "agent-coordinator.service"))
		run("systemctl", "--user", "daemon-reload")
	}
	uninstallHookConfig(filepath.Join(home, ".claude", "settings.json"), binPath)
	uninstallHookConfig(filepath.Join(home, ".codex", "hooks.json"), binPath)
	unregisterMCPClients(run)
	if err := uninstallOpenCode(binPath, home); err != nil {
		fmt.Fprintln(os.Stderr, "note: OpenCode MCP config:", err)
	}
	return nil
}
