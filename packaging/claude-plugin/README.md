# agent-coordinator Claude Code plugin

This plugin connects concurrent Claude Code sessions in the same repository, and lets them work
alongside Codex, Grok Build, and other MCP sessions there. Each session gets a name, appears on a
shared status board, can claim files, and can message the others; mail shows up in the
recipient's context without polling. This page covers installing the plugin, what it registers,
how its binary arrives, and how to release it. For what the tool does and how to use it, see the
[project README](https://github.com/Rooba/agent-coordinator#readme).

## Install

```sh
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Inside a session, `/plugin marketplace add https://github.com/Rooba/agent-coordinator` and
`/plugin install agent-coordinator@agent-coordinator` do the same. No PATH setup is needed.

If you previously ran `agent-coordinator install`, run `agent-coordinator install --uninstall`
first, or every hook fires twice.

## What it registers

- ten hooks (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, PreCompact, PostCompact,
  Stop, SubagentStart, SubagentStop, SessionEnd) that keep presence and identity current;
- one MCP server, `agent-coordinator`, exposing the coordinator tools;
- one skill, `agent-coordinator`, teaching agents the collaboration workflow;
- one session monitor, `monitors/monitors.json`, that runs `agent-coordinator watch` so DMs wake
  the session without a hand-armed `wait`. Claude Code starts it in interactive sessions where the
  Monitor tool is available (v2.1.105+), including on plugin reload. It stays off for `claude -p`,
  when the API provider or telemetry settings disable that tool, and for a project-scope plugin.
  Those sessions keep the manual `wait` instructions.

## How the binary arrives

The binary ships as one npm package per platform: `agent-coordinator-cli-linux-x64`,
`-linux-arm64`, `-darwin-x64`, `-darwin-arm64`, and `-win32-x64`, built and published with
provenance by this repository's release workflow. `package.json` lists them as optional
dependencies and `package-lock.json` pins each to an exact version and integrity hash. When the
plugin is installed, Claude Code installs the package for your platform with install scripts
disabled, into `${CLAUDE_PLUGIN_ROOT}/node_modules/agent-coordinator-cli-<os>-<cpu>/`.

`scripts/agent-coordinator` is a small bash launcher that runs that binary. The plugin makes no
network requests of its own. Windows needs Git Bash, which Claude Code already requires there.

If the binary is missing, hooks exit 0 silently so sessions keep working, and the MCP server
reports one error line asking you to reinstall the plugin.

What the plugin stores locally, for how long, and what reaches the network are described in
[PRIVACY.md](PRIVACY.md).

## Releasing

1. Bump `version` in `.claude-plugin/plugin.json` here, in the repository root
   `.claude-plugin/marketplace.json`, and in `package.json` (its `version` and every
   `optionalDependencies` entry).
2. Tag `v<version>` and push the tag. The release workflow refuses a tag that does not match
   these versions; otherwise it builds the release and, when the repository variable
   `NPM_PUBLISH` is `true`, publishes the npm packages.
3. Run `make plugin-lock` from the repository root, commit `package-lock.json`, push, and
   resubmit the plugin to the directory.
