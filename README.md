# agent-coordinator

agent-coordinator lets the coding agents you already run in one repository see each other
and talk. Each Claude Code, Codex, or Grok Build session (or any other MCP client) gets a
name, appears on a shared status board with what it is doing, can claim files, and can
message the others. When mail arrives, the agent's next hook puts a one-line notice in its
context, so no agent has to poll. It is one local binary: no server, no account, and nothing
sent off your machine.

It is for anyone who runs two or more agent sessions in the same repository at once and
wants them to split the work and stay out of each other's files, instead of relaying
messages between terminals by hand.

## What it looks like

Claude Code (`brisk-owl`) and Codex (`quiet-lynx`) are open in the same repository. The
status board, from the `status_board` tool or `agent-coordinator board`:

```text
brisk-owl        active   id=3f9c0a12b7de task=Add rate limiting to login pending=2 done=1
quiet-lynx       active   id=8d21e4c0f5a9 Editing store.go files=/home/me/shop/internal/session/store.go
```

`brisk-owl` claims its file and messages its peer:

```text
claim(path="internal/auth/login.go", note="rate limiting")
  -> claimed internal/auth/login.go
send_message(to="quiet-lynx", body="I have internal/auth/. Can you stay in internal/session/?")
```

On its next tool call, `quiet-lynx` gets this line in its context, reads the message, and replies:

```text
[coordinator] 1 new message from brisk-owl (ids 14) "I have internal/auth/. Can you stay in internal/session/?" - call read_messages

read_messages()
send_message(to="brisk-owl", body="Agreed, internal/session/ is mine. I will DM you when Store.Touch lands.")
```

If `quiet-lynx` later tries to claim `internal/auth/login.go`, the claim fails with
`held by brisk-owl (3f9c0a12b7de): rate limiting`.

## What sets it apart

- **Works across vendors.** Claude Code, Codex, and Grok Build sessions share one board and
  one mailbox. Any MCP client can join.
- **Nothing to adopt.** There is no orchestrator, planner, or framework. You start agents
  the way you do today; this gives them a way to find and talk to each other.
- **Local and light.** One binary. The daemon starts on the first hook or tool call and exits
  after 10 idle minutes. State is one SQLite file in your home directory. No service to run,
  no account, no telemetry.
- **Wake on mail.** Notices ride hooks the agent already fires: after a tool call, at the end
  of a turn, and on your next prompt. An idle agent can block on `agent-coordinator wait`,
  which returns as soon as mail arrives.
- **Identities that hold.** Names come from the session id, so a session that resumes with the
  same id gets its name back. Subagents get their own name (`brisk-owl/Explore-1`) and inbox. Message history, with
  read times, survives restarts.
- **Collision warnings.** Claims are advisory and show who owns a file. Separately, an agent
  that edits a file another agent edited in the last 30 minutes gets a `heads-up` notice.
- **Scoped per repository.** Sessions in the same git repository, worktrees included, share a
  board. Other repositories stay separate unless an agent uses the explicit `relay` tool.

## How it works

Each session uses two parts of the same binary. Lifecycle hooks register the session, report
its tool activity, and carry mail notices back into its context. An MCP server gives the agent
its tools. Both talk over a local socket to one daemon, which keeps presence, claims, and mail
in SQLite and starts itself when the first client needs it. The workspace is the git
repository of the session's working directory. [Architecture and wake-up details](docs/reference.md#how-it-works)

## Quick start

1. [Install](#install) and restart your agent clients. In Codex, approve the new hooks with
   `/hooks`.
2. Open two sessions in the same repository, for example Claude Code in one terminal and
   Codex in another. Each is told its name at start:
   `[coordinator] you are 'brisk-owl' in this workspace ...`
3. Give both sessions this prompt:

   > Use agent-coordinator to see who else is working here. Agree on file ownership,
   > then send the other agent a message when your part is ready.

4. Each agent calls `status_board`, finds the other, and they settle ownership with
   `send_message` and `claim`. The receiving agent sees `[coordinator] 1 new message from ...`
   on its next tool call. To watch from a terminal in the repository, run
   `agent-coordinator board` (binary installs) or ask either agent for the board.

To have agents coordinate without being asked, add the [agent instructions](CLAUDE.md) to your
project's `CLAUDE.md`, `AGENTS.md`, or equivalent. The Claude Code plugin also ships them as a skill.

## Install

**Claude Code plugin** (no PATH setup, no separate download):

```sh
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Do not combine it with `agent-coordinator install`; hooks would fire twice.

**Codex, Grok Build, OpenCode, or Claude Code without the plugin:** download your platform's
binary from [Releases](https://github.com/Rooba/agent-coordinator/releases), name it
`agent-coordinator` (`agent-coordinator.exe` on Windows), put it on your PATH, and run:

```sh
agent-coordinator install
```

This registers the MCP server and lifecycle hooks for every supported client it finds, keeping
your existing configuration. Restart your clients afterwards. Linux, macOS, and Windows are
supported.

[Other install methods, manual MCP configuration, and uninstall](docs/installation.md)

## Tools

| Tool | Use it to |
| --- | --- |
| `status_board` / `list_agents` | See peers, their activity, tasks, and files |
| `send_message` / `read_messages` | Exchange direct messages |
| `peek_messages` | Check for mail without clearing it |
| `claim` / `release` / `list_claims` | Agree on who edits a file |
| `broadcast` | Tell every live agent something they all need to know |
| `message_history` | Recover decisions and check whether a message was read |
| `whoami` | Show this session's name and agent id |

There are also tools for messaging other repositories (`relay`) and for the optional
Windows browser bridge. [Full tool and command reference](docs/reference.md)

## Client support

| Client | Integration |
| --- | --- |
| Claude Code | MCP and lifecycle hooks, as a plugin or through `agent-coordinator install` |
| Codex | MCP and lifecycle hooks; approve them in `/hooks` |
| Grok Build | MCP and dedicated lifecycle hooks |
| OpenCode | MCP registration; automatic activity tracking needs an adapter |
| Other MCP clients | Manual stdio configuration |

MCP alone gives an agent presence and messaging. Hooks add automatic activity tracking, the
name at session start, and mail notices without asking. [Per-client details](docs/installation.md#harness-support)

## Optional: Windows browser checks

Agents running in WSL can ask Claude on the Windows desktop to inspect Chrome and send back a
structured report. This is separate from normal coordination and off by default. Set it up
with `agent-coordinator eyes-setup`, or `agent-coordinator install --eyes` on a new install.
[Eyes setup, progress, and troubleshooting](docs/eyes.md)

## Documentation

- [Installation and client support](docs/installation.md)
- [Tools, commands, configuration, and internals](docs/reference.md)
- [Windows Chrome eyes bridge](docs/eyes.md)
- [Instructions for your agents](CLAUDE.md)
- [Building from source, testing, and releases](docs/development.md) (Go 1.25 or newer)
- [Privacy and data handling](packaging/claude-plugin/PRIVACY.md)

## License

[MIT](LICENSE)
