# Privacy and data handling

Everything the agent-coordinator plugin records stays on your machine. Nothing is sent to
the publisher, and there is no telemetry, analytics, or crash reporting.

## What is stored, and where

The daemon keeps a local SQLite database at `$AC_DB`, else
`$XDG_STATE_HOME/agent-coordinator/coordinator.db`, else
`~/.local/state/agent-coordinator/coordinator.db` (Windows:
`%LOCALAPPDATA%\agent-coordinator\coordinator.db`). It holds:

- generated agent names (adjective-animal) and harness session ids;
- the workspace directory each agent is working in;
- tool names, a short activity label, and the file paths a tool touched;
- task and plan titles reported by the harness;
- messages, broadcasts, and file claims that agents send each other;
- records of optional cross-workspace relay and browser-inspection ("eyes") tasks.

Hooks receive the harness's tool-call metadata only to derive the items above. Tool
arguments and tool outputs are not stored.

## Retention

The daemon prunes its own database: presence rows two hours after the last heartbeat, file
touches after one hour, messages seven days after every recipient has read them (thirty days
at most), activity events and tasks after seven days, eyes tasks after one day. The daemon
exits after ten idle minutes. Delete the database file to remove everything.

## Network access

- The plugin makes no network requests of its own. Claude Code installs its pinned npm
  packages, which hold the platform binary, when the plugin is installed.
- The cross-workspace relay and the Windows eyes bridge are optional and connect only to
  endpoints you configure yourself.
- Running `agent-coordinator eyes-setup` on WSL downloads the Windows host program from the
  project's GitHub Releases and verifies it against the release checksums. Nothing runs that
  command unless you do.

## Personal data and credentials

The plugin collects no names, emails, or addresses, reads no credentials, and pulls nothing
from external connectors. Message bodies contain whatever agents choose to write and never
leave the local database.
