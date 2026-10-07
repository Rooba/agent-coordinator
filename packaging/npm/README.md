# agent-coordinator platform binary

This package holds one prebuilt `agent-coordinator` binary for a single operating system and
CPU. It is not meant to be installed directly: the agent-coordinator Claude Code plugin lists
every platform package as an optional dependency pinned to an exact version, and Claude Code
installs the one matching your machine when the plugin is installed.

agent-coordinator gives concurrent coding agents presence, status, and messaging in a shared
workspace. Each package is built and published with provenance by the project's release workflow.

Source, documentation, and issues: https://github.com/Rooba/agent-coordinator
