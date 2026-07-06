# agent-coordinator

Presence, status, and messaging for concurrent Claude Code sessions working
in the same repository. v2 is hooks-based: session lifecycle events flow
through Claude Code hooks into a small socket-activated daemon, and peer
tools are exposed over MCP. There is no proxy in front of the model and no
polling - message notices are pushed into an agent's context on its next
tool call.

## How it works

One binary, four subcommands:

- `daemon` - owns the SQLite state, serves a line-JSON protocol on a unix
  socket (systemd socket activation: the daemon only runs while in use).
- `hook` - invoked by user-level Claude Code hooks (SessionStart,
  PostToolUse, Stop, SessionEnd). Forwards the event to the daemon and
  injects any response back into the session as additional context. Every
  error path is a silent no-op: a broken coordinator can never break a
  Claude session.
- `mcp` - stdio MCP server exposing the five peer tools, backed by the
  same socket.
- `install` - registers all of the above (see Install below).

```
   Claude Code session A              Claude Code session B
    |            |                     |            |
    | hooks      | MCP (stdio)         | hooks      | MCP (stdio)
    v            v                     v            v
   `hook`      `mcp`                  `hook`      `mcp`
      \           \                     /           /
       +-----------+---------+---------+-----------+
                             |
                             v
             $XDG_RUNTIME_DIR/agent-coordinator.sock
                 (systemd socket activation)
                             |
                             v
                 agent-coordinator daemon
                             |
                             v
        ~/.local/state/agent-coordinator/coordinator.db
```

The push path: agent B calls `send_message`. The next time agent A
finishes any tool call, A's PostToolUse hook reports the event and the
daemon piggybacks a notice on the reply - `[coordinator] 1 new message
from brisk-owl - call read_messages` - which Claude Code injects into A's
context. A then reads it with `read_messages`.

## Install

```
make install
```

This builds the binary, copies it to `~/.local/bin/agent-coordinator`, and
runs `agent-coordinator install`, which:

- writes and enables a systemd user socket unit
  (`agent-coordinator.socket` + `agent-coordinator.service`), then
  `try-restart`s the service so a running daemon picks up the new binary
  (a no-op when nothing is running),
- merges the four hooks into `~/.claude/settings.json` (existing hooks are
  preserved; the merge is idempotent and the write is atomic),
- registers the MCP server (done for you; equivalent to `claude mcp add
  --scope user --transport stdio agent-coordinator --
  ~/.local/bin/agent-coordinator mcp`).

Uninstall with `make uninstall` (or `agent-coordinator install
--uninstall`): removes the units, strips exactly the hooks it added, and
deregisters the MCP server. State in `~/.local/state/agent-coordinator/`
is left behind; delete it by hand if you want a clean slate.

## The five tools

All under the MCP server `agent-coordinator`. `from` is always YOUR agent
name, given to you at session start.

- `status_board` - the full workspace board: every coordinated agent with
  name, presence, current task, task counts, latest activity and files.
- `list_agents` - live peers (active or idle) in this workspace.
- `send_message` - direct message to one agent, by name or agent_id.
- `read_messages` - read and clear your unread messages.
- `broadcast` - message every live peer in the workspace at once.

## Agent naming and presence

At SessionStart the daemon registers the session and the hook tells it its
name: `[coordinator] you are 'deft-pika' in this workspace ...`. Names are
adjective-animal pairs derived deterministically from the session id, with
a `-2`, `-3` suffix on collision within a scope. Presence decays with
inactivity: active (seen < 2 min ago), idle (< 15 min), stale (< 60 min),
then gone. Stop marks a session idle immediately; SessionEnd marks it
gone.

## Broadcast etiquette

A broadcast interrupts every live agent in the workspace on its next tool
call. Keep broadcasts need-to-know only: schema changes, lock handoffs,
"stop touching X". Anything meant for one agent is a `send_message`.

## Scope semantics

An agent's scope is the git repository root of its working directory.
Linked worktrees resolve to the MAIN repository root, so a session working
in a worktree shares the board with sessions in the main checkout.
Non-git directories scope to themselves. Scopes are fully isolated:
sessions in different repositories never see each other's agents, boards,
or messages.

## Data

A single SQLite database at
`~/.local/state/agent-coordinator/coordinator.db` (honors
`XDG_STATE_HOME`). The daemon is the only writer. Housekeeping prunes
agents unseen for 7 days, and messages 7 days after every delivery is
read (30 days unconditionally).

## Environment variables

- `AC_SOCKET` - socket path. Default `$XDG_RUNTIME_DIR/agent-coordinator.sock`;
  if `XDG_RUNTIME_DIR` is unset, a private per-uid directory
  `/tmp/agent-coordinator-<uid>/agent-coordinator.sock` (mode 0700).
- `AC_DB` - database path. Default `~/.local/state/agent-coordinator/coordinator.db`.
- `AC_DEBUG` - when set, the hook logs diagnostics to stderr instead of
  failing silently. Try `AC_DEBUG=1 agent-coordinator hook < event.json`.

## Testing

```
make test                     # unit + integration tests
scripts/e2e-messaging.sh      # live E2E: two headless claude sessions
                              # exchange a DM through the coordinator
```

The E2E script requires an installed coordinator and the `claude` CLI.

## History

v1 of this project was a task-registry MCP server with a very different
design. Its code and docs live in git history at baseline commit
`236d6d9`.
