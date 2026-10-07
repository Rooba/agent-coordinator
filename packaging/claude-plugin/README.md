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
   these versions; otherwise it builds the release and publishes the npm packages.
3. Run `make plugin-lock` from the repository root, commit `package-lock.json`, push, and
   resubmit the plugin to the directory.
