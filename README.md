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

Paste the block below into your project's `CLAUDE.md` (or equivalent agent instructions file)
so every agent that opens the repo knows to use the coordinator without being told. Keep it in
sync with the root `CLAUDE.md` in this repo:

```markdown
# Agent Coordinator - use it, actively

This workspace is served by the **agent-coordinator**: a lightweight presence + messaging mesh that
lets multiple agents and sessions see each other and collaborate without colliding. When more than
one agent or session may share a workspace, USING IT IS NOT OPTIONAL - it markedly improves speed and
prevents duplicated or conflicting work. The tools below are exposed via the `agent-coordinator`
MCP server; the `agent-coordinator` CLI provides the `wait` wake-lever.

## Your identity

At SessionStart the coordinator hook injects your name:
`[coordinator] you are '<name>' in this workspace` (an adjective-animal, e.g. `deft-pika`).
If no SessionStart hook ran, call `register_agent` (or shell `agent-coordinator join`) to get a name.
Use that exact `<name>` as `from`, or omit `from` after registering and let the MCP session supply
it. Do not grep the filesystem for it.

## The tools

- `register_agent` - fallback only when no SessionStart name; call once.
- `whoami` - this connection's identity (name, agent_id, scope, parent?).
- `status_board` - full detail; hides gone by default (`include_gone=true` to list ghosts).
- `list_agents` - who is active or idle right now (presence only).
- `send_message(to, body, from?)` - DM by name or agent_id; short bodies preferred.
- `read_messages(from?)` - **DESTRUCTIVE**: read AND CLEAR unread. Subagents must pass
  `from='<child name>'` so they do not drain the parent inbox.
- `peek_messages(from?)` - non-destructive unread preview.
- `broadcast(body, from?)` - one-shot to agents registered **now**; late joiners miss it.

## Wake pattern (be woken, do not busy-poll)

Arm a background task: `agent-coordinator wait '<yourname>' -timeout <sec>` (default 570s). It
baselines on your current high-water message id at arm time and exits 0 only when **newer** mail
arrives (stale backlog does not wake). On success stdout is `mail from=... count=N ids=...`; on
timeout `timeout` (exit 1). Treat exit 0 as a WAKE SIGNAL, then confirm with `read_messages`.

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
- Don't launch a large recursive subagent fan-out on a quota-limited model without checking headroom
  (prefer higher-capacity models for big trees; Fable-class quotas can crash fan-outs mid-run).
- Don't trust display-name self-identification under concurrent spawns; verify against `status_board`.

## Subagent identity

Subagent tool events register a **child** coordinator identity (parent linkage on the board).
Child `read_messages` only clears the child inbox. A bare `read_messages` from a subagent is
denied by PreToolUse with the child name to use as `from`. `whoami` is for parent sessions and
foreign harnesses joining without a hook name; on the shared MCP connection it reports the
PARENT identity, so subagents must not rely on it. Subagents get their child name from the
PreToolUse deny reason (it states it) or their spawn context, and must pass `from=<child name>`
on every coordinator call. Key durable artifacts by `agent_id`, not display name.
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

- merges Claude lifecycle hooks into `~/.claude/settings.json` (SessionStart,
  UserPromptSubmit, PreToolUse, PostToolUse, Stop, SubagentStart, SubagentStop,
  SessionEnd) - existing hooks are preserved, the merge is idempotent, and the
  write is atomic (PreToolUse guards subagent inbox drain once the handler lands),
- merges the four lifecycle events currently supported by Codex (SessionStart,
  UserPromptSubmit, PostToolUse, Stop) into `~/.codex/hooks.json`,
- writes Grok Build lifecycle hooks to `~/.grok/hooks/agent-coordinator.json`
  (same set as Claude - dedicated file, not only Claude-compat import),
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
- Grok Build: MCP registration plus dedicated hooks in
  `~/.grok/hooks/agent-coordinator.json` (SessionStart name injection and the
  same lifecycle events as Claude). The hook parser accepts both Claude
  snake_case and Grok camelCase stdin envelopes.
- OpenCode: MCP registration is installed. Automatic activity/file/task tracking
  still requires an OpenCode plugin adapter and is not yet claimed here.

Codex requires new or changed non-managed hooks to be reviewed and trusted with
`/hooks` before they execute.

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

## How it works

One binary, several subcommands:

- `daemon` - owns the SQLite state, serves a line-JSON protocol on a unix
  socket. Started on demand by the other subcommands and exits after 10
  minutes idle, so it only runs while in use.
- `hook` - invoked by supported harness hooks (SessionStart, UserPromptSubmit,
  PostToolUse, Stop, and SessionEnd where available). Forwards the event to
  the daemon and injects any response back into the session as additional
  context. Every error path is a silent no-op: a broken coordinator can
  never break the host agent session.
- `mcp` - stdio MCP server exposing the peer tools, backed by the same socket.
- `wait` - blocks until **new** mail arrives for an agent (see wake pattern).
- `join` - one-shot register + print the SessionStart injection line (for
  harnesses without hooks, or manual bootstrap).
- `board` - print the workspace board (`--live` active+idle only, `--all`
  include gone, `--json` machine-readable).
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

`wait` resolves the workspace scope from its cwd, records the agent's
**high-water** message id at arm time, then polls the daemon (read-only
peek, default every 2s) until an unread message with id strictly greater
than that baseline appears. Stale backlog that was already unread when
`wait` started does **not** wake. Exit 0 prints
`mail from=<names> count=N ids=...`; exit 1 on timeout prints `timeout`
(default 570s, under common 600s background caps); exit 2 on usage error.
Peeking never consumes mail or the once-only notice nudge.

An agent blocked on a synchronous subagent has no harness touchpoint and
cannot be woken. But an agent that arms `wait` as a BACKGROUND task before
delegating or idling gets re-invoked by the harness the moment `wait`
exits - i.e. the moment **new** mail arrives. Arm first, then delegate. The
SessionStart injection (or `agent-coordinator join`) teaches every agent
this pattern with its own name filled in.

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

Addressed by the 2026-08-05 pair work (see `docs/IMPROVEMENTS-2026-08-05-pair-session-retro.md`):
hook/MCP identity unification via bind files, subagent child identities + PreToolUse drain
guard, wait high-water baseline, board hide-gone + 2h GC, Grok hooks install, whoami /
peek_messages, richer notice previews.

Remaining field notes:

- Name collisions - simultaneous sibling spawns can still land on the same adjective-animal
  base before suffixing; key durable artifacts by the stable `agent_id` instead of display name.
- Under ~30-agent load the daemon's unix socket produced transient i/o timeouts on `send_message`;
  retry with backoff.
- Broadcasts are one-shot (see Broadcast etiquette above) - an agent spawned after a broadcast fired
  never sees it; DM critical directives instead of relying on broadcast for late joiners.

Further backlog (claims ledger, message journal, shared LSP) is tracked in the retro doc and TODO.md.

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
