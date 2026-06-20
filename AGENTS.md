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
- Run unit/integration tests: `make test`
- Run unit/integration tests directly: `go test -race ./...`
- Run end-to-end tests: `make e2e`
- Re-record end-to-end fixtures: `make e2e-record`
- Regenerate the committed agent skill: `make skill`
- Run skill drift check and vet: `make lint`
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
- `make lint`
- `go test -race ./...`
- `make e2e` when touching agent integrations, the e2e harness, or recorded fixtures
- `go build -o ./bin/no-mistakes ./cmd/no-mistakes`

**Project Layout**

- `cmd/no-mistakes`: process entrypoint
- `internal/cli`: cobra commands and CLI wiring
- `internal/daemon`: background daemon and run management
- `internal/pipeline` and `internal/pipeline/steps`: orchestration plus review/test/lint/push/PR/CI steps
- `internal/agent`: Claude, Codex, Rovo Dev, OpenCode, Pi, and ACP/acpx integrations
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
- Prefer e2e tests, new or existing, for behavior that crosses a process or I/O boundary: CLI flags, config loading, git operations, agent spawning, daemon/process coordination, stdout/stderr, and recorded fixtures.
- Unit-test pure helpers and tightly scoped package behavior where speed and failure localization are worth more than full-product realism.
- Prefer targeted package tests while iterating, then finish with `go test -race ./...` and `make e2e` when your change affects those process or I/O boundaries.
- The e2e suite lives behind the `e2e` build tag, so it is excluded from `go test ./...` and runs separately in CI via `make e2e`.

**Test Step (coverage-aware, pluggable per language)**

- After the test suite passes, `internal/pipeline/steps/test.go` calls `runCoverageCheck` in `internal/pipeline/steps/coverage.go`. That dispatcher does the language-neutral diff plumbing once (`git diff --name-only` + `git diff --unified=0 <base>..<head>` → added-line ranges), then loops over every registered `coverageProvider` (defined in `internal/pipeline/steps/coverage_provider.go`), asking each active one to filter coverable files, run its native coverage tool, and parse the result into blocks. Each provider's blocks feed into the shared, language-neutral `uncoveredChangedLineFindings` core, which flags any changed source file where at least one added line is executable but not exercised (`warning`/`ask-user` finding, `id: uncovered-changed-lines:<lang>:<path>`; description reports the uncovered added-line count).
- **Adding a language = one isolated file.** Implement `coverageProvider` (`Name`, `Active`, `CoverableChangedFiles`, `RunCoverage`, `ParseBlocks`) in its own file (`coverage_go.go`, `coverage_jsts.go`, `coverage_swift.go`) and call `registerCoverageProvider(...)` in `init()`. No shared code (`coverage.go`, `test.go`) is touched. The interface lives in `coverage_provider.go`; the registry and `namespaceFindings`/`toRepoRelPOSIX` helpers live there too. **Filename gotcha:** the `_js` suffix is a Go GOOS (WebAssembly target), so the JS/TS provider file is `coverage_jsts.go`, not `coverage_js.go` — Go silently ignores the latter on non-js GOOS. Check the suffix isn't a reserved GOOS/GOARCH before naming a new provider file.
- **Path-key invariant (#1 correctness rule):** `ParseBlocks` must key blocks by repo-relative POSIX path byte-identical to `git diff --name-only` output, because the coverable and added-line maps are keyed that way. Use the shared `toRepoRelPOSIX(absPath, workDir)` helper (it strips the workDir prefix, handles symlink spellings like macOS `/var` vs `/private/var`, Cleans, and POSIX-izes separators) so the invariant is encoded once.
- This is changed-line (diff) level, which subsumes file-level: a brand-new untested file (all lines added, all uncovered) still fires, and so does a new function dropped into an already-tested file. Blank/comment added lines in no coverage block are ignored; added lines in a zero-count block are the signal. The `<lang>:` segment is inserted after the `uncovered-changed-lines:` prefix so downstream TUI/filter prefix matching keeps working.
- Gated by `NO_MISTAKES_COVERAGE_CHECK` (`1`/`0`); the explicit override always wins. When unset, default ON when ANY registered provider is `Active()` for the worktree (e.g. `go.mod`, `package.json`, `Package.swift` present), OFF otherwise. Coverprofile line numbers are against HEAD, so they align with the diff's `+` side with no offset math.
- It re-runs the suite instrumented, so it only fires when tests already pass and only when coverable changed files exist. Errors degrade to a logged no-op (never block the pipeline).
- **Swift provider (`coverage_swift.go`):** delegates to a Mac build executor over SSH because this VPS has no Swift toolchain. Active only when both a Swift manifest (`Package.swift` or `*.xcodeproj`/`*.xcworkspace`) is present AND `NM_SWIFT_SSH_HOST` names the Mac. Two build modes selected by `NM_SWIFT_BUILD_MODE` (default `swiftpm`): `swiftpm` runs `swift test --enable-code-coverage` + `xcrun llvm-cov export <test-binary> --format=json` (parses `data[].files[].segments` → blocks spanning `[seg.line, nextSeg.line-1]`); `xcode` runs `xcodebuild test -enableCodeCoverage` + per-file `xcrun xccov view --file --json` (parses `coveredLines`/`uncoveredLines` → 1-line blocks), and **errors clearly when `/Applications/Xcode.app` is absent** (still gated). Remote flow syncs the head SHA via `git fetch origin && git checkout $HEAD_SHA` (refuses a dirty tree — never hard-resets) and uses `ssh host bash -l` over stdin so the Mac's Homebrew/CLT tools are on PATH. Config knobs: `NM_SWIFT_SSH_HOST`, `NM_SWIFT_REMOTE_PATH`, `NM_SWIFT_BUILD_MODE`, `NM_SWIFT_SCHEME`, `NM_SWIFT_PROJECT`, `NM_SWIFT_SSH_OPTS`. Note: on a CLT-only Mac (no Xcode.app), `swift test` still fails because the XCTest/Swift Testing modules ship with Xcode.app — so both modes effectively require Xcode.app to be installed for end-to-end coverage collection, just via different code paths.
- **JS/TS provider (`coverage_jsts.go`):** active when `package.json` is at the worktree root. Runs the project's test runner under `npx --yes c8@latest --reporter=json` (V8-native coverage; preferred over nyc) and parses the canonical Istanbul `coverage/coverage-final.json` — one `coverBlock` per statement (branches folded in for tighter executable-line detection), keys relativized via `toRepoRelPOSIX`. Test-runner resolution priority: `NM_JS_TEST_RUNNER` env var (explicit override) → `commands.test` from project config → `npm test` if `package.json` declares a test script → `node --test` fallback. Coverage is written to a temp `--reports-dir` and removed afterwards so the worktree stays clean. Coverable files: `*.{js,jsx,ts,tsx,mjs,cjs}` minus `*.test.*`/`*.spec.*` and `__tests__/`/`test/`/`tests/` directories.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.
