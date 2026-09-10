# agent-coordinator - findings, fixes, and improvements

## 2026-09-06 - relay follow-ups: status after the three-coordinator pass

Source: the three-session relay build (bold-ibis/Claude core, eager-crane/Grok CLI+MCP,
solid-mole broker), landed on main through 8ec17b6. Full ruling ledger and review reports are
archived (git-ignored) under `.ignore/coordination/bold-ibis-sdd-archive/`.

**Status 2026-09-06 (later the same day): worked by lucid-fox/Claude, amber-crane/Grok,
lucid-raven/Codex.** Everything below marked DONE landed with tests in one uncommitted tree
(see `git status`); the eyes bridge was set up for real on this host.

### Coordinator bugs
- DONE (amber-crane) - `register_agent` on a resumed MCP connection now always re-registers,
  writes the bind file and marks the connection bound, so `whoami`/`claim`/`send_message`
  work after the 2h GC purged the row. Tests: `TestRegisterAgentReRegistersCachedIdentity`,
  `TestRegisterAgentRebindsAgedOutRow`, `TestRegisterAgentBindSurvivesProcessRestart`.
- DONE (lucid-raven) - `agent-coordinator wait` persists an atomic 24h per-scope/name cursor,
  so a wait killed by the harness watchdog or a timeout gap loses no mail on re-arm; it prints
  `armed after_id=N` only after the durable arm. Wakes are at-least-once: the cursor advances
  only after the mail is consumed and a later quiet peek sees it, never on the wake itself.
  The wait is still a process per waiter and can still be killed: re-arm it. Caveat: a
  first-ever arm baselines mail that arrived before the durable ack.
- DONE (lucid-fox) - socket split-brain: MCP shims and hooks launched without
  `XDG_RUNTIME_DIR` (about half of the live shims on this host) dialed
  `/tmp/agent-coordinator-<uid>/` and spawned a SECOND daemon beside the systemd one, sharing
  the DB but not in-memory state (relay listener, eyes tasks, wakes). `defaultSocket` now falls
  back to `/run/user/<uid>` when that directory exists and is ours, before `/tmp`.
- DONE (lucid-raven) - Codex child identity: Codex has no PreToolUse/SubagentStart hooks, so
  its subagents got no deny-reason name injection and guessed `from=parent/default-N`.
  PostToolUse already registers the child; the daemon now returns that child name and the
  hook emits it as additionalContext (`use from=<child>`). Limitation: the name arrives
  after the child's first tool call, so a coordinator call made before that is unattributed.
- DONE (lucid-raven) - `paths_windows.go` resolves the socket dir from the canonical
  `FOLDERID_LocalAppData` known folder instead of the caller's environment, with no temp
  fallback; an unresolvable directory exits with `AC_SOCKET` guidance instead of forming a
  second mesh.

### Relay polish
- DONE - `eyesRole` launcher arm compares `(launcher_scope, launcher_session)`; tasks record
  `launcher_scope` (new column). Test: a launcher session re-registered in another `host:`
  scope cannot ack the task. `ReissueEyesChild` binds to the same pair.
- DONE (lucid-raven) - one `internal/taskid.Valid` replaces the three `validTaskID` copies.
- DONE - cancel path resolves the requester once and threads a `Sender` through `Delivery`;
  `sendToScope` dropped its separate kind query.
- DONE - `taskMessageType` duplicate-`type` coverage confirmed in
  `internal/daemon/relay_test.go` (`TestPlainMailCannotSpeakForATask`,
  `TestTaskReportsRequireStrictEnvelopeAndCanonicalTarget`).
- DONE - spec sections 4, 7 and 10 updated (gate answers only `unauthorized`; `task.result`
  status enum `succeeded|failed` with the error rule; `launcher_scope` in the ALTER list).

### Deployment: exercised for real on this host (2026-09-06)
- Relay listener: systemd drop-in `~/.config/systemd/user/agent-coordinator.service.d/relay.conf`
  sets `AC_RELAY_LISTEN=127.0.0.1:7400`; the token is minted at
  `~/.local/state/agent-coordinator/relay.token`.
