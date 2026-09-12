# Windows Chrome eyes bridge

[Back to the README](../README.md)

The eyes bridge lets an agent running under WSL hand one browser-observation brief to Claude on the
Windows desktop. The WSL daemon adds an opt-in, authenticated TCP listener; a per-user Windows broker
polls it, runs the provider in the interactive desktop session, validates one structured report, and
delivers that report to the WSL requester. Normal same-workspace coordination does not require this
bridge.

## Quick setup

Run from WSL as the user whose Windows desktop will run Claude and Chrome:

```sh
agent-coordinator eyes-setup --dry-run
agent-coordinator eyes-setup
```

For a new coordinator installation, `agent-coordinator install --eyes` starts the
same setup after installing the normal integrations. Each step reports what it
found or changed. If a prerequisite is missing, setup stops with the action
needed to continue; rerun it after completing that action.

Setup configures the relay service, locates or installs the Windows binary,
pairs it with the relay, checks connectivity, creates working directories, and
configures the per-user broker. It reuses existing setup where possible. Released
builds can fetch the matching Windows release from the repository recorded in
their build metadata.
A source build is a fallback when a checkout and Go toolchain are available.

An unchanged binary with an existing broker task lets setup reuse pairing and skip broker installation;
it does not verify the saved credentials or read back the broker's settings.
Use `--repair` after rotating the relay token or changing the address, model,
or working/config directories to re-pair and apply those settings.

Downloads use HTTPS from GitHub and check a published checksum when available.
The binary's version check confirms compatibility; it does not authenticate the
download. Releases built with the updated workflow also publish `SHA256SUMS`.

Claude sign-in and the Chrome extension smoke check require your interaction.
After verifying that Claude can inspect the intended Chrome profile, finish with:

```sh
agent-coordinator eyes-setup --claude-chrome-ready
```

The flag records your confirmation of that check. Setup reports connection,
relay-token, or WSL forwarding problems with recovery instructions.

Useful overrides:

| Flag | Purpose |
| --- | --- |
| `--dry-run` | Inspect the full plan without installing or downloading |
| `--repair` | Re-pair and repair an existing setup, including after token rotation |
| `--windows-binary PATH` | Supply an existing Windows binary, including for private releases |
| `--addr IP:PORT` | Override the default `127.0.0.1:7400` relay address |
| `--distro NAME` / `--windows-user NAME` | Override detected WSL/Windows identities |
| `--claude-exe PATH` | Choose the Windows Claude executable |
| `--claude-workdir PATH` / `--claude-config-dir PATH` | Choose the broker's working/config directories |
| `--claude-model MODEL` | Override the default eyes model |

Run `agent-coordinator eyes-setup --help` for all options. The manual steps below
are useful for troubleshooting or custom installations.

## Manual setup

### 1. Enable the relay in WSL

The relay is off by default. It accepts only a numeric loopback listener; the production default is
`127.0.0.1:7400`. If `agent-coordinator install` created the systemd user units, add a service drop-in:

```ini
# systemctl --user edit agent-coordinator.service
[Service]
Environment=AC_RELAY_LISTEN=127.0.0.1:7400
```

Then reload and restart it:

```sh
systemctl --user daemon-reload
systemctl --user restart agent-coordinator.service
systemctl --user status agent-coordinator.service
```

Without systemd, run the daemon in a dedicated WSL terminal:

```sh
AC_RELAY_LISTEN=127.0.0.1:7400 agent-coordinator daemon
```

Only one daemon may own the Unix socket. Stop `agent-coordinator.service` and
`agent-coordinator.socket` before using the foreground command if the systemd units are active; if a
client-spawned daemon is already running, let it exit or stop it first.

The daemon creates the relay token only after the TCP listener binds successfully. Its default path is:

```text
~/.local/state/agent-coordinator/relay.token
```

