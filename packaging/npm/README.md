# agent-coordinator platform binary

This package holds one prebuilt `agent-coordinator` binary for a single operating system and
CPU. It is not meant to be installed directly: the agent-coordinator Claude Code plugin lists
every platform package as an optional dependency pinned to an exact version, and Claude Code
installs the one matching your machine when the plugin is installed.

agent-coordinator lets concurrent coding agent sessions in the same repository (Claude Code,
Codex, Grok Build, and other MCP clients) see each other, claim files, and message each other.
It runs locally as a single binary. Each package is built and published with provenance by the
project's release workflow.

Source, documentation, and issues: https://github.com/Rooba/agent-coordinator
