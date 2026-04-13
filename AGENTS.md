# AGENTS.md

This file is for agentic coding tools working in this repo.

This repository is a Go CLI app named `no-mistakes`.
The binary entrypoint is `cmd/no-mistakes`.
Most implementation code lives under `internal/`.

**Environment**

- Go version: `1.25.0` from `go.mod`
- Build tooling: standard Go toolchain plus `Makefile`
- CLI/UI libraries: `cobra`, `bubbletea`, `bubbles`, `lipgloss`
- Database: SQLite via `modernc.org/sqlite`

**Primary Commands**

- Build with release metadata: `make build`
- Plain build: `go build -o ./bin/no-mistakes ./cmd/no-mistakes`
- Install locally: `make install`
- Cross-compile archives: `make dist`
- Run unit + integration tests: `make test`
- Run all tests (unit + integration + e2e): `make test-all`
- Run vet: `make lint`
- Run vet directly: `go vet ./...`
- Format all Go files: `make fmt`
- Format directly: `gofmt -w .`
- Check formatting only: `gofmt -l .`
- Clean build output: `make clean`

**Single-Test Commands**

- Run one package: `go test ./internal/cli`
- Run one package with race detector: `go test -race ./internal/cli`
- Run one top-level test: `go test ./internal/update -run '^TestCompareVersions$'`
- Run a subset by regex: `go test ./internal/tui -run 'TestModel_'`
- Re-run without test cache: `go test ./internal/cli -run '^TestDoctorBasic$' -count=1`

Safest local verification sequence after non-trivial changes:

- `make fmt`
- `make lint`
- `make test`
- `make build`

**Project Layout**

- `cmd/no-mistakes`: process entrypoint
- `internal/cli`: cobra commands and CLI wiring
- `internal/daemon`: background daemon and run management
- `internal/pipeline` and `internal/pipeline/steps`: orchestration plus review/test/lint/push/PR/CI steps
- `internal/agent`: Claude, Codex, Rovo Dev, and OpenCode integrations
- `internal/git`, `internal/ipc`, `internal/config`, `internal/db`, `internal/paths`, `internal/types`: shared infrastructure
- `internal/tui`: terminal UI

**Context, Concurrency, and Processes**

- Thread `context.Context` through long-running, subprocess, and networked work.
- Prefer `exec.CommandContext` for subprocesses.
- Use derived contexts and timeouts for cleanup and HTTP calls.
- Use `context.Background()` mainly at top-level boundaries, background tasks, or in tests.
- Protect shared mutable state with `sync.Mutex`, `sync.RWMutex`, `sync.Map`, or `atomic` where appropriate.
- Be explicit about ownership and cleanup of goroutines, worktrees, temp dirs, and channels.

**Filesystem and Paths**

- Use `filepath.Join` and related helpers.
- Respect `NM_HOME` when working with app state.
- Tests should isolate filesystem state with `t.TempDir()` and `t.Setenv("NM_HOME", ...)`.
- Existing code typically uses `0o755` for directories and `0o644` for files such as logs.
- On macOS, remember that path comparisons may need symlink resolution like `/var` vs `/private/var`.

**Testing Conventions**

- Tests live next to the code in `*_test.go` files.
- Use the standard `testing` package.
- Table-driven tests are common and use `tests := []struct { ... }` plus `t.Run`.
- Use `t.Helper()` in helpers.
- Use `t.TempDir()` for isolated filesystem state.
- Use `t.Setenv()` for environment-dependent behavior.
- Prefer creating real git repos in temp directories instead of relying on heavy mocking.
- CLI tests often capture output and assert with `strings.Contains`.
- Prefer targeted package tests while iterating, then finish with `make test`.

**Test Tagging**

Tests are split into three tiers using Go build tags. The tag goes on the first
line of the file, before the `package` declaration. The decision is per-file,
not per-package - a single package can have both untagged unit tests and tagged
integration tests (e.g. `internal/ipc` has unit tests in `protocol_test.go` and
integration tests in `server_test.go`).

| Tier        | Tag                      | When to use                                              |
| ----------- | ------------------------ | -------------------------------------------------------- |
| unit        | (none)                   | Pure logic: JSON, config, DB, types, data transforms     |
| integration | `//go:build integration` | Real subprocesses, IPC, daemon lifecycle, git operations |
| e2e         | `//go:build e2e`         | Full CLI command flows, pipeline step execution          |

When adding a new test file, pick the lowest tier that fits:

- Does it only test in-memory logic? Leave untagged (unit).
- Does it spawn a subprocess, open a socket, or exercise OS-specific behavior? Tag `integration`.
- Does it drive a full command or pipeline flow end-to-end? Tag `e2e`.

**Makefile targets:**

- `make test` - unit + integration (~30s, the local dev loop)
- `make test-all` - unit + integration + e2e (~170s, what CI runs)
- `make test-unit` - unit only (~3s)

CI runs the full suite (`test-all`) on Linux and macOS. Windows CI runs only
packages that contain integration or e2e tagged files to skip redundant unit coverage.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.
