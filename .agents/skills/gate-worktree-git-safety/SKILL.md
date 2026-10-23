---
name: gate-worktree-git-safety
description: Use when changing recursive gate containment, run worktree placement, bare-gate Git calls, GitHub PR targeting, or post-receive hooks.
user-invocable: false
metadata:
  internal: true
---

**Recursive Gate-Execution Containment**

- `internal/gatecontext` is the single classifier for recursive pipeline control. It combines canonical registered gate common-directory identity with OS-authenticated IPC peer ancestry; `NO_MISTAKES_GATE` is diagnostic only. CLI preflight, daemon mutation ingress, gate init/eject, branch-sync mutation, and the managed pre-receive hook must all keep using that owner so marker removal, cwd changes, and direct pushes cannot bypass refusal. Read-only AXI status/logs, help, and doctor remain available. Regressions: `internal/gatecontext`, `TestGateStepCannotStartRecursivePipeline`.
- Every pipeline agent prompt receives the phase boundary from `internal/gateguidance`, and the generated user-level skill reuses the same owner. Step agents return only their assigned phase; the outer executor alone controls other validation, push, PR, and CI phases. Edit `internal/skill/skill.go`, then run `make skill`; never edit the generated skill directly.

**Filesystem and Paths**

- Use `filepath.Join`; respect `NM_HOME` for app state; directories are `0o755` and files `0o644` by convention.
- On macOS, path comparisons may need symlink resolution (`/var` vs `/private/var`); use `worktrees.Canonical`/`worktrees.Contains` wherever run worktree paths are compared, so one spelling matches everywhere.
- Run worktree placement (`worktree_roots`) is owned by `internal/worktrees`. Configuration decides it exactly once, at run creation (`Layout.Dir` in `RunManager.startRunWithIntentSource`), and the result is persisted in `runs.worktree_dir`; every later consumer - resume, step diff, startup cleanup, `procreap`, eject, gatecontext attribution - must read it back through `worktrees.RecordedDir` and never re-derive it from config, so a mid-flight edit can neither strand a parked run nor point a removal at a directory the run never used. An empty column means the default `<NM_HOME>/worktrees/<repoID>/<runID>`.
- `worktrees.CheckPlacement` is the single policy for an unusable root (inside `NM_HOME`, inside any registered checkout); `config.ValidateWorktreeRoots` owns what the config can judge alone. The daemon refuses to start on an unusable placement, so `init --worktree-root` must refuse exactly the same set or it prints a paste that takes the operator's CLI down, and EVERY `init` refuses to register a checkout that contains a configured root - the same state reached from the other direction. User-facing semantics live in `docs/src/content/docs/reference/global-config.md`. Regressions: `internal/worktrees`, `internal/config/config_worktree_roots_test.go`, `internal/daemon/worktree_roots_test.go`, `internal/gate/eject_sweep_test.go`, `internal/cli/init_test.go`.

**Git on Bare Gate Repos (`safe.bareRepository`)**

- Agent harnesses and hardened CI inject `safe.bareRepository=explicit`, which forbids cwd-based discovery of bare repositories. Route every gate git call through `git.Run`, which detects a bare git dir and prepends `--git-dir=<dir>`; never shell out to git in a bare gate repo relying on `cmd.Dir` or `-C` discovery (issue #362).
- Startup gate migration is DB-authoritative with a strict validated `<id>.git` legacy fallback; it must reject non-gates before hook or Git mutation and use `git.RunBare` so a malformed directory cannot discover an ancestor worktree. Completed migrations carry the content-versioned gate-config stamp and normal restarts must stay filesystem-only for current gates. Regressions: `TestMigrateGateConfigsRejectsInvalidDirectoriesAndSkipsCurrentGates`, `TestColdDetachedStartupProductionGateCardinality`.
- Regressions: `TestRunOnBareRepoUnderSafeBareRepositoryExplicit`, `TestWorktreeAddRemoveOnBareRepoUnderSafeBareRepositoryExplicit`, `TestInitUnderSafeBareRepositoryExplicit`.

**`gh` PR-Targeting From the Bare Gate Repo (`internal/scm/github`)**

- The daemon runs `gh` from the detached bare gate repo whose HEAD is the default branch, so every PR-targeting command must name the exact PR explicitly: an empty positional makes `gh pr <verb>` infer the cwd branch (`main`) and return `no pull requests found for branch main` even when the feature PR's checks are green. `GetChecks`, `GetPRState`, `GetMergeableState`, and `UpdatePR` route through the shared `prSelector` (number, else URL, else fail closed); `UpdatePR` then derives the number and `owner/repo` for a REST `gh api --method PATCH repos/<slug>/pulls/<n> --input -` (never `gh pr edit`, whose GraphQL lookup needs `read:org`) and also fails closed when no repository is known - never append a bare `pr.Number`/`pr.URL` that can be empty. This is the `gh` analogue of the git bare-gate-repo trap above.
- Regressions: `TestGetChecksTargetsKnownPRByURLWhenNumberMissing`, `TestPRTargetingReadsFailClosedWithoutIdentity`, `TestPRStateAndMergeableTargetKnownPRByURL`, `TestUpdatePRTargetsKnownPRByURLWhenNumberMissing`, `TestUpdatePRFailsClosedWithoutIdentity`, `TestUpdatePRPatchesPullRequestOverREST`.

**Post-Receive Hook Gate Path Resolution (`internal/git/hook.go`)**

- The hook's `--gate` value must never come from a bare `$(pwd)`: Git can invoke `post-receive` from a cwd that collapses to `.` (issue #269), which the daemon rejects and the pipeline silently never starts. The hook script resolves an absolute gate dir (git first, hook location fallback), and `normalizeNotifyGatePath` in `internal/cli/daemon_cmd.go` is an independent second layer that absolutizes whatever an already-installed older hook sends.
- Regressions: `TestPostReceiveHook_ResolvesAbsoluteGateDir`, `TestPostReceiveHook_FallsBackToHookLocationForGateDir`, `TestNormalizeNotifyGatePathResolvesLegacyDotGate`.
