# AGENTS.md

This file is for agentic coding tools working in this repo.

This repository is a Go CLI app named `no-mistakes`.
The binary entrypoint is `cmd/no-mistakes`.
Most implementation code lives under `internal/`.

**Environment**

- Go version: see `go.mod`
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
- `internal/agent`: Claude, Codex, Rovo Dev, OpenCode, Pi, Copilot, and ACP/acpx integrations
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

**GitLab Backend (`internal/scm/gitlab`)**

- The GitLab `Host` is constructed via `gitlab.New(cmd, cliAvailable, host, projectPath)`, mirroring the GitHub backend's positional constructor. `host` is the repo's GitLab hostname (from `scm.ExtractHost(UpstreamURL)`); `projectPath` is the `group/project` path (subgroups allowed, from `gitlab.ProjectPath` - which lives in the gitlab package next to the `Host` that consumes it, mirroring `github.RepoSlug`). Both are optional; passing `"", ""` reproduces the legacy unscoped behavior used by unit tests.
- glab's flag surface drifts between versions; the backend is pinned against `glab v1.5x`. Two flags bit us: `glab auth status` must be **host-scoped** with `--hostname <host>` (unscoped, it checks every configured instance and fails if ANY has a stale token, falsely reporting an authenticated repo as unauthenticated); and `glab mr list` no longer accepts `--state opened` (open is the default; v1.5x exposes `-c/--closed`, `-M/--merged`, `-A/--all`) - passing the removed flag fails the whole command. When the host is unknown, fall back to the unscoped auth check (fail-safe).
- The daemon operates in a **detached-HEAD worktree** (it checks out the commit, not a branch). `glab ci get` refuses to run there ("you're not on any Git branch (a 'detached HEAD' state)") even with an explicit `--pipeline-id`. Read pipeline jobs via the branch-independent REST endpoint instead: `glab api projects/<url-encoded group%2Fproject>/pipelines/<id>/jobs` (`Host.pipelineJobsArgs`). The legacy `glab ci get` path is kept only as the fallback when no project path is supplied. The `glab api .../jobs` payload carries `finished_at`, mapped into `Check.CompletedAt` (needed for CI re-run detection).

**Documentation**

- Keep `README.md` concise and high-level. The bar needs to be extremely high for what has to show up there.
- Do not put technical details or deep reference material in `README.md`.
- Most documentation should live in `docs/` which is the published docs site.

**Agent-Guidance Surfaces**

- `skills/no-mistakes/SKILL.md` is **generated**, not hand-written: the source of truth is the `body` constant in `internal/skill/skill.go`. Edit the body, then `make skill` to regenerate; `make lint` runs `skill-check` (`genskill --check`) and fails CI on drift. Never edit `SKILL.md` directly. `no-mistakes init` installs/refreshes this same rendering at user level, so the strings in the Go source are what ships to agents.
- The "how an agent drives the pipeline" guidance lives in **three surfaces that must stay in sync**: (1) the skill body above (loaded when an agent invokes `/no-mistakes`); (2) the live `axi` output strings in `internal/cli/axi*.go` - the home `help` (`axi.go`), the gate `note`/`help` and run/respond return help (`axi_render.go` `gateFields`), and the `--help` Long strings (`axi_drive.go`); and (3) the published `docs/src/content/docs/guides/agents.md`. When you change driving guidance in one, mirror it in the others. The point-of-use `axi` strings are the layer an agent reads while driving without reopening the skill.
- Review auto-fix is disabled by default (`config.go` `autoFixDefaults` `Review: 0`; a repo or global `auto_fix.review > 0` override re-enables it through `AutoFixLimit(types.StepReview)` and the executor auto-fix loop), so blocking and ask-user review findings park for an agent decision rather than being silently self-fixed.
  An info-level auto-fix review finding under the default neither parks nor is fixed, so keep the skill, live `axi` note, and docs qualified if you touch review auto-fix.

**Context, Concurrency, and Processes**

- Thread `context.Context` through long-running, subprocess, and networked work.
- Prefer `exec.CommandContext` for subprocesses.
- Route every long-lived subprocess spawned on behalf of a cancellable step/agent invocation through `shellenv.ConfigureShellCommand(cmd)` after building the `*exec.Cmd`.
  It puts the child in its own process tree boundary (Unix `Setpgid`, Windows job object with `taskkill` fallback) and installs `cmd.Cancel` to kill the whole tree on context cancellation.
  Without it, `exec.CommandContext` only kills the direct child and grandchildren survive (e.g. `npm` -> `node` test workers, agent-spawned git/build/editor), keep running, and hold the worktree locked so the next run on the same branch cannot proceed.
  Applied to the step shell runner (`runShellCommandWithEnv`) and the native agent `runOnce` builders (claude, codex, pi, copilot, acpx); apply it to any new subprocess in those paths.
