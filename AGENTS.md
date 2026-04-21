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
- Run all tests: `make test`
- Run all tests directly: `go test -race ./...`
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

- `gofmt -w .`
- `go vet ./...`
- `go test -race ./...`
- `go build -o ./bin/no-mistakes ./cmd/no-mistakes`

**Project Layout**

- `cmd/no-mistakes`: process entrypoint
- `internal/cli`: cobra commands and CLI wiring
- `internal/daemon`: background daemon and run management
- `internal/pipeline` and `internal/pipeline/steps`: orchestration plus review/test/lint/push/PR/CI steps
- `internal/agent`: Claude, Codex, Rovo Dev, and OpenCode integrations
- `internal/git`, `internal/ipc`, `internal/config`, `internal/db`, `internal/paths`, `internal/types`: shared infrastructure
- `internal/tui`: terminal UI

**Documentation**

- Keep `README.md` concise and high-level. The bar needs to be extremely high for what has to show up there.
- Do not put technical details or deep reference material in `README.md`.
- Most documentation should live in `docs/` which is the published docs site.

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
- Prefer targeted package tests while iterating, then finish with `go test -race ./...`.
- Windows PR CI runs the full test suite (`go test ./...`); no special suffix is required to opt a test in.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.