- Windows: `agent-coordinator.exe` in `%LOCALAPPDATA%\agent-coordinator\`, `host pair --file`
  over `\\wsl.localhost\openSUSE-Tumbleweed\...` succeeded, `Test-NetConnection 7400` true.
- FIXED - `host install` failed with "task XML is malformed / unable to switch the encoding":
  `schtasks /XML` requires UTF-16; the task XML is now written UTF-16 LE with BOM. After the
  fix the scheduled task `Agent Coordinator Host Broker-<hash>` runs and the launcher shows in
  `agent-coordinator eyes` as `deft-newt` on `host:RAMEN`.
- Claude eyes provider: WinGet `claude.exe`, workdir `%LOCALAPPDATA%\agent-coordinator\eyes-workdir`,
  config dir `...\claude-config` seeded with the user's Claude `.credentials.json`
  (Claude needs login state in the isolated config dir).
- FIXED - first live turn failed: `--tools ""` strips the Chrome tools before the allow list
  applies, so every `mcp__claude-in-chrome__*` call was denied under `dontAsk`; the flag is
  gone (dontAsk + `--allowedTools mcp__claude-in-chrome` already denies everything else).
  The model's `failed` report also carried no `error`, which the runner rejected; a failed
  report now takes its summary as the error. After the final deploy a real `request-eyes`
  through the scheduled broker returned `task.result status=succeeded` (heading "Example
  Domain" observed, console clean, tab closed). The `total_cost_usd` Claude prints (about
  $0.25-0.50) is its list-price estimate for ~190k mostly cached tokens; the seeded login is a
  Max subscription, so the turn draws from that allowance, not API billing.
- DONE - eyes turns run Sonnet by default (`--claude-model`, persisted as `claude_model`); the
  account default model was being used before. The broker also refreshes the isolated
  `.credentials.json` from `%USERPROFILE%\.claude\.credentials.json` (`--claude-credentials`)
  before each turn whenever the main file is newer, so a re-login propagates by itself.
- Codex runner remains internal-only; Grok absent.

### Deferred by design (revisit when needed)
- v1.1 bidirectional eyes: broker keeps the model's stdin open (`--input-format stream-json`),
  forwards inbound DMs as user turns and assistant finals as DMs, still with zero credentials in
  the child. Needs a Chrome-tool-in-stream-json probe first.
- Per-task minted relay tokens and TLS on the loopback listener (spec section 12 deferrals).
- Windows `C:\` <-> `/mnt/c` scope mapping (eyes join by explicit WSL scope in v1).
- Manual `join -kind eyes` over TCP (eyes rows are minted only by a launcher's reissue).

---

**Status 2026-08-05: retro backlog IMPLEMENTED** (pair: solid-mole/Claude + nimble-raven/Grok).
All P0 (subagent identities + drain guard, hook/MCP identity unification via bind files,
Grok bootstrap + join + whoami, wait high-water baseline), all P1 (board hides gone + 2h GC,
MCP heartbeat, tool copy, notice previews, agent_id visibility), and P2.1/P2.2 (claims
ledger, message journal) landed with tests; P2.3 stayed the documented .ignore/coordination
file-drop convention; P3.1 shared LSP hub parked by design. A post-implementation review
pass (10 findings, incl. bind-match selectivity, subagent whoami drain, purged-sender mail
visibility) was fixed in the same session. See
[`docs/IMPROVEMENTS-2026-08-05-pair-session-retro.md`](docs/IMPROVEMENTS-2026-08-05-pair-session-retro.md)
for the original backlog and acceptance criteria.

---

## Historical: 2026-07-12 stress test

Source: a live two-coordinator stress test (2026-07-12). Two Claude Code sessions
(`solid-heron`, `proud-raven`) each drove a 3-2-1 subagent tree (15 agents/tree, 30 total)
in one shared workspace, coordinating entirely through the coordinator. Co-authored by both
coordinators. File references point at the real source so fixes are actionable.

## P1 - Resumed-session subagents lose coordinator identity (highest impact)
- Observed: after a Claude Code process crash + resume, respawned Agent-tool subagents could
  not use the coordinator at all. Every `send_message` / `read_messages` rejected their `from`
  with "no agent <name> in this workspace"; 0/15 files written, no announces. Pre-crash
  subagents and the OTHER (un-crashed) session's subagents worked fine - so it is resume-specific.
- Root cause: agents are named by `friendlyName()` = FNV hash of `session_id`
  (`internal/store/names.go`), assigned at register time via the SessionStart hook
  (`internal/hookcli/hookcli.go`, which emits "[coordinator] you are '<name>'"). Agent-tool
  subagents inherit the PARENT's `CLAUDE_CODE_SESSION_ID`, so on a resumed session the hook
  does not fire a distinct per-subagent registration. The subagent then has no known name and
  there is no self-registration path in the MCP surface (only status_board / list_agents /
  send_message / read_messages / broadcast). Notably the daemon ALREADY receives per-subagent
  identity: the `internal/hookcli/hookcli.go` spike comment records verbatim that "subagent tool
  calls arrive under the PARENT session_id with these two extra fields set (agent_id, agent_type);
  parent-session events lack them" - but today those fields only TAG activity as "(subagent: X)"
  under the parent row instead of minting a distinct registration.
- Fixes:
  - Register subagents from the fields the daemon ALREADY sees: when `agent_type` is set, mint a
    distinct identity keyed on (parent `session_id` + `agent_id`) instead of only tagging activity.
    This is the lowest-effort fix and closes P1 directly.
  - Add a `whoami` MCP tool returning the caller's assigned name. Since the name is
    deterministic from `session_id`, this is a thin lookup and kills the chicken-and-egg.
  - Add a `register` / `join` MCP tool, OR auto-register an unknown `from` (rate-limited),
    so an agent can self-register when the hook did not run.
  - Derive identity from a per-agent stable token (parent `session_id` + the Agent task id /
    tool_use id), not `session_id` alone, so subagents get distinct names even under a shared
    session id - and inject the name at spawn time, not only via a hook that can miss on resume.

## P2 - Name collisions under concurrent spawns cause churn AND file clobbering
- Observed: under rapid parallel spawns a role label cycled through multiple names
  (L2a1: lucid-newt -> hardy-ibis; L3a1: frank-ibis -> bold-crane-3) and, worse, DISTINCT agents
  independently picked the SAME name (two `solid-owl`, two `vivid-pika`, two `keen-badger`).
  Because agents named their work files by display name, colliding names CLOBBERED each other's
  files within a tree (contents flipped between scans); one agent had to self-disambiguate manually.
- Root cause: `friendlyName` is a pure hash of `session_id` with `-2` / `-3` suffixing on
  scope collision; concurrent registrations that hash to the same base race on the suffix, and
  nothing guarantees global uniqueness at assign time.
- Fixes: guarantee globally-unique names atomically at register time (serialize behind the store
  lock in `internal/store/store.go`); key the hash on a stable per-agent token (see P1); and
  expose `agent_id` so agents key artifacts by stable id, not race-prone display name.

## P3 - Fable-5 model quota wall crashes multi-agent trees
- Observed: trees spawned on `model=fable` churned and died mid-run on a Fable-5 usage limit
  (not logic - same labels respawning under new names). Both trees hit it; both moved to Sonnet.
- Note: not a coordinator bug, but a multi-agent operational hazard worth surfacing in docs.
- Mitigations: recommend Sonnet/Opus for large fan-outs and reserve Fable for short tasks;
  optionally let the spawner set a per-tree model budget / fallback model; surface a clear
  "model quota exhausted" signal to the parent instead of silent churn.

## P4 - Daemon unreachable under load
- Observed: with ~30 agents messaging, the unix socket i/o-timed-out once (recovered on retry).
- Fixes: raise the socket accept backlog / handler concurrency in the daemon
  (`cmd/agent-coordinator/daemon.go`); add a small retry-with-backoff in the MCP shim
  (`internal/mcpserv`); document retry-with-backoff for clients and a recommended max
  concurrent-agents-per-daemon ceiling.

## P5 - No self-identification without the hook name
- `status_board` / `list_agents` return every row, but an agent cannot tell which row is
  itself without already knowing its assigned name. Resolved by the `whoami` tool in P1.

## P6 - Broadcast delivery semantics undocumented
- Observed: unclear whether `broadcast` is push-to-currently-active only or persisted for
  agents that connect later, so "received by every agent" is timing-dependent.
- Fixes: document the semantics explicitly; optionally persist broadcasts for a short TTL so
  late-joining agents pick them up on their next `read_messages`.

## P7 - `wait` CLI can return early without a message (false wake)
- Observed: `agent-coordinator wait '<name>'` sometimes returned immediately (exit 0, empty) with
  no fresh mail, so it is a wake SIGNAL, not a dependable idle-sleep.
- Root cause (`cmd/agent-coordinator/wait.go`): it polls read-only `OpPeek`, which does NOT
  consume the once-only Stop-hook notice - so it keeps re-seeing that persistent notice and exits 0.
- Fixes: block reliably until genuinely new mail or the timeout; do not count the persistent Stop
  notice as fresh mail; document `-timeout` (default 570s) / `-interval` (2s) behavior.

## General improvements
- `whoami` plus an optional `who <name>` (resolve any name -> presence / last activity) for
  cleaner steering.
- Optional lightweight message rate-limit / dedupe to blunt O(N^2) hello storms in big pools.
- A `--json` machine-readable board for programmatic coordinators.
- Surface the wake pattern (`agent-coordinator wait '<name>'`) prominently - it is the key to
  being woken on a DM instead of busy-polling.

## What already works well (keep)
- Spawn-on-demand daemon, deterministic friendly names, the presence board, DMs, scoped
  broadcast, and the `wait` wake pattern all performed reliably. Cross-session coordination
  (worktree ownership hand-off, disjoint file namespaces, ownership deconfliction, dual scoped
  broadcasts) had ZERO conflicts and ZERO cross-tree file contamination across 30 agents.
