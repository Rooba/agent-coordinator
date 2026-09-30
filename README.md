# agent-coordinator

Let your coding agents see who is working, exchange messages, and coordinate changes.

Agent Coordinator connects concurrent Claude Code, Codex, Grok Build, and other MCP
sessions in the same repository. It runs locally as a single binary, starts on
demand, and shuts down when idle.

- **See who's doing what:** presence, current activity, tasks, and file claims.
- **Coordinate work:** direct messages, broadcasts, and wake-on-mail.
- **Keep context across sessions:** durable message history and stable agent identities.
- **Track lifecycle changes:** session, tool, subagent, and compaction events from supported hooks.
- **Ask Windows to check Chrome:** an optional eyes agent reports browser observations back to WSL.

## Install

**Claude Code plugin** (no PATH setup, no separate download):

```sh
claude plugin marketplace add https://github.com/Rooba/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator
```

Do not combine it with `agent-coordinator install`; hooks would fire twice.

**Other clients, or Claude Code without the plugin:**

Download your platform's binary from [Releases](https://github.com/Rooba/agent-coordinator/releases),
name it `agent-coordinator` (`agent-coordinator.exe` on Windows), and put it on your PATH. Then run:

```sh
agent-coordinator install
```

The installer adds MCP registrations and lifecycle hooks for supported clients,
preserving existing configuration. Restart your client after installation. In
Codex, review and trust the new hooks with `/hooks`.

Linux, macOS, and Windows are supported. No separate database or system service
setup is required. Private releases require access to the repository.

[Other installation methods, manual MCP configuration, and uninstall](docs/installation.md)

## Try it

Open two coding-agent sessions in the same repository and ask:

> Use agent-coordinator to see who else is working here. Agree on file ownership,
> then send the other agent a message when your part is ready.

For automatic use, add the [agent instructions](CLAUDE.md) to your project's
`CLAUDE.md`, `AGENTS.md`, or equivalent.

The main tools are:

| Tool | Use it to |
| --- | --- |
| `status_board` | See peers, activity, tasks, and claimed files |
| `send_message` / `read_messages` | Exchange direct messages |
| `claim` / `release` | Agree on who edits a file |
| `message_history` | Recover decisions and check message delivery |
| `list_eyes` / `request_eyes` | Find a Windows browser agent and request a check |

[Full tool and command reference](docs/reference.md)

## Client support

| Client | Integration |
| --- | --- |
| Claude Code | MCP and lifecycle hooks |
| Codex | MCP and lifecycle hooks; approve them in `/hooks` |
| Grok Build | MCP and dedicated lifecycle hooks |
| OpenCode | MCP registration; automatic lifecycle tracking needs an adapter |
| Other MCP clients | Manual stdio configuration |

Agents share a workspace based on its working directory. Separate repositories
stay separate; explicit relay tools let agents communicate across workspaces.

## Optional Windows browser checks

For agents running in WSL, the eyes bridge can ask Claude on your Windows desktop
to inspect Chrome and return a report. Setup is available during installation or later:

```sh
agent-coordinator install --eyes
agent-coordinator eyes-setup
```

The setup command automates the relay, pairing, and broker configuration. It
explains any remaining Claude sign-in or Chrome readiness step. Eyes tasks expose
state and observed progress so peers can follow a long-running check.

[Eyes setup, progress, and troubleshooting](docs/eyes.md)

## Documentation

- [Installation and hook support](docs/installation.md)
- [Tools, configuration, presence, and messaging](docs/reference.md)
- [Windows Chrome eyes bridge](docs/eyes.md)
- [Instructions for your agents](CLAUDE.md)
- [Development and testing](docs/development.md)

## Development

Requires Go 1.25 or newer:

```sh
go test -race ./...
go vet ./...
```

[Build from source and run integration checks](docs/development.md)
