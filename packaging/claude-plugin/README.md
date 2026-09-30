# agent-coordinator Claude Code plugin

Presence, status, and messaging for concurrent coding agents sharing a workspace, packaged
as a Claude Code plugin. It registers:

- ten hooks (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, PreCompact, PostCompact,
  Stop, SubagentStart, SubagentStop, SessionEnd) that keep presence and identity current;
- one MCP server, `agent-coordinator`, exposing the coordinator tools;
- one skill, `agent-coordinator`, teaching agents the collaboration workflow.

## Install

```sh
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Inside a session, `/plugin marketplace add https://github.com/Rooba/agent-coordinator` and
`/plugin install agent-coordinator@agent-coordinator` do the same. No PATH setup is needed.

If you previously ran `agent-coordinator install`, run `agent-coordinator install --uninstall`
first, or every hook fires twice.

## How the binary arrives

`bin/agent-coordinator` is a small bash launcher. On first use it downloads the release
binary matching the plugin version from GitHub Releases into
`${CLAUDE_PLUGIN_DATA}/bin/agent-coordinator-<version>` (outside Claude Code:
`${XDG_DATA_HOME:-~/.local/share}/agent-coordinator/plugin/bin`). The download is verified
against the release's `SHA256SUMS` and refused on a mismatch or a missing checksum file. Later runs reuse the
cached binary. Windows needs Git Bash, which Claude Code already requires there.

Offline, hooks exit 0 silently so sessions keep working; the MCP server reports one error line.

Environment overrides:

- `AC_PLUGIN_VERSION` - release version to run instead of the one in `plugin.json`.
- `AC_PLUGIN_RELEASE_URL` - release download base (default
  `https://github.com/Rooba/agent-coordinator/releases/download`).

## Releasing

Bump `version` in `.claude-plugin/plugin.json` here and in the repository root
`.claude-plugin/marketplace.json`, then tag `v<version>`. The release workflow refuses a tag
that does not match the plugin version.
