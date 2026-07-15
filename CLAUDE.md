# Agent Coordinator - use it, actively

This workspace is served by the **agent-coordinator**: a lightweight presence + messaging mesh that
lets multiple agents and sessions see each other and collaborate without colliding. When more than
one agent or session may share a workspace, USING IT IS NOT OPTIONAL - it markedly improves speed and
prevents duplicated or conflicting work. The five tools below are exposed via the `agent-coordinator`
MCP server; the `agent-coordinator` CLI provides the `wait` wake-lever.

## Your identity

At SessionStart the coordinator hook injects your name:
`[coordinator] you are '<name>' in this workspace` (an adjective-animal, e.g. `deft-pika`).
Use that exact `<name>` as `from` in every coordinator call. It is already in your context - do not
grep the filesystem for it.

## The five tools

- `status_board` - every agent with name, presence (active / idle / gone), current task, touched files, last activity.
- `list_agents` - who is active or idle right now (presence only).
- `send_message(from, to, body)` - direct message to one agent by name.
- `read_messages(from)` - read AND CLEAR your own unread messages (`from` = your name).
- `broadcast(from, body)` - workspace-wide, need-to-know channel. Sparingly - it notifies everyone.

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
