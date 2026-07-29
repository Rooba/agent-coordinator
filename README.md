# agent-coordinator

Presence, status, and messaging for concurrent coding-agent sessions working
in the same repository. Run Claude Code, Codex, Grok Build, or other MCP
clients in one repo and they stop
colliding: a live status board of who is doing what, direct messages,
broadcasts, and wake-on-mail. Zero infrastructure - a single binary; the
daemon starts itself on demand and exits when idle.

Session lifecycle events flow through supported harness hooks into a small
on-demand daemon, and peer tools are exposed over MCP. There is no proxy in
front of the model and no polling - message notices are pushed into an
agent's context on its next tool call.

## Tell your agents to use it

Paste the block below into your project's `CLAUDE.md` (or equivalent agent instructions file) so every agent that opens the repo knows to use the coordinator without being told:

```markdown
# Agent Coordinator - use it, actively

This workspace is served by the **agent-coordinator**: a lightweight presence + messaging mesh that
lets multiple agents and sessions see each other and collaborate without colliding. When more than
one agent or session may share a workspace, USING IT IS NOT OPTIONAL - it markedly improves speed and
prevents duplicated or conflicting work. The six tools below are exposed via the `agent-coordinator`
MCP server; the `agent-coordinator` CLI provides the `wait` wake-lever.

## Your identity

At SessionStart the coordinator hook injects your name:
`[coordinator] you are '<name>' in this workspace` (an adjective-animal, e.g. `deft-pika`).
If no SessionStart hook ran, call `register_agent` to get a name. Use that exact `<name>` as `from`,
or omit `from` after registering and let the MCP session supply it. Do not grep the filesystem for it.

## The six tools

- `register_agent` - fallback for a session with no hook-assigned identity; do not call when SessionStart assigned a name.
- `status_board` - every agent with name, presence (active / idle / gone), current task, touched files, last activity.
- `list_agents` - who is active or idle right now (presence only).
- `send_message(to, body, from?)` - direct message to one agent by name.
- `read_messages(from?)` - read AND CLEAR your own unread messages.
- `broadcast(body, from?)` - workspace-wide, need-to-know channel. Sparingly - it notifies everyone.

## Wake pattern (be woken, do not busy-poll)

Arm a background task: `agent-coordinator wait '<yourname>' -timeout <sec>` (default 570s). It exits
the moment a DM arrives (or on timeout) and the harness re-invokes you. Treat it as a WAKE SIGNAL,
then confirm with `read_messages` - it can return early on a residual notice, so it is not a precise
timer.

## DO

- On start, or when joining shared work: call `status_board` and `read_messages`, and announce your presence.
- Check the board BEFORE heavy or shared work so you do not duplicate or collide with a peer.
- DM peers to divide work: agree ONE writer per file and disjoint dir/file namespaces up front. This
  alone let ~30 concurrent agents run with zero file conflicts in testing.
- Resolve races deterministically (e.g. alphabetical tie-break on who creates a shared resource).
- Under heavy agent load, COORDINATE load-bearing host operations: elect ONE agent (or take turns) to
  build binaries (`go build`), index a repo, run migrations, or start a dev server. Run concurrently
  these thrash the host (a real incident: an uncoordinated rebuild storm consumed ~17 GB/min);
  coordinated, they are normal-impact.
- Retry with backoff on a transient `daemon unreachable` / socket i/o timeout (seen under ~30-agent load).
- Key durable work artifacts by your stable `agent_id`, not by display name (names can collide).

## DON'T

- Don't broadcast anything that is not genuine need-to-know - it notifies every active agent.
- Don't have N agents independently run the same expensive command (build / index / deploy) - coordinate one.
- Don't reply-all storm; one hello per peer is enough.
- Don't assume a broadcast reached later-spawned agents - broadcasts are one-shot; DM critical directives.
- Don't launch a large recursive subagent fan-out on a quota-limited model without checking headroom.
- Don't trust display-name self-identification under concurrent spawns; verify against `status_board`.

## Subagent identity (current limitation - read if you spawn or are a subagent)

Agent-tool subagents inherit the parent session's id, so the SessionStart hook may NOT mint them a
distinct coordinator name, and peer tools reject an unregistered `from` (`no agent X in this
workspace`). If you are a subagent without a name: use any `you are '<name>'` line in your context;
otherwise call `list_agents` and take the newest active entry that is unmistakably you. For
multi-agent runs prefer a FRESH (non-resumed) session - a resumed session currently fails to register
its subagents into the mesh.
```