`XDG_STATE_HOME` changes the base directory. If `AC_DB` selects a custom database, `relay.token` is
placed beside that database. The file must remain a regular `0600` file containing the generated
64-character lowercase hexadecimal token. Check its existence and mode, but do not `cat` it, paste it
into a command line, or put it in logs.

### 2. Build and copy the Windows binary

From this repository in WSL:

```sh
make build-windows
mkdir -p /mnt/c/Users/<WindowsUser>/AppData/Local/agent-coordinator
cp agent-coordinator.exe /mnt/c/Users/<WindowsUser>/AppData/Local/agent-coordinator/
```

Run all `host` commands from Windows PowerShell as the same Windows user who will run Claude:

```powershell
$ac = "$env:LOCALAPPDATA\agent-coordinator\agent-coordinator.exe"
```

### 3. Pair and verify Claude in Chrome

Pair directly from the protected WSL file so the token is not printed or placed in argv:

```powershell
$tokenFile = "\\wsl.localhost\<Distro>\home\<WSLUser>\.local\state\agent-coordinator\relay.token"
& $ac host pair --file $tokenFile
Test-NetConnection 127.0.0.1 -Port 7400
```

Older WSL installations may expose the same path under `\\wsl$\<Distro>\...`. Re-run `host pair`
after intentionally rotating the WSL token; pairing preserves the broker's stable launcher identity.

Choose an existing absolute Claude executable, working directory, and dedicated Claude config
directory. The config directory must be outside the working directory. Using that same executable and
config directory, manually run a harmless Claude `--chrome` smoke test and confirm it can inspect the
intended Chrome profile. `--claude-chrome-ready` is only your assertion that this manual test passed;
the coordinator does not probe Chrome during installation.

### 4. Install or run the broker

Pairing is required before installation. In PowerShell:

```powershell
$claudeExe = "C:\absolute\path\to\claude.exe"
$workDir = "C:\absolute\path\to\eyes-workdir"
$configDir = "$env:LOCALAPPDATA\agent-coordinator\claude-config"
New-Item -ItemType Directory -Force $workDir, $configDir | Out-Null

& $ac host install `
  --addr 127.0.0.1:7400 `
  --claude-exe $claudeExe `
  --claude-workdir $workDir `
  --claude-config-dir $configDir `
  --claude-model sonnet `
  --claude-chrome-ready
```

All three paths must already exist, and the executable must be a regular file. `host install` saves
the protected configuration, creates a least-privilege per-user Task Scheduler entry with an
interactive logon token, and starts it immediately. It starts again when that user logs on. Append
`--dry-run` to validate the inputs and print the scheduler plan without saving configuration or
changing Task Scheduler.

Eyes turns run on Sonnet by default; `--claude-model` selects another alias or model id instead of
your account default. Before each turn the broker refreshes the isolated login in
`--claude-config-dir` from your main Claude credentials file (`--claude-credentials`, by default
`%USERPROFILE%\.claude\.credentials.json`) whenever that file is the newer of the two, so signing in
again on your main Claude keeps the bridge working.

For foreground debugging, first persist a valid configuration with `host install`, then stop the
scheduled copy so the single-instance lock is free:

```powershell
& $ac host uninstall
& $ac host run
```

`host run` accepts temporary `--addr`, `--claude-exe`, `--claude-workdir`,
`--claude-config-dir`, `--claude-model`, `--claude-credentials`, and `--claude-chrome-ready`
overrides, but does not save them. Press Ctrl+C to stop it. Re-run `host install` with the full
configuration to restore logon startup.

To inspect or remove the scheduled task:

```powershell
& $ac host uninstall --dry-run
& $ac host uninstall
```

Uninstall ends and deletes the scheduled task. It intentionally retains the protected pairing,
configuration, and task journal.

### Use the bridge from WSL

Run these commands from a registered agent session; the MCP tools with the same names are usually more
convenient inside an agent:

