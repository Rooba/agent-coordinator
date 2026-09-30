---
name: agent-coordinator
description: Use when more than one agent or session shares a workspace - to see who is working on what (presence board), divide work and deconflict files over direct messages, broadcast need-to-know notices, or wait for a peer's reply with wake-on-mail instead of polling.
---

# Agent Coordinator

A presence and messaging mesh for agents sharing a workspace. Using it is what keeps
parallel agents from duplicating or clobbering each other's work.

## Identity

At session start a hook injects `[coordinator] you are '<name>' in this workspace`.
Use that exact name as `from` (or omit `from` and let the connection supply it). Only if no
such line appeared, call `register_agent` once. Never search the filesystem for your name.

## Tools

Under this plugin the tools are named `mcp__plugin_agent-coordinator_agent-coordinator__<tool>`.

- `status_board` - every agent with presence, current task, touched files, last activity.
- `list_agents` - who is active or idle right now.
- `send_message(to, body, from?)` - DM by name or agent_id; keep bodies short.
- `read_messages(from?)` - reads AND CLEARS your unread mail. `peek_messages` previews without clearing.
- `broadcast(body, from?)` - one-shot to agents registered now; late joiners miss it.
- `claim` / `release` / `list_claims` - advisory ownership ledger for shared files.
- `message_history` - audit sent and received mail without clearing anything.
- `whoami`, `register_agent` - identity checks and fallback registration.

## Wake pattern (do not busy-poll)

When waiting on a peer, run the exact `wait` command the SessionStart line prints (under the
plugin the binary is not on PATH, so that line carries its full path). In Claude Code, arm it
as a BACKGROUND task: a new turn starts when it exits.

- Stderr prints `armed after_id=N` once its durable cursor is set; no mail is lost across a
  kill or timeout.
- Exit 0 (`mail from=... count=N`) is a wake signal: call `read_messages`, act, then re-arm.
- Exit 1 (`timeout`): re-arm, lengthening the timeout as work quiesces (570 -> 900 -> 1500s).
- Idling in multi-agent work without an armed wait is a bug.

## Do

- On joining shared work: `status_board`, `read_messages`, then announce yourself to peers.
- Agree ONE writer per file and disjoint directories up front, before editing.
- Elect one agent to run expensive host operations (builds, indexing, migrations, dev servers).
- Put long content (plans, surveys) in a file under `.ignore/coordination/` and DM the path.
- Retry with backoff on a transient `daemon unreachable`.
- Key durable artifacts by your `agent_id`, not your display name.

## Don't

- Don't broadcast anything that is not genuine need-to-know, and don't reply-all.
- Don't let several agents run the same expensive command at once.
- Don't assume a broadcast reached agents that joined later; DM critical directives.
- Don't trust display-name self-identification under concurrent spawns; check `status_board`.

## Subagents

Subagents share the parent's MCP connection. They must pass `from='<child name>'` on every
coordinator call, taking the name from their spawn context or the hook message that states it.
A bare `read_messages` from a subagent would drain the parent inbox, so it is denied.

## Cross-workspace relay and eyes

`list_workspaces` finds other scopes with live agents; `relay(body, target)` messages them,
preferring the `reply_to` target returned with their mail. On WSL with the optional Windows
bridge paired, `request_eyes(brief)` asks a host agent to inspect Chrome and mails back a
one-shot report (arm `wait` meanwhile); `list_eyes`, `list_eyes_tasks`, and `cancel_eyes`
manage it.
