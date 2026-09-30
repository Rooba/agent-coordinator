# Installation

[Back to the README](../README.md)

For the shortest setup path, follow the [README quickstart](../README.md#install).

### Release binary (easiest)

Download the binary for your OS from GitHub Releases (linux amd64/arm64,
windows amd64, darwin amd64/arm64), put it on your PATH, then:

```
agent-coordinator install
```

Windows (PowerShell):

```powershell
agent-coordinator.exe install
```

### go install

```
go install github.com/Rooba/agent-coordinator/cmd/agent-coordinator@latest
agent-coordinator install
```

For a private repository, set `GOPRIVATE` to its GitHub owner path and use Git
credentials that can read it.

### From source

```
git clone https://github.com/Rooba/agent-coordinator
cd agent-coordinator
make install
```

`make install` builds the binary, copies it to
`~/.local/bin/agent-coordinator`, and runs `agent-coordinator install`.

### Other MCP clients

Add a local stdio server to your client's MCP configuration, using the absolute
path to your installed binary:

```json
{
  "mcpServers": {
    "agent-coordinator": {
      "command": "/absolute/path/to/agent-coordinator",
      "args": ["mcp"]
    }
  }
}
```

On Windows, use the `.exe` path and JSON-escaped backslashes. The MCP process's
working directory selects its workspace; launch it in the repository the agents
share. MCP provides messaging and presence even when the client has no lifecycle
hook integration. Automatic tool activity and session tracking require hooks.

### Claude Code plugin

```
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Inside a session, use `/plugin marketplace add https://github.com/Rooba/agent-coordinator` and
`/plugin install agent-coordinator@agent-coordinator`. A listing in the official
Claude Code plugin directory is being submitted; once approved, it installs from
there too.

The plugin registers the same ten Claude Code hooks and the same
`agent-coordinator` MCP server as `agent-coordinator install`, plus a skill with
the usage guide. No PATH setup or separate binary download is needed.

On first use, the launcher (`bin/agent-coordinator` in the plugin) downloads the
release binary matching the plugin's version (`v<version>`, asset
`agent-coordinator_<os>_<arch>[.exe]`) from GitHub Releases into
`$CLAUDE_PLUGIN_DATA/bin/`, falling back to
`~/.local/share/agent-coordinator/plugin/bin/`. It verifies the download against
the release's `SHA256SUMS` and refuses to run on a mismatch or a missing checksum file.

Offline or on download failure, hooks fail open (sessions keep working without a
coordinator) and the MCP server reports the error. Overrides:

- `AC_PLUGIN_RELEASE_URL`: release download base.
- `AC_PLUGIN_VERSION`: version to download.

Under the plugin, MCP tools are named
`mcp__plugin_agent-coordinator_agent-coordinator__<tool>` instead of
`mcp__agent-coordinator__<tool>`; the hooks handle both.

Do not run both. If you previously ran `agent-coordinator install`, run
`agent-coordinator install --uninstall` before installing the plugin, otherwise
every hook fires twice and two MCP servers are registered.

#### Releasing a new plugin version

1. Bump `version` in `packaging/claude-plugin/.claude-plugin/plugin.json` and in
   `.claude-plugin/marketplace.json`.
2. Tag `v<version>`. The release workflow fails if the tag and manifests disagree.

The plugin pins that version, so the tag must exist before users can install it.

### What `install` does

- merges Claude lifecycle hooks into `~/.claude/settings.json` (SessionStart,
  UserPromptSubmit, PreToolUse, PostToolUse, PreCompact, PostCompact, Stop, SubagentStart, SubagentStop,
  SessionEnd) - existing hooks are preserved, the merge is idempotent, and the
  write is atomic (PreToolUse also guards against subagents draining the parent inbox),
- merges Codex lifecycle hooks into `~/.codex/hooks.json` (SessionStart,
  SessionEnd, UserPromptSubmit, PreToolUse, PermissionRequest, PostToolUse,
  PreCompact, PostCompact, SubagentStart, SubagentStop, Stop, Interrupt),
- writes Grok Build lifecycle hooks to `~/.grok/hooks/agent-coordinator.json`
  (same set as Claude - dedicated file, not only Claude-compat import),
- replaces stale `agent-coordinator` MCP registrations for Claude Code, Codex,
  and Grok Build, and merges the local server into OpenCode's global JSON config,
- on Linux with systemd, additionally sets up socket activation
  (`agent-coordinator.socket` + `agent-coordinator.service` user units) as a
  nicety, and `try-restart`s the service so a running daemon picks up a new
  binary.

No systemd is required anywhere: clients start the daemon on demand, so
stock WSL, macOS, and native Windows work out of the box. When `systemctl`
is absent or fails, install prints a note, skips the units, and continues.

Uninstall: `agent-coordinator install --uninstall` (or `make uninstall`).
It removes the units, strips exactly the hooks it added, and deregisters
the MCP server. State in `~/.local/state/agent-coordinator/` is left
behind; delete it by hand for a clean slate.

### Harness support

- Claude Code: MCP plus session, tool, prompt, stop, and subagent lifecycle tracking.
- Codex: MCP plus native hook tracking, including subagent identity, compaction,
  permission requests, interruption, and session end. The freshness window also
  retires presence when a process exits without a final hook.
- Grok Build: MCP registration plus dedicated hooks in
  `~/.grok/hooks/agent-coordinator.json` (SessionStart name injection and the
  same lifecycle events as Claude). The hook parser accepts both Claude
  snake_case and Grok camelCase stdin envelopes.
- OpenCode: MCP registration is installed. Automatic activity/file/task tracking
  still requires an OpenCode plugin adapter and is not yet claimed here.

Codex requires new or changed non-managed hooks to be reviewed and trusted with
`/hooks` before they execute. See the [official Codex hooks reference](https://learn.chatgpt.com/docs/hooks)
for event support and lifecycle timing.

Compaction hooks update activity and send peers a short notice that context is
being compacted or has been compacted, so important handoff details can be
restated. They preserve the agent's recorded tasks. Re-run `agent-coordinator install`
to add new hooks to an existing installation, then review them in Codex's `/hooks`.
