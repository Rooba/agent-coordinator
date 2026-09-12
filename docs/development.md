# Development

[Back to the README](../README.md)

Requires Go 1.25 or newer. From the repository root:

```sh
make build
go vet ./...
go test -race ./...
```

```
make test                     # unit + integration tests
scripts/e2e-messaging.sh      # live E2E: two headless claude sessions
                              # exchange a DM through the coordinator
```

The E2E script requires an installed coordinator and the `claude` CLI.

CI runs the test matrix on Linux, Windows, and macOS. Tags matching `v*`
publish release binaries for linux/amd64, linux/arm64, windows/amd64,
darwin/amd64, and darwin/arm64.

## Transport workload

The daemon tests include thirty concurrent clients sending messages and reading
the status board over Unix sockets and loopback TCP:

```sh
go test ./internal/daemon -run '^TestTransportConcurrentMailAndBoard$' \
  -bench '^BenchmarkTransportConcurrent$' -benchtime=300x -count=2
```

On Linux/amd64 during the September 11, 2026 comparison, replacing the board's
151 database calls with one snapshot query for thirty agents reduced aggregate
time per board operation from about 7.5 ms to 0.57–0.60 ms. Both transports
improved similarly, and the test verified message counts and uniqueness.

These are local throughput measurements, not individual request latencies. The
test gives both transports the same trusted handler path to isolate socket cost;
it does not benchmark relay authentication or reproduce the historical timeout.
See [transport configuration](reference.md#local-transport-and-tcp).
