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

**Fork Routing**

- `repos.upstream_url` is the parent repository used for PR base routing.
- `repos.fork_url` is an optional GitHub fork push target.
- `no-mistakes init --fork-url <url>` expects `origin` to point at the GitHub parent repository and `<url>` to point at the contributor fork.
- Plain `no-mistakes init` preserves an existing fork URL on idempotent refresh.
- Push code must use `Repo.PushURL()` so configured forks receive branch updates.
- GitHub PR code must keep `--repo` pointed at the parent and use `--head <fork_owner>:<branch>` when `fork_url` is set.
- GitHub existing-PR lookup must not pass `<owner>:<branch>` to `gh pr list --head`; list by the bare branch and filter the returned head owner fields.
- GitLab and Bitbucket fork MR/PR routing is intentionally out of scope until implemented end to end.
- If a legacy or manually edited row has `fork_url` for GitLab or Bitbucket, PR creation must skip instead of opening a self PR.

**Documentation**

- Keep `README.md` concise and high-level. The bar needs to be extremely high for what has to show up there.
- Do not put technical details or deep reference material in `README.md`.
- Most documentation should live in `docs/` which is the published docs site.

**Context, Concurrency, and Processes**

- Thread `context.Context` through long-running, subprocess, and networked work.
- Prefer `exec.CommandContext` for subprocesses.
- Route every long-lived subprocess spawned on behalf of a cancellable step/agent
  invocation through `shellenv.ConfigureShellCommand(cmd)` after building the
  `*exec.Cmd`. It puts the child in its own process group (Unix `Setpgid` /
  Windows `CREATE_NEW_PROCESS_GROUP`) and installs `cmd.Cancel` to kill the whole
  tree on context cancellation. Without it, `exec.CommandContext` only kills the
  direct child and grandchildren survive (e.g. `npm` -> `node` test workers,
  agent-spawned git/build/editor), keep running, and hold the worktree locked so
  the next run on the same branch cannot proceed. Applied to the step shell
  runner (`runShellCommandWithEnv`) and the native agent `runOnce` builders
  (claude, codex, pi, acpx); apply it to any new subprocess in those paths.
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

**Repo Config Trust Boundary (security)**

- The daemon runs `commands.*` from `.no-mistakes.yaml` verbatim via `sh -c`, and `agent` selects which process launches (incl. `acp:` targets) with the maintainer's credentials. To prevent supply-chain RCE, the **code-executing selection fields (`commands.{test,lint,format}` and `agent`)** are loaded from the trusted default branch, never from the pushed SHA. See `internal/daemon/manager.go` `startRun` + `loadTrustedRepoConfig`, and `config.EffectiveRepoConfig`.
- `startRun` fetches the default branch, resolves it to an exact commit SHA (`git.ResolveRef`), and `loadTrustedRepoConfig` reads `.no-mistakes.yaml` at that **pinned SHA** (not the `origin/<defaultBranch>` ref name). On fetch failure (or if the ref does not resolve) the trusted SHA is empty → `loadTrustedRepoConfig` returns nil → `EffectiveRepoConfig` forces empty `commands`/`agent`. This fails closed: a stale `origin/<default>` ref left in the shared bare repo by a previous run cannot serve a value the live default branch removed. Regression tests: `TestLoadTrustedRepoConfig_FailClosedOnFetchFailure`, `TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch`.
- Non-executing fields (`ignore_patterns`, `auto_fix`, `intent`, `test`) are still read from the pushed branch.
- `allow_repo_commands` is **per-repo, read from the trusted default-branch copy of `.no-mistakes.yaml`** (declared on `RepoConfig`), never the global config and never the pushed SHA. It defaults `false`; when `true` the maintainer has opted in to honoring the pushed branch's `commands` and `agent` wholesale. A contributor cannot self-enable it from a pushed branch. When changing this logic, keep `commands`/`agent` locked to the default branch and update the e2e test `TestRepoConfigCommandsFromDefaultBranch` (incl. the `pushed_branch_cannot_self_enable` subtest).
- The e2e harness models a trusted single-developer environment, so it commits `allow_repo_commands: true` to the default-branch `.no-mistakes.yaml` via `SetupOpts.AllowRepoCommands`; security tests pass `false` to exercise the secure default.

**CI Monitor Lifecycle**

- The CI step (`internal/pipeline/steps/ci.go`) babysits an open PR until it is merged, closed, the run is cancelled, or `ci_timeout` elapses. It auto-fixes failing checks and rebases on merge conflicts via `autoFixCI`.
- `ci_timeout` is an **idle timeout, not an absolute deadline**: it re-arms (`timeoutAnchor = now()`) every time the upstream default-branch tip advances, so an actively-rebased green PR keeps its monitor no matter how long it stays open. `started` stays fixed for poll-interval/grace-period pacing; only `timeoutAnchor` moves. Re-arm only ever extends the deadline, so a transient base-tip resolution failure is fail-safe. `baseBranchTip` is injectable for tests.
- `config.CITimeout` semantics: `>0` finite, `0` = unset (step falls back to `config.DefaultCITimeout`, 7 days), `<0` = `config.CITimeoutUnlimited` (never self-terminate). Config keyword `ci_timeout: "unlimited"` (also `none`/`off`/`never`) or any non-positive duration resolves to the unlimited sentinel via `parseCITimeout`. Keep `config.DefaultCITimeout` and the `defaultConfigYAML` `ci_timeout` value in sync (`TestDefaultConfigYAML_MatchesGoDefaults`).
- Reap a run by id from outside its worktree with `no-mistakes axi abort --run <id>` (`runAxiAbortByRunID`). It needs only `NM_HOME` + the daemon, not a repo/branch/worktree, because `ipc.MethodCancelRun` → `RunManager.HandleCancel` only cancels runs live in daemon memory. An unknown/inactive id, or a stopped daemon, is an idempotent no-op (`aborted: false`), not an error. This is how an orphaned monitor (worktree torn down before merge) gets reaped deterministically. Bare `axi abort` (no `--run`) stays worktree/branch-scoped.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.
