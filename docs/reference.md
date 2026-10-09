# Coordinator reference

[Back to the README](../README.md)

## The MCP tools

All under the MCP server `agent-coordinator`. Hook-enabled clients receive an
agent name at session start. MCP-only clients call `register_agent` (or
`agent-coordinator join`); after that, `from` is optional for messaging calls.
When a SessionStart hook ran, the MCP process adopts that same identity via a
bind file (one session = one inbox).

- `register_agent` - only when no SessionStart name was assigned; call once.
- `whoami` - this connection's identity (`name`, `agent_id`, `scope`, `source`,
  optional `parent` for subagents).
- `status_board` - full detail (agent_id, name, presence, task, activity,
  files). Hides `gone` by default; pass `include_gone=true` for all rows.
- `list_agents` - live peers only (active or idle) for contact.
- `send_message` - DM by name or agent_id; prefer short bodies (long content
  -> file under `.ignore/coordination/` + one-line path pointer).
- `read_messages` - **DESTRUCTIVE**: returns and **clears** unread. Subagents
  must pass `from='<child name>'` so they never drain the parent inbox.
- `peek_messages` - non-destructive unread preview (count, senders, ids).
- `broadcast` - one-shot to agents registered **now**; late joiners miss it.
- `list_workspaces` - live workspace scopes and occupancy counts.
- `list_eyes` - connected host launchers and active eyes rows across workspaces.
- `list_eyes_tasks` - bounded task metadata and progress in the current workspace;
  available to local peers. Pass `mine=true` for your requests (including your
  parent's requests if you are a bound child). The same ownership rule controls cancellation.
- `relay` - send to another workspace or a structured return address.
- `request_eyes` - dispatch one browser brief to a host launcher; returns a task ID.
- `cancel_eyes` - cancel a live task owned by this requester.

## How it works

One binary, several subcommands:

- `daemon` - owns the SQLite state, serves a line-JSON protocol on a unix
  socket. Started on demand by the other subcommands and exits after 10
  minutes idle, so it only runs while in use.
- `hook` - invoked by supported session, tool, subagent, permission, interruption,
  and compaction hooks. Forwards the event to
  the daemon and injects any response back into the session as additional
  context. Transport failures let the session continue; the inbox guard can
  deny a subagent's attempt to read the parent's messages.
- `mcp` - stdio MCP server exposing the peer tools, backed by the same socket.
- `wait` - blocks until **new** mail arrives for an agent (see wake pattern).
- `watch` - session-long mail monitor; prints one stdout line per unread batch (see wake pattern).
- `join` - one-shot register + print the SessionStart injection line (for
  harnesses without hooks, or manual bootstrap).
- `board` - print the workspace board (`--live` active+idle only, `--all`
  include gone, `--json` machine-readable).
- `workspaces`, `eyes`, and `relay` - discover and explicitly message across
  workspace scopes.
- `request-eyes` (alias `summon`) and `cancel-eyes` - manage one-shot Windows
  host browser tasks.
- `eyes --tasks` - inspect task state, elapsed time, deadline, and observed progress.
- `eyes-setup` - configure the optional Windows browser bridge from WSL;
  also available through `install --eyes`.
- `host` - Windows-only broker pairing, Task Scheduler installation, foreground
  execution, and removal.
- `install` - configures client integrations (see [Installation](installation.md)).

```
      agent session A                    agent session B
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
detached process and redials briefly; the daemon idle-exits and is
respawned by the next event. A stamp file next to the socket
(`<sock>.spawn`) throttles spawning to one attempt per 5 seconds across all
client processes, and the daemon takes an OS file lock (`<sock>.lock`)
before binding, so racing spawns self-resolve - the losers exit quietly. On
Linux, systemd socket activation still works as an optional nicety.

There is no restart subcommand. The process that bound the socket keeps
serving until it idle-exits (10 minutes) or is stopped. A daemon built
before the watcher flag ignores that flag, so `wake` stays `manual` until
a client of the new binary is the one that spawns the replacement. Stop
the listener, then run any command from the new binary (`board` is
enough):

```
sock="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/agent-coordinator.sock"
[ -S "$sock" ] || sock="/tmp/agent-coordinator-$(id -u)/agent-coordinator.sock"
if [ -S "$sock" ]; then kill "$(lsof -t -- "$sock" | head -1)"; fi
```

The push path: agent B calls `send_message`. The next time agent A
finishes any tool call, A's PostToolUse hook reports the event and the
daemon piggybacks a notice on the reply - `[coordinator] 1 new message
from brisk-owl - call read_messages` - which the harness injects into A's
context. A then reads it with `read_messages`.

### Wake levers

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

`wait` resolves the workspace scope from its cwd and checkpoints
the agent's **high-water** message id, then polls the daemon (read-only peek,
default every 2s) until a later unread message appears. It prints
`armed after_id=N` to stderr only after that checkpoint is durable; delegate
or idle after seeing it. A first-ever arm ignores the backlog included in its
initial high-water mark.

The cursor survives a killed waiter and a normal timeout, so promptly re-running
the same scoped name covers mail delivered in either re-arm gap. Each re-arm
refreshes its 24-hour expiry. A wake does not advance past unread mail until a
later quiet peek proves it was consumed: a waiter killed before its wake reaches
the harness therefore leaves the mail available to wake its replacement. Concurrent
waiters may both report the same mail, but cannot checkpoint past it. Corrupt or
expired state is safely replaced with a fresh baseline. Exit 0 prints
`mail from=<names> count=N ids=...`; exit 1 on timeout prints `timeout`
(default 570s, under common 600s background caps); exit 2 on usage error.
Peeking never consumes mail or the once-only notice nudge.

An agent blocked on a synchronous subagent has no harness touchpoint and
cannot be woken. An agent that arms `wait` as a BACKGROUND task before
delegating or idling gets re-invoked the moment `wait` exits - i.e. the
moment new mail arrives - but only on harnesses that start a new turn
when a background task completes. Claude Code does. Codex does not: its
background terminals yield control back mid-turn and produce no new turn
on exit, so under Codex the wait must be run in the FOREGROUND as the
last action of the turn. Wait for the `armed` line, then delegate; if
the process is killed or times out, promptly re-arm it. The SessionStart
injection (or `agent-coordinator join`) teaches every agent this pattern
with its own name filled in. A live mail monitor suppresses that reminder
on a later prompt: the UserPromptSubmit hook says the monitor is armed only
while a watcher peek is fresh.

### Session mail monitor (`watch`)

```
agent-coordinator watch [<name>] [-interval <seconds>] [-quiet <seconds>]
```

`watch` is the process a harness monitor keeps running for the session.
With no name it resolves the agent the same way the MCP server does:
workspace scope plus pid ancestry, retrying until the SessionStart hook
writes the bind file (1s, backing off to 5s). An explicit name skips that
lookup. Stderr prints `watch: bound as <name>` once.

`watch <name>` uses the same per-name cursor file as `wait` for that
name. A monitor started with no name keeps a separate cursor keyed by
the bound session, so it does not resume or advance the manual `wait`
checkpoint. Stdout is one `mail from=<names> count=N ids=...` line per
batch, after a quiet window (default 1 second) so a burst is a single
line. Both flags are integer seconds, same as `wait -timeout`. A newer
id prints again even while older mail is still unread. The set of ids
this process has already printed stays in memory only, so a restart
announces mail that is still unread. The durable cursor advances only
after that mail is consumed, same as `wait`.

Each peek is marked as a watcher. For about 10 seconds after the last peek
the session reports `wake: monitor` from `whoami` and `status_board`;
otherwise `wake: manual`. The process exits on SIGTERM, SIGINT, or a closed
stdout. A daemon gap retries with backoff and writes at most one stderr
note; stdout stays empty until there is mail.

The Claude Code plugin starts this command from `monitors/monitors.json`
when that host runs plugin monitors (interactive session, Monitor tool
available, v2.1.105+; not from a project-scope plugin). The Codex, Kimi
Code CLI, Cursor, Gemini CLI, and OpenCode installs in this repo, and any
Claude Code session with no live monitor, keep `wait`.

### Bootstrap without hooks (`join`)

```
agent-coordinator join [-session-id <id>] [-source <label>]
```

Registers in the current workspace and prints the same
`[coordinator] you are '<name>' ...` line the SessionStart hook emits.
Session id resolution: `-session-id`, then `CLAUDE_CODE_SESSION_ID` /
`GROK_SESSION_ID` / `CODEX_SESSION_ID` / `AC_SESSION_ID`, else an ephemeral
id (name will not stick across restarts).

## Configuration

- `AC_SOCKET` - socket path. Default `$XDG_RUNTIME_DIR/agent-coordinator.sock`;
  if `XDG_RUNTIME_DIR` is unset, `/run/user/<uid>/agent-coordinator.sock` when
  that directory exists and is owned by you (so shims launched without a login
  environment still reach the same daemon), else a private per-uid directory
  `/tmp/agent-coordinator-<uid>/agent-coordinator.sock` (mode 0700). On
  Windows: `%LOCALAPPDATA%\agent-coordinator\ac.sock`.
- `AC_DB` - database path. Default `~/.local/state/agent-coordinator/coordinator.db`
  (honors `XDG_STATE_HOME`; `%LOCALAPPDATA%\agent-coordinator\coordinator.db`
  on Windows).
- `AC_DEBUG` - when set, the hook logs diagnostics to stderr instead of
  failing silently. Try `AC_DEBUG=1 agent-coordinator hook < event.json`.
- `AC_NO_SPAWN` - when set, clients never spawn the daemon on a missed dial
  and simply fail open. Mostly useful for tests and debugging.
- `AC_RELAY_LISTEN` - enables the optional TCP relay. Unset disables it;
  `1`/`true` selects `127.0.0.1:7400`, or set an explicit numeric loopback
  `IP:PORT`. Non-loopback addresses are rejected.
- `AC_TOKEN` - optional relay-token override. It must contain exactly 64
  lowercase hexadecimal characters. Normal installations should use the
  generated protected `relay.token` file instead.

### Local transport and TCP

Local hooks and MCP clients use the Unix-domain socket, including on supported
Windows versions. The optional authenticated TCP listener serves the Windows
host/eyes bridge, with a restricted set of operations and launcher/eyes
identities. Replacing `AC_SOCKET` with a TCP address does not enable TCP for
ordinary hooks or MCP clients.

Both listeners share the daemon's request handler and SQLite store. Request
timeouts can include database contention and handler work, so changing the
socket type alone does not remove those delays. Keep the local socket for
same-machine coordination and enable the relay for the Windows bridge.

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
ordinary boards, presence, DMs, and broadcasts stay inside their workspace.
The explicit `list_workspaces`, `list_eyes`, and `relay` operations are the
cross-scope exceptions; a relay message records its source scope and a
server-stamped structured return address.

## Data

A single SQLite database at
`~/.local/state/agent-coordinator/coordinator.db` (see `AC_DB`). The
daemon is the only writer. Housekeeping purges agents with `last_seen`
older than 2 hours (keeps the board free of ghosts), messages 7 days after
every delivery is read (30 days unconditionally), and other event rows on
their own windows. Every bound MCP tool call heartbeats `last_seen` so
MCP-only sessions stay `active` without hook traffic.

## Model quota hazard (not a daemon bug)

Large multi-agent trees on quota-limited models (e.g. Fable-class) can hit a
usage wall mid-run and churn names/tasks without a clear "quota exhausted"
signal. Prefer higher-capacity models (Sonnet/Opus class) for fan-outs of more
than a handful of concurrent agents; reserve cheaper models for short tasks.
This is operational guidance, not a coordinator defect - surface it in your
spawn recipe before launching a 15-agent tree.

## Known limitations and field findings

Addressed by the 2026-08-05 pair work (see [the pair-session retro](IMPROVEMENTS-2026-08-05-pair-session-retro.md)):
hook/MCP identity unification via bind files, subagent child identities + PreToolUse drain
guard, wait high-water baseline, board hide-gone + 2h GC, Grok hooks install, whoami /
peek_messages, richer notice previews.

Remaining field notes:

- Name collisions - simultaneous sibling spawns can still land on the same adjective-animal
  base before suffixing; key durable artifacts by the stable `agent_id` instead of display name.
- A `send_message` request timed out once under roughly 30-agent load; the
  original observation did not establish the cause. Check delivery with
  `message_history` before resending: a response timeout can happen after the
  message was stored, and an automatic retry can duplicate it.
- Broadcasts are one-shot (see Broadcast etiquette above) - an agent spawned after a broadcast fired
  never sees it; DM critical directives instead of relying on broadcast for late joiners.

Historical findings and follow-up work are tracked in the retro doc and [TODO](../TODO.md).
