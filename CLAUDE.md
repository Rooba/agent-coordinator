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