The same guide lives at the repo root as `CLAUDE.md`, ready to copy as-is.

## Install

### Release binary (easiest)

Download the binary for your OS from GitHub Releases (linux amd64/arm64,
windows amd64, darwin amd64/arm64), put it on your PATH, then:

```
agent-coordinator install
```

Windows (PowerShell):

```powershell
agent-coordinator.exe install
```

### go install

```
go install github.com/Rooba/agent-coordinator/cmd/agent-coordinator@latest
agent-coordinator install
```

While the repository is private you need `GOPRIVATE=github.com/Rooba`
and git credentials that can read the repo.

### From source

```
git clone https://github.com/Rooba/agent-coordinator
cd agent-coordinator
make install
```

`make install` builds the binary, copies it to
`~/.local/bin/agent-coordinator`, and runs `agent-coordinator install`.

### What `install` does

- merges the five hooks (SessionStart, UserPromptSubmit, PostToolUse, Stop,
  SessionEnd) into `~/.claude/settings.json` - existing hooks are preserved,
  the merge is idempotent, and the write is atomic,
- merges the four lifecycle events currently supported by Codex (SessionStart,
  UserPromptSubmit, PostToolUse, Stop) into `~/.codex/hooks.json`,
- replaces stale `agent-coordinator` MCP registrations for Claude Code, Codex,
  and Grok Build, and merges the local server into OpenCode's global JSON config,
- on Linux with systemd, additionally sets up socket activation
  (`agent-coordinator.socket` + `agent-coordinator.service` user units) as a
  nicety, and `try-restart`s the service so a running daemon picks up a new
  binary.

No systemd is required anywhere: clients start the daemon on demand, so
stock WSL, macOS, and native Windows work out of the box. When `systemctl`
is absent or fails, install prints a note, skips the units, and continues.

Uninstall: `agent-coordinator install --uninstall` (or `make uninstall`).
It removes the units, strips exactly the hooks it added, and deregisters
the MCP server. State in `~/.local/state/agent-coordinator/` is left
behind; delete it by hand for a clean slate.

### Harness support

- Claude Code: MCP plus full five-hook lifecycle tracking.
- Codex: MCP plus native hook tracking. Codex has no SessionEnd event, so stale
  presence is retired by the coordinator's freshness window.
- Grok Build: MCP plus lifecycle tracking through its documented Claude Code
  hook compatibility.
- OpenCode: MCP registration is installed. Automatic activity/file/task tracking
  still requires an OpenCode plugin adapter and is not yet claimed here.

Codex requires new or changed non-managed hooks to be reviewed and trusted with
`/hooks` before they execute.

## The six tools

All under the MCP server `agent-coordinator`. Hook-enabled clients receive an
agent name at session start. MCP-only clients call `register_agent`; after that,
`from` is optional for messaging calls.

- `register_agent` - register this MCP process only when no hook assigned an identity.
- `status_board` - the full workspace board: every coordinated agent with
  name, presence, current task, task counts, latest activity and files.
- `list_agents` - live peers (active or idle) in this workspace.
- `send_message` - direct message to one agent, by name or agent_id.
- `read_messages` - read and clear your unread messages.
- `broadcast` - message every live peer in the workspace at once.

## How it works

One binary, five subcommands:

- `daemon` - owns the SQLite state, serves a line-JSON protocol on a unix
  socket. Started on demand by the other subcommands and exits after 10
  minutes idle, so it only runs while in use.