- `cmd.Cancel` only covers the **cancellation** half of the lifecycle.
  On a clean exit (exit 0) or an error return it never fires, so a grandchild that outlived the leader - a test runner's worker pool, a build watcher, a dev server - is **not** reaped.
  This is the agent-spawning test step's failure mode: a repo with no `commands.test` asks the agent to run the tests, the agent's worker pool leaks on every clean run, and the orphans accumulate (each a multi-hundred-MB pool) until the host is out of memory and the OS OOM-killer SIGKILLs the daemon - surfacing on the next start as `daemon crashed during execution` (no Go stack trace, because SIGKILL is uncatchable).
  Use `shellenv.RunShellCommand`, `shellenv.OutputShellCommand`, or `shellenv.CombinedOutputShellCommand` for one-shot commands; they start the command and reap the group on success/error paths too.
  When manual pipe handling is needed, use `shellenv.StartShellCommand(cmd)` and ensure `shellenv.TerminateShellCommandGroup(cmd)` runs as soon as the command is done or the parse loop fails.
  For stdout/stderr parsers that read until EOF, make the Wait owner terminate the group when the leader exits so a descendant holding inherited pipes cannot wedge the parser.
  `startNativeAgentCommand` owns that lifecycle for the native agent runners.
  Group termination is a harmless no-op (ESRCH) when nothing survived.
  `ConfigureShellCommand` also installs a `cmd.WaitDelay` pipe backstop (5s, now on unix as well as Windows) so a grandchild holding an inherited stdout/stderr pipe open after exit can't wedge `cmd.Wait`/`CombinedOutput` forever; it bounds the hang into a graceful step failure instead of taking the daemon down.
  Regressions:
  `TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit` (agent path),
  `TestRunShellCommandWithEnv_ReapsGrandchildOnCleanExit` (configured-command path),
  `TestTerminateShellCommandGroup_*` (the primitive).