```sh
agent-coordinator workspaces
agent-coordinator eyes
agent-coordinator eyes --tasks

agent-coordinator relay --workspace /path/to/other/repo --to deft-pika --body 'Can you verify this?'
agent-coordinator relay --workspace /path/to/other/repo --agent-id 98ffc675471a --body-file note.md

agent-coordinator request-eyes --runtime claude --timeout 300 --brief 'Inspect the login page and report visible errors.'
agent-coordinator request-eyes --runtime claude --brief-file browser-check.md
agent-coordinator cancel-eyes task-0123456789ab
```

`workspaces` discovers live workspace scope IDs; `eyes` shows connected launchers and active eyes rows.
`relay` can address a whole scope or one name/12-hex agent ID. The daemon stamps an authenticated
`reply_to`; when using the MCP tool, pass that structured `{scope, agent_id, name}` back as `target` to
reply without turning a direct message into a workspace broadcast.

`request-eyes` is always owned by the authenticated caller in its current workspace. It returns a
`task_id` and launcher name, then prints a suggested `wait` command. Launch delivery is retried until
accepted. The requester normally receives `task.accepted`, followed by one `task.result` or
`task.failed`; cancellation is authorized against that recorded requester or its bound child agents. The default deadline is
300 seconds, and an explicit `--timeout` must be 300 through 1800 seconds.

### Follow a running check

Any coordinator in the task's workspace can inspect recent eyes tasks with the
`list_eyes_tasks` MCP tool or the CLI:

```sh
agent-coordinator eyes --tasks
agent-coordinator eyes --tasks --json
agent-coordinator eyes --tasks --mine
```

The view shows task state, elapsed time, remaining deadline, observed model turns,
the latest tool name, and the age of the last heartbeat. `--mine` (MCP: `mine=true`)
limits it to your requests, including your parent's requests when you are a bound
child agent. The same ownership rule controls cancellation. Task listings stay
within the current workspace.

While a provider runs, the broker sends a heartbeat every five seconds, including
when there is no new tool activity. The heartbeat reports broker liveness; it does
not prove that the model or browser is making progress. Turn and tool fields show
observed provider events. A stale heartbeat means the daemon has received no recent
update, so check the broker and connection before assuming the task stopped.

The requester receives occasional progress messages when observed turns advance.
Tool arguments and browser content are excluded from these updates. Findings arrive
in the final structured report; there is no incremental findings backlog or
percentage-complete estimate. A passed deadline without confirmed completion or
cancellation is labelled as unconfirmed, rather than reported as a stopped process.

### Security and v1 boundaries

- The WSL relay binds loopback only and requires the shared token plus per-launcher/per-task session
  secrets. The database stores hashes of session secrets, not the secrets themselves.
- Windows stores broker credentials in the current user's Credential Manager. Broker configuration
  and its small recovery journal are sealed for that user with DPAPI.
- Coordinator credentials and `AC_*` relay variables are removed from the provider child. Claude does
  receive the explicitly configured, isolated `CLAUDE_CONFIG_DIR`, including whatever Claude login
  state that directory contains.
- Browser output is untrusted. The broker bounds and validates the structured report before relaying
  it, and cancellation/deadline handling terminates the provider process tree.
- The scheduled broker runs only in that user's interactive desktop session, never as an elevated
  machine service. This is necessary for access to the user's Chrome session.
- The v1 `host` command is Windows-only and configures one Claude provider, one task at a time. CLI and
  MCP schemas reserve `codex` and `grok` runtime names, but this broker does not advertise them.
- Tasks are one-shot reports. Follow-up conversation, arbitrary remote hosts, non-loopback listeners,
  and TLS are not part of v1.
- Windows-to-WSL `127.0.0.1` depends on WSL localhost forwarding or mirrored networking. If
  `Test-NetConnection` fails, fix that WSL/Windows networking path; v1 deliberately rejects a WSL VM IP
  or other non-loopback `--addr` as an unsafe fallback.
