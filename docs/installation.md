# Installation

[Back to the README](../README.md)

This page covers every way to install agent-coordinator, what the installer changes on your
machine, how to remove it, and what each supported client gets. Most people need only one of
the two commands in the quick path below; the rest is for other setups and troubleshooting.

## Quick path

Claude Code only:

```sh
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Any mix of Claude Code, Codex, Grok Build, and OpenCode: put the release binary on your PATH,
then run:

```sh
agent-coordinator install
```

Use one or the other for Claude Code, not both: running both makes every hook fire twice.
Restart your clients afterwards, and in Codex review and trust the new hooks with `/hooks`.

## Install methods

### Claude Code plugin

```sh
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Inside a session, use `/plugin marketplace add https://github.com/Rooba/agent-coordinator` and
`/plugin install agent-coordinator@agent-coordinator`. A listing in the official
Claude Code plugin directory is being submitted; once approved, it installs from
there too.

The plugin registers the same ten Claude Code hooks and the same
`agent-coordinator` MCP server as `agent-coordinator install`, plus a skill with
the usage guide, plus one session monitor. No PATH setup or separate binary
download is needed.

If you previously ran `agent-coordinator install`, run
`agent-coordinator install --uninstall` before installing the plugin, otherwise
every hook fires twice and two MCP servers are registered.

Under the plugin, MCP tools are named
`mcp__plugin_agent-coordinator_agent-coordinator__<tool>` instead of
`mcp__agent-coordinator__<tool>`; the hooks handle both.

#### How the plugin gets its binary

The binary ships as one npm package per platform (`agent-coordinator-cli-<os>-<cpu>`
for linux-x64, linux-arm64, darwin-x64, darwin-arm64, and win32-x64), built and
published with provenance by the release workflow. The plugin's `package.json` lists
them as optional dependencies and its `package-lock.json` pins exact versions. When
the plugin is installed, Claude Code installs the package for your platform with
install scripts disabled, into `${CLAUDE_PLUGIN_ROOT}/node_modules/`. The launcher
(`scripts/agent-coordinator` in the plugin) runs that binary and makes no network
requests. On Windows it needs Git Bash, which Claude Code already requires there.

If the binary is missing, hooks fail open (sessions keep working without a
coordinator) and the MCP server reports one error line asking you to reinstall the
plugin.

#### Mail monitor

The plugin can wake a session the moment a DM arrives, without the agent arming
`wait` by hand. `monitors/monitors.json` declares a monitor named `mail`. Its command is
`bash "${CLAUDE_PLUGIN_ROOT}/scripts/agent-coordinator" watch`. On Claude Code
v2.1.105 and later, an interactive session starts that process when the
Monitor tool is available, and each stdout line becomes a task notification
(`mail from=... count=N ids=...`). The default trigger is `always`: session
start and plugin reload. The agent calls `read_messages` and does not also
arm `wait` while `whoami` reports `wake: monitor`.

Claude Code skips the monitor in noninteractive `claude -p` sessions, and in
sessions where the API provider or telemetry settings make the Monitor tool
unavailable. A project-scope plugin (enabled from the repository) does not
load background monitors; a personal-scope install does. Those limits are
Claude Code's, from the [monitors component](https://code.claude.com/docs/en/plugins/components#monitors)
and the [loading reference](https://code.claude.com/docs/en/plugins/loading).
`agent-coordinator install` only writes hooks and MCP config, so that setup
keeps the manual `wait` wake. A skipped monitor leaves `wake: manual`, and
the SessionStart line still tells the agent to arm `wait`.

The `watch` command the monitor runs is newer than the v0.5.1 release. Until the
next release, try the monitor with a binary built from source; see
[Development](development.md#try-the-plugin-monitor-from-source).

Grok Build can arm `agent-coordinator watch` once through its own monitor
tool (`persistent: true`). The Codex and OpenCode installs keep `wait`.

### Release binary

Download the binary for your OS from
[GitHub Releases](https://github.com/Rooba/agent-coordinator/releases)
(linux amd64/arm64, windows amd64, darwin amd64/arm64). Assets are named
`agent-coordinator_<os>_<arch>`; rename yours to `agent-coordinator`
(`agent-coordinator.exe` on Windows), put it on your PATH, then:

```sh
agent-coordinator install
```

Windows (PowerShell):

```powershell
agent-coordinator.exe install
```

Each release also attaches a `SHA256SUMS` file and one `.mcpb` MCP bundle per platform.

### go install

```sh
go install github.com/Rooba/agent-coordinator/cmd/agent-coordinator@latest
agent-coordinator install
```

For a private fork, set `GOPRIVATE` to its GitHub owner path and use Git
credentials that can read it.

### From source

```sh
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
share, or set `AC_SCOPE` (see [Configuration](reference.md#configuration)). MCP
provides messaging and presence even when the client has no lifecycle hook
integration; the agent calls `register_agent` once to get a name. Automatic tool
activity and session tracking require hooks.

## What `install` does

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
  and Grok Build, and merges the local server into OpenCode's global JSON config.
  A client whose CLI is not installed is skipped with a note,
- on Linux with systemd, additionally sets up socket activation
  (`agent-coordinator.socket` + `agent-coordinator.service` user units) as a
  nicety, and `try-restart`s the service so a running daemon picks up a new
  binary.

No systemd is required anywhere: clients start the daemon on demand, so
stock WSL, macOS, and native Windows work out of the box. When `systemctl`
is absent or fails, install prints a note, skips the units, and continues.

Re-run `agent-coordinator install` after an upgrade to add new hooks to an
existing installation, then review them in Codex's `/hooks`.

## Uninstall

```sh
agent-coordinator install --uninstall
```

(or `make uninstall` from a source checkout). It removes the systemd units,
strips exactly the hooks it added (including the Grok hooks file), and
deregisters the MCP server from Claude Code, Codex, and OpenCode. It does not
remove the Grok Build MCP registration; run `grok mcp remove --scope user agent-coordinator`
for that. State in `~/.local/state/agent-coordinator/` is left behind; delete it
by hand for a clean slate.

For the plugin: `claude plugin uninstall agent-coordinator@agent-coordinator`.

## Harness support

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
restated. They preserve the agent's recorded tasks.