- On Windows the daemon runs console-less, so every console child it spawns (git, shell steps, native agents, provider CLIs, and helper commands such as `powershell`/`taskkill`/managed servers) would otherwise flash a fresh visible console window for its lifetime (#287).
  Route each such `*exec.Cmd` through `winproc.Harden(cmd)` after building it: it OR-s `CREATE_NO_WINDOW` into any existing creation flags (preserving e.g. `CREATE_NEW_PROCESS_GROUP`) and sets `HideWindow`, leaves stdout/stderr redirection intact, is safe on a nil or already-populated `SysProcAttr` and safe to call more than once, and is a no-op on non-Windows platforms.
  `shellenv.ConfigureShellCommand` already calls it, so cancellable step/agent subprocesses routed through that helper are covered; one-shot commands built directly (`git.Run`, `stepCmd`, `scm.AuthConfigured`, doctor's `git --version`, and the Windows service/taskkill/powershell/managed-server helpers) call it themselves, so apply it to any new such subprocess.
  Intentionally detached background processes already run without a window: `update`'s detached spawn OR-s `CREATE_NO_WINDOW` into its own flags directly rather than calling `Harden`.
  Regressions: `TestHarden*` in `internal/winproc`.
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

**Git on Bare Gate Repos (`safe.bareRepository`)**

- Agent harnesses (e.g. Claude Code) and hardened CI inject `safe.bareRepository=explicit` via `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`, which forbids cwd-based discovery of bare repositories. Every git operation on a gate repo must therefore name it explicitly: `git.Run` detects a bare git dir (`isBareGitDir`: `HEAD` file + `objects/` dir, no `.git` entry) and prepends `--git-dir=<dir>`; working trees and linked worktrees keep normal discovery. Route gate git calls through `git.Run` - never shell out to git in a bare gate repo relying only on `cmd.Dir` or `-C` discovery (issue #362). Regressions: `TestRunOnBareRepoUnderSafeBareRepositoryExplicit`, `TestWorktreeAddRemoveOnBareRepoUnderSafeBareRepositoryExplicit`, `TestInitUnderSafeBareRepositoryExplicit`.

**Daemon Singleton Lock (`internal/daemon/lock.go`)**

- Only one live daemon process may own an `NM_HOME` at a time. Before v1.33.x this was unenforced: `internal/ipc/transport_unix.go` `listen()` unconditionally `os.Remove`d the Unix socket before binding, so a second daemon starting against the same root silently stole the socket path from a still-live first daemon instead of failing. The stolen-from daemon stayed alive but became unreachable, while every subsequent daemon startup ran **global** stale-run recovery (`RecoverStaleRuns`) and orphan-worktree cleanup against the shared DB/worktree root - marking the still-running daemon's active runs `daemon crashed during execution` and deleting worktrees out from under pipelines that were still executing in them (`chdir .../worktrees/...: no such file or directory`).
- The fix is an OS-level advisory file lock at `p.LockFile()` (`<NM_HOME>/daemon.lock`, distinct from the informational `daemon.pid`). `acquireSingletonLock` (Unix: `syscall.Flock(LOCK_EX|LOCK_NB)`; Windows: `windows.LockFileEx` with `LOCKFILE_FAIL_IMMEDIATELY`) is taken as the very first action inside `RunWithOptions` - shared by every entrypoint (`daemon.Run()` for `daemon start`'s detached child and for direct `no-mistakes daemon run --root`, plus `RunWithResources`/`RunWithOptions` used by tests) - strictly before `recoverOnStartup` and before the IPC socket binds, and held for the process lifetime via `defer lock.Release()`. A failed acquire returns `ErrSingletonLockHeld` and the daemon does not start.
- No separate "is the holder still alive" staleness check is needed the way the PID file requires one: `flock`/`LockFileEx` are held per open file description, and the kernel releases them automatically when every fd referencing that description closes, including on process exit or SIGKILL. A held lock therefore always means a genuinely live holder - this is what makes the guard safe by construction rather than needing pid+start-time heuristics.
- `internal/ipc/transport_unix.go` `listen()` is a second, independent layer: before removing the socket path it now dials it (`net.DialTimeout("unix", endpoint, 200ms)`); if something answers, it fails instead of unlinking a live daemon's socket. Only a proven-stale path (nothing listening - a leftover from an unclean exit) is removed and rebound. This keeps `listen()` itself safe even if called outside the daemon package's lock.
- Startup worktree cleanup (`cleanupOrphanWorktrees`, split out of `recoverOnStartup`) is DB-aware: `skipWorktreeCleanup` looks up each worktree's run row and never removes one whose status is `pending` or `running`, only terminal-run leftovers or directories with no matching run row. `RunManager.startRun` always inserts the run row before creating the worktree directory, so a "no matching run" directory is never one whose insert simply hasn't landed yet on a single daemon - it's safe to remove immediately (no grace period; one was tried and it broke `TestRecoverCleansUpOrphanedWorktrees`, which expects immediate removal of a directory with no DB row at all).
- A third layer guards the client side: `EnsureDaemon`'s liveness probe (`daemonIsRunningViaIPC` in `internal/daemon/selfexec.go`) dials through `ipc.Dial`, which bounds the attempt with `daemon_connect_timeout` (config field, default `3s`; env `NM_DAEMON_CONNECT_TIMEOUT`, see `internal/ipc/client.go`). A `*ipc.ConnectTimeoutError` means the socket exists and the process is alive but stuck, not stale, so the probe propagates that error instead of reporting not-alive, and `EnsureDaemon` returns it instead of falling through to `Start` a second daemon against the same root. Any other dial failure (e.g. nothing listening) still reports not-alive so `Start` can proceed normally.
- Regressions: `TestAcquireSingletonLock_SecondAcquireFails`/`_ReleaseAllowsReacquire`/`_ReportsExistingHolder` (lock mechanics), `TestRunWithResources_SecondDaemonForSameRootFailsWithoutStealingSocket` (end-to-end duplicate-daemon repro, asserts the first daemon stays reachable), `TestRunWithOptions_RequiresSingletonLockBeforeRecovery` (lock gates `RecoverStaleRuns`), `TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree` (active-run worktree survives `cleanupOrphanWorktrees`), `TestServe_SecondListenerForLiveSocketDoesNotStealIt` (`internal/ipc`, socket-steal repro at the transport layer), `TestDialConnectTimeoutFailsFastAndNamesSocket` (bounded client dial), `TestLoadGlobal_InvalidDaemonConnectTimeout` (config validation).

**Destructive Daemon Lifecycle Guard (`internal/lifecycle/guard.go`)**

- `no-mistakes daemon stop`, `no-mistakes daemon restart`, and `no-mistakes update` all share a daemon-owned by a whole machine, not just one repo, so stopping/replacing it mid-run can fail every pipeline currently active across every repo on that `NM_HOME`. All three refuse by default when `lifecycle.ActiveRuns(p)` (pending/running rows in the local state DB) is non-empty, print each active run's ID, status, branch, and short head SHA via `lifecycle.RunList`, and require an explicit `--force` flag to proceed anyway. `guardDestructiveDaemonLifecycle` (`internal/cli/daemon_cmd.go`) implements the `daemon stop`/`daemon restart` check; `updater.confirmActiveRunsBeforeUpdate` (`internal/update/active_runs.go`) implements it for `update`, sharing the same `lifecycle.ActiveRuns`/`lifecycle.RunList` helpers so the three commands report runs identically.
- `-y`/`--yes` on `update` does **not** bypass this guard - it only answers the separate "daemon running from a different executable path" prompt. Only `--force` accepts the risk of active runs failing. This is a deliberate split: `-y` was previously overloaded to also skip the active-run warning, which made it too easy for a scripted/agent-driven `update -y` to silently kill in-flight pipelines.
- Every invocation of `daemon stop`, `daemon restart`, or `update` - forced or not, guard-triggered or not - is logged via `logLifecycleInvocation` (`internal/cli/lifecycle_log.go`) to `<NM_HOME>/logs/cli.log` with the command name, `force` value, the caller's PID and PPID, and the parent process's command line (`ps -o command= -p <ppid>`, best-effort, empty on failure). This is the forensic trail for "which agent or process stopped/restarted the shared daemon" after an incident; do not remove or weaken it when touching these commands.
- Regressions: `TestDaemonStopRefusesWithActiveRunsAndListsThem`, `TestDaemonStopForceOverridesActiveRunGuard`, `TestDaemonRestartRefusesWithActiveRuns`, `TestLifecycleCommandsWriteCallerAttributionToCLILog` (`internal/cli/daemon_lifecycle_test.go`); `TestUpdaterRunRefusesWithActiveRunsAndListsThem`, `TestUpdaterActiveRunGuardAllowsForce` (`internal/update/update_test.go`).

**Testing Conventions**

- Tests live next to the code in `*_test.go` files.
- Use the standard `testing` package.
- Table-driven tests are common and use `tests := []struct { ... }` plus `t.Run`.
- Use `t.Helper()` in helpers.
- Use `t.TempDir()` for isolated filesystem state.
- Use `t.Setenv()` for environment-dependent behavior.
- Prefer creating real git repos in temp directories instead of relying on heavy mocking.
- Packages whose tests shell out to git unset `GIT_CONFIG_COUNT` in `TestMain` so ambient `GIT_CONFIG_*` injection from agent harnesses cannot leak into tests; a test that exercises the injected config re-sets it with `t.Setenv` (see `internal/git`, `internal/gate`, `internal/daemon`, `internal/pipeline/steps`). Follow the same pattern in new git-spawning test packages.
- CLI tests often capture output and assert with `strings.Contains`.
- Prefer e2e tests, new or existing, for behavior that crosses a process or I/O boundary: CLI flags, config loading, git operations, agent spawning, daemon/process coordination, stdout/stderr, and recorded fixtures.
- Unit-test pure helpers and tightly scoped package behavior where speed and failure localization are worth more than full-product realism.
- Prefer targeted package tests while iterating, then finish with `go test -race ./...` and `make e2e` when your change affects those process or I/O boundaries.
- The e2e suite lives behind the `e2e` build tag, so it is excluded from `go test ./...` and runs separately in CI via `make e2e`.
- `make e2e` sweeps both `./internal/e2e/...` (full journey suite) and `./internal/pipeline/steps/...`, so step-local e2e tests (e.g. `internal/pipeline/steps/*_e2e_test.go`, gated by `//go:build e2e`) are also covered. Keep new step-local e2e tests behind the `e2e` tag so `go test ./...` still skips them.

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

**Parked / Awaiting-Agent Signal**

- A run carries a pollable "parked, awaiting the driving agent" marker so a supervisor can tell in one `axi status` read whether a run is waiting for the agent to drive a gate versus actively running/fixing/ci. It is **observability only**: it does not change gate resolution, auto-resume, or the `--yes` default.
- Storage: `runs.awaiting_agent_since` (unix seconds, nullable) on `db.Run.AwaitingAgentSince`. `ipc.RunInfo` exposes both `AwaitingAgent bool` (= since != nil) and `AwaitingAgentSince *int64`; `runToInfo` derives them.
- Invariant: `awaiting_agent_since` is non-nil **iff a step is actually parked** at an `awaiting_approval`/`fix_review` gate. The executor (`internal/pipeline/executor.go`) sets it via `db.SetRunAwaitingAgent` on gate entry (right before the step status flips to the gate state, so it is already set once pollers observe the gate) and clears it via `db.ClearRunAwaitingAgent` the moment `waitForApproval` returns - covering both the agent's `axi respond` and a cancel. `RecoverStaleRuns` also clears it so a crash-recovered (failed) run is never reported as parked.
- Surface: the `run:` TOON object adds `awaiting_agent: parked <duration>` right after `status`, rendered only while `AwaitingAgentSince != nil` and the run is non-terminal (`internal/cli/axi_render.go` `runObjectFieldWithKey` + `formatParkedFor`). The render clock is the injectable `nowUnix` package var so parked-duration tests are deterministic.
- Tests: db set/clear + recovery (`internal/db/run_test.go`), executor flips-on-gate/clears-on-respond (`internal/pipeline/executor_approval_test.go`), formatter + render shape (`internal/cli/axi_test.go`), and e2e `TestAxiParkedAwaitingAgentSignal`.

**Rebase Base & Force-Push Safety (data-loss prevention)**

- The whole job of this tool is to not lose people's code. Two invariants protect the rebase/push path; favor failing safe (refuse the push, surface a finding) over any clever recovery.
- **Rebase base comes from the freshly-fetched authoritative remote, never local/stale state.** The rebase step (`internal/pipeline/steps/rebase.go`) fetches `origin/<default>` and `origin/<branch>` (or the fork tracking ref) and rebases onto those remote-tracking refs - never the local default branch.
- **A gated branch must not silently bundle the contributor's unpushed local-default-branch commits.** `detectBundledLocalDefaultCommits` reads the working repo's local `<default>` tip (`Repo.WorkingPath`), and when that tip is ahead of `origin/<default>` **and** is an ancestor of the branch HEAD (i.e. the branch was built on unpushed default-branch work), the step returns `NeedsApproval` + `AutoFixable=false` so a human decides instead of widening the PR. Detection is best-effort: if the local default advanced past the branch point, or the working repo can't be read, it returns nil and the run proceeds. Regression: `TestRebaseStep_DetectsUnpushedLocalDefaultBranchCommits` (#283).
- **Every force-push is lease-guarded against discarding unseen upstream commits.** All force-push sites (`PushStep` in `push.go`, CI auto-fix `pushUpdatedHeadSHA` in `ci_fix.go`) route through `resolveForcePushDecision` (`internal/pipeline/steps/forcepush.go`). It re-reads the live remote head and allows the push only when: the branch is new; the remote already equals the head; the remote still equals `lastSeenSHA` (what the run last observed); or every commit now on the remote is already incorporated **by patch-id** (`git rev-list --cherry-pick --right-only HEAD...current`), excluding history the run is knowingly rewriting (`^baseSHA`, i.e. reachable from the run base - the common amend or reverting the pipeline's own autofix). Anything else returns `forcePushWouldDiscardError` and the caller must NOT push. An out-of-band commit reaches the branch _after_ the run base, so it is never an ancestor of `baseSHA` and stays flagged.
- **`lastSeenSHA` must stay the head the run last _observed_, never the live remote tip.** The push step passes the remote-tracking ref the rebase step synced (`lastFetchedBranchTip`); the CI step passes `Run.HeadSHA`. Both callers also pass `Run.BaseSHA` for the `^baseSHA` exclusion. Critically, **the rebase step refreshes `origin/<branch>` only on a normal push, NOT on a force push** - on a force push it skips both the rebase-onto and the fetch, so the tracking ref stays the last-observed head. If it refreshed on a force push, `lastSeenSHA` would equal the live tip, the `current == lastSeenSHA` fast path would pass without the content check, and an out-of-band commit on the branch would be silently clobbered. Anchoring the lease to a SHA read from the remote _immediately before pushing_ is the original #281 bug (it always passes and protects nothing); making the rebase always-fetch the branch was the same bug re-created for the force-push path. Never reintroduce either, and never degrade to a bare `--force`/`--force-with-lease` without an explicit anchor when ls-remote/fetch fails (fail closed instead). Regressions: `TestCIStep_CommitAndPush_RefusesToClobberUnseenUpstreamCommit` (#281), `TestPushStep_RefusesToClobberAdvancedUpstreamBranch` (#305), `TestForcePushRun_RefusesToClobberOutOfBandBranchCommit` (force-push fast-path clobber), and `TestResolveForcePushDecision_*`.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
