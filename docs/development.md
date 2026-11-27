# Development

[Back to the README](../README.md)

This page is for working on agent-coordinator itself: building and testing it, running the
live end-to-end check, trying the Claude Code plugin from a checkout, cutting a release, and
the transport benchmark. To use the tool, see [Installation](installation.md) instead.

## Quick path

Requires Go 1.25 or newer. From the repository root:

```sh
make build
go vet ./...
go test -race ./...
```

## Tests

```sh
make test                     # unit + integration tests
scripts/e2e-messaging.sh      # live E2E: two headless claude sessions
                              # exchange a DM through the coordinator
```

The E2E script requires an installed coordinator and the `claude` CLI.

CI runs the test matrix on Linux, Windows, and macOS. Tags matching `v*`
publish release binaries for linux/amd64, linux/arm64, windows/amd64,
darwin/amd64, and darwin/arm64.

## Try the plugin monitor from source

Use an interactive session. The published npm package will not contain
`watch` until the next release, so point the plugin launcher at a binary
built from this tree. The launcher looks up
`${CLAUDE_PLUGIN_ROOT}/node_modules/agent-coordinator-cli-<os>-<cpu>/agent-coordinator`
(`linux-x64`, `linux-arm64`, `darwin-x64`, `darwin-arm64`, or `win32-x64`;
`.exe` on Windows). That `node_modules` directory is gitignored.

```sh
mkdir -p packaging/claude-plugin/node_modules/agent-coordinator-cli-linux-x64
go build -o packaging/claude-plugin/node_modules/agent-coordinator-cli-linux-x64/agent-coordinator ./cmd/agent-coordinator
```

If `agent-coordinator install` already merged hooks into `~/.claude/settings.json`,
remove them first or every hook fires twice:

```sh
agent-coordinator install --uninstall
```

Remove a previously installed copy of this plugin, then add this repo as a
marketplace and install the plugin at user scope (the marketplace name is
`agent-coordinator`):

```sh
claude plugin uninstall agent-coordinator@agent-coordinator
claude plugin marketplace add /absolute/path/to/agent-coordinator
claude plugin install agent-coordinator@agent-coordinator --scope user
```

A marketplace added from a local path loads the plugin in place, so the next
session start or `/reload-plugins` picks up source edits. The default monitor
trigger starts on that reload. A copied plugin that updates mid-session keeps
the previous monitor binary until the next session.

The daemon already running is the old binary until it is stopped. That
process ignores the watcher flag, so `wake` stays `manual`. There is no
restart subcommand. Stop the listener on the socket, then let the new
binary spawn the replacement:

```sh
sock="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/agent-coordinator.sock"
[ -S "$sock" ] || sock="/tmp/agent-coordinator-$(id -u)/agent-coordinator.sock"
if [ -S "$sock" ]; then kill "$(lsof -t -- "$sock" | head -1)"; fi
packaging/claude-plugin/node_modules/agent-coordinator-cli-linux-x64/agent-coordinator board >/dev/null
```

After SessionStart writes the bind file, the task panel shows `mail`
("coordinator mail: DMs from peer agents") and `whoami` reports
`wake: monitor`. If that task is absent, the host skipped monitors: arm
`wait` as before.

## Releasing a new plugin version

1. Bump `version` in `packaging/claude-plugin/.claude-plugin/plugin.json`,
   `.claude-plugin/marketplace.json`, and `packaging/claude-plugin/package.json`
   (its `version` and every `optionalDependencies` entry).
2. Tag `v<version>` and push the tag. The release workflow fails if the tag and
   manifests disagree; otherwise it builds the release and, when the repository
   variable `NPM_PUBLISH` is `true`, publishes the npm packages.
3. Run `make plugin-lock`, commit `packaging/claude-plugin/package-lock.json`, push,
   and resubmit the plugin to the directory.

The lockfile can only be generated once the packages for that version are published.

## Transport workload

The daemon tests include thirty concurrent clients sending messages and reading
the status board over Unix sockets and loopback TCP:

```sh
go test ./internal/daemon -run '^TestTransportConcurrentMailAndBoard$' \
  -bench '^BenchmarkTransportConcurrent$' -benchtime=300x -count=2
```

On Linux/amd64 during the September 11, 2026 comparison, replacing the board's
151 database calls with one snapshot query for thirty agents reduced aggregate
time per board operation from about 7.5 ms to 0.57-0.60 ms. Both transports
improved similarly, and the test verified message counts and uniqueness.

These are local throughput measurements, not individual request latencies. The
test gives both transports the same trusted handler path to isolate socket cost;
it does not benchmark relay authentication or reproduce the historical timeout.
See [transport configuration](reference.md#local-transport-and-tcp).