- `hook` - invoked by supported harness hooks (SessionStart, UserPromptSubmit,
  PostToolUse, Stop, and SessionEnd where available). Forwards the event to
  the daemon and injects any response back into the session as additional
  context. Every error path is a silent no-op: a broken coordinator can
  never break the host agent session.
- `mcp` - stdio MCP server exposing the six peer tools, backed by the
  same socket.
- `wait` - blocks until mail arrives for an agent, for the wake pattern
  (see below).
- `install` - registers all of the above (see Install above).

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

`wait` resolves the workspace scope from its cwd and polls the daemon
(read-only peek, default every 2s) until the named agent has unread mail.
It exits 0 the moment mail arrives, 1 on timeout (default 570s, under
common 600s background caps), 2 on usage error. Peeking never consumes the
once-only notice nudge - the other levers still fire.

An agent blocked on a synchronous subagent has no harness touchpoint and
cannot be woken. But an agent that arms `wait` as a BACKGROUND task before
delegating or idling gets re-invoked by the harness the moment `wait`
exits - i.e. the moment a DM arrives. Arm first, then delegate. The
SessionStart injection teaches every agent this pattern with its own name
filled in.

## Configuration

- `AC_SOCKET` - socket path. Default `$XDG_RUNTIME_DIR/agent-coordinator.sock`;
  if `XDG_RUNTIME_DIR` is unset, a private per-uid directory
  `/tmp/agent-coordinator-<uid>/agent-coordinator.sock` (mode 0700). On
  Windows: `%LOCALAPPDATA%\agent-coordinator\ac.sock`.
- `AC_DB` - database path. Default `~/.local/state/agent-coordinator/coordinator.db`
  (honors `XDG_STATE_HOME`; `%LOCALAPPDATA%\agent-coordinator\coordinator.db`
  on Windows).
- `AC_DEBUG` - when set, the hook logs diagnostics to stderr instead of
  failing silently. Try `AC_DEBUG=1 agent-coordinator hook < event.json`.
- `AC_NO_SPAWN` - when set, clients never spawn the daemon on a missed dial
  and simply fail open. Mostly useful for tests and debugging.

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
`~/.local/state/agent-coordinator/coordinator.db` (see `AC_DB`). The
daemon is the only writer. Housekeeping prunes agents unseen for 7 days,
and messages 7 days after every delivery is read (30 days
unconditionally).

## Known limitations and field findings

Findings from a two-session, ~30-agent stress test:

- Subagent identity - Agent-tool subagents inherit the parent's `CLAUDE_CODE_SESSION_ID`, so
  SessionStart never mints them a distinct coordinator name; a resumed session cannot register its
  subagents into the mesh at all (a fresh session can - see the "Subagent identity" note in the
  pasteable CLAUDE.md above).
- Name collisions - simultaneous sibling spawns can independently land on the same adjective-animal
  name; agents that keyed work files by display name saw those files clobbered between siblings.
  Key durable artifacts by the stable `agent_id` instead.
- `wait` is a wake signal, not a precise timer - it can return early on a residual (already-delivered)
  notice, so treat exit 0 as "check now," not "mail is new," and confirm with `read_messages`.
- Under ~30-agent load the daemon's unix socket produced transient i/o timeouts on `send_message`;
  retry with backoff.
- Broadcasts are one-shot (see Broadcast etiquette above) - an agent spawned after a broadcast fired
  never sees it; DM critical directives instead of relying on broadcast for late joiners.

Proposed fixes are tracked in TODO.md.

## Development

```
make test                     # unit + integration tests
scripts/e2e-messaging.sh      # live E2E: two headless claude sessions
                              # exchange a DM through the coordinator
```

The E2E script requires an installed coordinator and the `claude` CLI.

CI runs the test matrix on Linux, Windows, and macOS. Tags matching `v*`
publish release binaries for linux/amd64, linux/arm64, windows/amd64,
darwin/amd64, and darwin/arm64.
