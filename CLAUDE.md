# no-mistakes

Local Git proxy that intercepts pushes and runs code through a validation pipeline before forwarding upstream.

## Build & Test

```sh
make build   # builds bin/no-mistakes with version info
make test    # go test -race ./...
make lint    # go vet ./...
make fmt     # gofmt -w .
```

## Architecture

- Single binary: CLI mode (default) or daemon mode (`NM_DAEMON=1` env var)
- Data: `~/.no-mistakes/` (override with `NM_HOME`)
- IPC: JSON-RPC over Unix socket
- DB: SQLite via modernc.org/sqlite (pure Go, no CGO)

## Conventions

- Use `log/slog` for structured logging
- Shell out to `git` CLI for all git operations
- Use `oklog/ulid` for IDs
- Tests use `t.TempDir()` and `paths.WithRoot()` for isolation
- Error messages: lowercase, no punctuation, wrap with `fmt.Errorf("context: %w", err)`
