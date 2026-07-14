# agent-coordinator

Presence, status, and messaging for concurrent Claude Code sessions working
in the same repository. v2 is hooks-based: session lifecycle events flow
through Claude Code hooks into a small on-demand daemon, and peer tools are
exposed over MCP. There is no proxy in front of the model and no
polling - message notices are pushed into an agent's context on its next
tool call.

## How it works

One binary, five subcommands:

- `daemon` - owns the SQLite state, serves a line-JSON protocol on a unix
  socket. Started on demand by the other subcommands and exits after 10
  minutes idle, so it only runs while in use.
- `hook` - invoked by user-level Claude Code hooks (SessionStart,
  UserPromptSubmit, PostToolUse, Stop, SessionEnd). Forwards the event to
  the daemon and injects any response back into the session as additional
  context. Every error path is a silent no-op: a broken coordinator can
  never break a Claude session.
- `mcp` - stdio MCP server exposing the five peer tools, backed by the
  same socket.
- `wait` - blocks until mail arrives for an agent, for the wake pattern
  (see below).
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
                    (spawned on demand)
                             |
                             v
                 agent-coordinator daemon
                             |
                             v
        ~/.local/state/agent-coordinator/coordinator.db
```

Spawn on miss: no service manager is required. Any client (`hook`, `mcp`,
`wait`) that finds nobody listening spawns `agent-coordinator daemon` as a
detached process and redials briefly; the daemon idle-exits and is respawned
by the next event. A stamp file next to the socket throttles spawning to one
attempt per cooldown across all client processes, and the daemon takes an OS
file lock (sock+".lock") before binding, so racing spawns self-resolve - the
losers exit quietly. On Linux, systemd
socket activation still works as an optional nicety; native Windows and WSL
without systemd work out of the box.

The push path: agent B calls `send_message`. The next time agent A
finishes any tool call, A's PostToolUse hook reports the event and the
daemon piggybacks a notice on the reply - `[coordinator] 1 new message
from brisk-owl - call read_messages` - which Claude Code injects into A's
context. A then reads it with `read_messages`.

## Wake levers

A notice can only reach an agent at a harness touchpoint. Four are wired
up; each notice is delivered exactly once (the first touchpoint that
fires consumes it, the mail itself stays unread until `read_messages`):

- PostToolUse - the classic push path above: notices ride the next tool
  call.
- Stop (turn-end nudge) - when an agent ends its turn with pending
  notices, the Stop hook emits blocking output (`decision: block`) whose
  reason carries the notices, so the model sees the mail instead of going
  idle. Once-only by construction: a repeat Stop with unread-but-noticed
  mail returns nothing, so there is no Stop loop.
- UserPromptSubmit - pending notices are injected as additional context
  when the user submits a prompt, so a fresh turn starts already knowing
  about the mail.
- `agent-coordinator wait` - programmatic wake for agents that would
  otherwise be unreachable (blocked on a synchronous subagent, or simply
  idle with no hook touchpoint coming).

### The wake pattern (`wait`)

```
agent-coordinator wait <name> [-timeout <seconds>] [-interval <seconds>]
```

`wait` resolves the workspace scope from its cwd and polls the daemon
(read-only peek, default every 2s) until the named agent has unread mail.
It exits 0 the moment mail arrives (`mail: N unread - call
read_messages`), 1 on timeout (default 570s, under common 600s background
caps), 2 on usage error. Peeking never consumes the once-only notice
nudge - the other levers still fire.

An agent blocked on a synchronous subagent has no harness touchpoint and
cannot be woken. But an agent that arms `wait` as a BACKGROUND task
before delegating or idling gets re-invoked by the harness the moment
`wait` exits - i.e. the moment a DM arrives. Arm first, then delegate.
The SessionStart injection teaches every agent this pattern with its own
name filled in.

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
- merges the five hooks into `~/.claude/settings.json` (existing hooks are
  preserved; the merge is idempotent and the write is atomic),
- registers the MCP server (done for you; equivalent to `claude mcp add
  --scope user --transport stdio agent-coordinator --
  ~/.local/bin/agent-coordinator mcp`).

systemd is no longer required: when `systemctl` is absent or fails (WSL
without systemd, containers), install prints a note, skips the units, and
continues - clients start the daemon on demand.

Uninstall with `make uninstall` (or `agent-coordinator install
--uninstall`): removes the units, strips exactly the hooks it added, and
deregisters the MCP server. State in `~/.local/state/agent-coordinator/`
is left behind; delete it by hand if you want a clean slate.

### Windows

```powershell
go build -o agent-coordinator.exe .\cmd\agent-coordinator
.\agent-coordinator.exe install
```

Or cross-compile from Linux with `make build-windows` and copy
`agent-coordinator.exe` over. `install` merges the same five hooks and
registers the MCP server exactly as on Linux; there are no service units -
clients start the daemon on demand. Socket and state live under
`%LOCALAPPDATA%\agent-coordinator\`.

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
  `/tmp/agent-coordinator-<uid>/agent-coordinator.sock` (mode 0700). On
  Windows: `%LOCALAPPDATA%\agent-coordinator\ac.sock`.
- `AC_DB` - database path. Default `~/.local/state/agent-coordinator/coordinator.db`
  (`%LOCALAPPDATA%\agent-coordinator\coordinator.db` on Windows).
- `AC_NO_SPAWN` - when set, clients never spawn the daemon on a missed dial
  and simply fail open. Mostly useful for tests and debugging.
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
