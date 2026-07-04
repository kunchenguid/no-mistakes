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

**GitHub Backend (`internal/scm/github`)**

- `Host.GetChecks` runs `gh pr checks <n> --json name,state,bucket,completedAt` and relies on a subtle `gh` behavior: `gh pr checks` documents special exit codes (non-zero for failing, **exit 8 for pending**), but those are applied **only in table-render mode**. With `--json` the command early-returns via the exporter (`return opts.Exporter.Write(...)`) *before* the exit-code logic, so it exits **0 for passing, failing, AND pending** checks (verified against `gh` v2.95.0, cli/cli `pkg/cmd/pr/checks/checks.go`). The check state is read from the parsed `bucket` field, not the exit code. The only non-zero exits `GetChecks` sees with `--json` are the "no checks reported" case (output contains that string → treated as an empty result, `nil, nil`) and genuine CLI errors (surfaced as an error). Do NOT "fix" `GetChecks` to treat a non-zero exit as failed/pending — that would misclassify the "no checks" case and break on any error. If a future `gh` release starts applying exit 8 in `--json` mode, revisit this. Regression: `TestGetChecksExitConditions` pins pass/fail/pending (exit 0) plus the no-checks and error exits.

**Test-file Detection (`isTestFile`, `internal/pipeline/steps/common_diff.go`)**

- `isTestFile` is **path-based only** (it never reads file contents), so each language is matched by naming/location convention: Go `*_test.go`, Rust `*_test.rs`, Python `test_*`/`*_test.py`, Ruby `test_*.rb`, Java `*Test(s).java`, JS/TS `*.test|spec.{js,ts,jsx,tsx}`, and Swift/XCTest `*Test(s).swift` or any `.swift` under a `Tests/` directory (the SPM layout). XCTest is matched by these path patterns, not by scanning for `XCTestCase`. `detectNewTestFiles` uses this to flag agent-authored test files at the gate. Regression: `TestIsTestFile`.

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
- `make e2e` sweeps both `./internal/e2e/...` (full journey suite) and `./internal/pipeline/steps/...`, so step-local e2e tests (e.g. `internal/pipeline/steps/*_e2e_test.go`, gated by `//go:build e2e`) are also covered. Keep new step-local e2e tests behind the `e2e` tag so `go test ./...` still skips them.

**Repo Config Trust Boundary (security)**

- The daemon runs `commands.*` from `.no-mistakes.yaml` verbatim via `sh -c`, and `agent` selects which process launches (incl. `acp:` targets) with the maintainer's credentials. To prevent supply-chain RCE, the **code-executing selection fields (`commands.{test,lint,format}` and `agent`)** are loaded from the trusted default branch, never from the pushed SHA. See `internal/daemon/manager.go` `startRun` + `loadTrustedRepoConfig`, and `config.EffectiveRepoConfig`.
- `startRun` fetches the default branch, resolves it to an exact commit SHA (`git.ResolveRef`), and `loadTrustedRepoConfig` reads `.no-mistakes.yaml` at that **pinned SHA** (not the `origin/<defaultBranch>` ref name). On fetch failure (or if the ref does not resolve) the trusted SHA is empty → `loadTrustedRepoConfig` returns nil → `EffectiveRepoConfig` forces empty `commands`/`agent`. This fails closed: a stale `origin/<default>` ref left in the shared bare repo by a previous run cannot serve a value the live default branch removed. Regression tests: `TestLoadTrustedRepoConfig_FailClosedOnFetchFailure`, `TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch`.
- Non-executing fields (`ignore_patterns`, `auto_fix`, `intent`, `test`) are still read from the pushed branch.
- `allow_repo_commands` is **per-repo, read from the trusted default-branch copy of `.no-mistakes.yaml`** (declared on `RepoConfig`), never the global config and never the pushed SHA. It defaults `false`; when `true` the maintainer has opted in to honoring the pushed branch's `commands` and `agent` wholesale. A contributor cannot self-enable it from a pushed branch. When changing this logic, keep `commands`/`agent` locked to the default branch and update the e2e test `TestRepoConfigCommandsFromDefaultBranch` (incl. the `pushed_branch_cannot_self_enable` subtest).
- The e2e harness models a trusted single-developer environment, so it commits `allow_repo_commands: true` to the default-branch `.no-mistakes.yaml` via `SetupOpts.AllowRepoCommands`; security tests pass `false` to exercise the secure default.

**Custom Command Steps & Per-Step Options (`steps:` mapping form)**

- A `steps:` entry can be a mapping (not just a scalar built-in name). A mapping carrying a `command:` is a **custom command step** (`config.StepSpec.IsCommand()` → `steps.CommandStep`); a mapping with only `name:` is a built-in with options. `config.StepSpec` grew `Command`, `FindingsJSON`, `Timeout` (Go duration), `AutoFix`, `Instructions []string`; `StepSpec.UnmarshalYAML` accepts scalar OR mapping (other node kinds still error). Because `StepSpec` now has a slice field it is no longer `==`-comparable — use `config.StepSpecsEqual` (that's why `manager.go` no longer uses `slices.Equal` on `Steps`).
- `steps.BuildPipeline` builds a `CommandStep` for command specs and a built-in constructor otherwise; `validateStepSpecs` adds custom-step rules (name matches `^[a-z0-9][a-z0-9_-]*$`, must not collide with a built-in, unique across ALL step names since the executor keys step records + `<name>.log` files by name) on top of the shared push-chain ordering checks (`stepChainProblems`, also used by the scalar-only `validateStepNames`).
- `CommandStep` (`internal/pipeline/steps/command.go`): runs its command via `runShellCommandWithEnv` under a per-step `context.WithTimeout` (default `DefaultCommandTimeout` = 30m). Distinguishes a **run cancellation** (`sctx.Ctx.Err() != nil` → return error, fail the run) from a **step timeout** (`ctx.Err()==DeadlineExceeded` → gated timeout finding, not auto-fixable). Contract #1 (structured findings): a `findings_json` path is parsed into real per-line `types.Finding`s (`parseCommandFindings` accepts the findings object OR a bare array); absent file → exit-code fallback (parity with lint/test). `auto_fix: true` sets `AutoFixable` + finding `Action=auto-fix` and is honored by `config.AutoFixLimit` (custom lookup returns `DefaultCommandAutoFixLimit`=3); default false parks for the agent (`Action=ask-user`). In fix mode it drives the agent like lint/test then re-runs the command.
- Contract #2 (per-step instructions, SECURITY): `instructions:` file **contents** are read at the **trusted default-branch SHA** via `git.ShowFile` in `loadTrustedStepInstructions` (`manager.go`, next to `loadTrustedRepoConfig`), never the pushed worktree — enforced regardless of `allow_repo_commands` (only the paths may come from a pushed branch under opt-in; content is always trusted). Empty trusted SHA → no instructions (fail closed). The resolved text lands on `config.Config.StepInstructions` and is injected (sanitized via `intent.StripAdversarial(sanitizePromptMultilineText(...))`, wrapped in BEGIN/END markers) into the built-in agent steps by `stepInstructionsPromptSection`, appended to the `executionContextPromptSection()+roundHistoryPromptSection(sctx)+userIntentPromptSection(sctx)` chain in review/test/lint/document. Regression: `TestLoadTrustedStepInstructions_ReadsTrustedSHANotWorkingTree` (working-tree edit must NOT change injected content).
- Backward compat: an empty `steps:` list still yields the exact default pipeline (`TestBuildPipeline_NilMatchesDefaultPipeline`).

**iOS / Xcode Testing (recipe, no engine code)**

- iOS testing is a **configuration story, not engine code**. `xcodebuild test` is just a shell command, so the built-in test step (`internal/pipeline/steps/test.go`) runs it via `commands.test` today: non-zero exit → auto-fixable gate, and the fix-loop prompt already asks the agent to triage a failure as real product bug vs. fixable setup vs. flaky infra. Recommended one-liner: `xcodebuild test -project App.xcodeproj -scheme App -destination 'platform=iOS Simulator,name=iPhone 16' -quiet`. `commands.test` is a trusted default-branch field (see the security section above).
- For a slow simulator run alongside fast unit tests, prefer a PR2 `type: command` step (`steps: - name: ios-test / command: xcodebuild ... / timeout: 30m`) ordered *after* the fast `test`/lint steps. The per-step `timeout` (`CommandStep`, default `DefaultCommandTimeout` = 30m) is the point: a hung simulator otherwise blocks the run until `axi abort`; a timeout gates with a non-auto-fixable finding instead.
- Preconditions are **host-environment** problems no config fixes: pin `-destination` to an installed simulator (`xcrun simctl list devices available`), pre-install the runtime (`xcodebuild -downloadPlatform iOS`), budget for cold first-boot latency, and keep Xcode version/license/`xcode-select` correct on the daemon host.
- The `.xcresult` → per-test-findings summarizer is **deliberately deferred** (not shipped). The growth path is the existing `findings_json` seam on `CommandStep`: a repo-side wrapper (`xcodebuild test -resultBundlePath` + `xcrun xcresulttool get --format json`) maps failed tests to finding objects. A built-in parser is avoided because `xcresulttool`'s JSON schema drifts across Xcode versions. Docs: `docs/src/content/docs/guides/ios-testing.md` (sidebar entry in `docs/astro.config.mjs`).

**Skill-Driven Steps (`type: skill`, `mode: review`)**

- A `steps:` mapping carrying a `skill:` path is a **skill-driven step** (`config.StepSpec.IsSkill()` → `steps.SkillStep`, `internal/pipeline/steps/skillstep.go`). It is the SAME machinery as the built-in `review` step — a prompt template + a findings JSON schema handed to `sctx.Agent.Run` — but the prompt body comes from a repo-owned skill markdown file. Agent-agnostic by construction: the body is inlined into the prompt (no skill-invocation channel; works with codex/opencode). `config.StepSpec` grew `Type`, `Skill`, `Mode`, and the resolved `SkillBody` (compared by `StepSpecsEqual`, populated by the daemon — never parsed from YAML). `IsCommand()`/`IsSkill()` are mutually exclusive; `validateStepSpecs`/`validateCustomStepSpec`/`validateSkillStepSpec` enforce it plus: name pattern + no built-in collision (shared with command steps), a `type:` that agrees with the payload, a repo-relative non-escaping `skill` path, and `mode ∈ {"", "review"}` (PR3 ships read-only `review` only; `revise` is a later PR and must NOT be pulled forward). `isCustomStepSpec` routes a `type: skill`/`command` spec with a missing payload to the custom validator so the error is precise, not "unknown step".
- **Prompt composition = three fixed layers** (mirrors `review.go` exactly, `SkillStep.reviewPrompt`): (a) engine-owned context header (branch, base/target commit, review scope, default branch, ignore patterns + `executionContextPromptSection()+roundHistoryPromptSection+userIntentPromptSection+stepInstructionsPromptSection`); (b) repo-owned skill body, frontmatter stripped by `skillPromptBody`, fenced in BEGIN/END SKILL markers; (c) engine-owned read-only output contract with the review `ask-user`/`auto-fix`/`no-op` vocabulary. Enforced with the shared `findingsSchema` (NOT `reviewFindingsSchema` — no risk fields). Gate on `hasBlockingFindings`; `AutoFixable = len(findings)>0`; default `auto_fix: 0` so findings park like built-in review (`config.AutoFixLimit` custom lookup now matches `IsCommand() || IsSkill()`). Fix rounds reuse `executeFixMode` + `commitAgentFixes` (`runFix`).
- **Trusted skill loading (SECURITY)**: the skill **body** is read at the **trusted default-branch SHA** via `git.ShowFile` in `loadTrustedSkillBodies` (`manager.go`, next to `loadTrustedStepInstructions`), never the pushed worktree — enforced regardless of `allow_repo_commands` (the `steps:` list including the skill path is already trusted-only under the secure default; the body content is ALWAYS trusted). The resolved bodies ride on `cfg.Steps` into `BuildPipeline`. Empty trusted SHA / absent file → empty body → the step **fails closed**: `SkillStep.Execute` parks with a misconfiguration finding (`misconfiguredOutcome`) rather than running an empty or pushed-branch prompt. Regression: `TestLoadTrustedSkillBodies_ReadsTrustedSHANotWorkingTree` (unit) + `TestSkillStepTrustedSHA` (e2e — pushed-branch skill edit ignored).
- **Read-only guard (enforced, not hoped)**: `mode: review` must not mutate the worktree. `SkillStep.enforceReadOnly` snapshots `git status --porcelain` before the review agent runs and, if the tree changed after, discards the edits (`git reset --hard` + `git clean -fd`) and appends a warning finding ("skill modified the worktree during a review-mode step; changes were discarded") that gates the step. The guard runs only on the read-only review pass, NOT in fix mode (a user `fix` round is allowed to edit and commits via `commitAgentFixes`). Regression: `TestSkillStep_WorktreeGuard_ResetsAndWarns`.
- Gate flow is identical to built-in review (`TestSkillStepGateFlow` e2e): findings park; `axi respond --action fix --findings <id> --instructions "..."` weaves the user's guidance (as finding `UserInstructions`, via `sanitizedPreviousFindingsForPrompt`) into the fix-round prompt. The e2e harness gained `SetupOpts.RepoExtraFiles` to commit a trusted skill body to the default branch. A first-party template ships at `.no-mistakes/skills/review.md`.

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
- **Every force-push is lease-guarded against discarding unseen upstream commits.** All force-push sites (`PushStep` in `push.go`, CI auto-fix `pushUpdatedHeadSHA` in `ci_fix.go`) route through `resolveForcePushDecision` (`internal/pipeline/steps/forcepush.go`). It re-reads the live remote head and allows the push only when: the branch is new; the remote already equals the head; the remote still equals `lastSeenSHA` (what the run last observed); or every commit now on the remote is already incorporated **by patch-id** (`git rev-list --cherry-pick --right-only HEAD...current`), excluding history the run is knowingly rewriting (`^baseSHA`, i.e. reachable from the run base - the common amend or reverting the pipeline's own autofix). Anything else returns `forcePushWouldDiscardError` and the caller must NOT push. An out-of-band commit reaches the branch *after* the run base, so it is never an ancestor of `baseSHA` and stays flagged.
- **`lastSeenSHA` must stay the head the run last *observed*, never the live remote tip.** The push step passes the remote-tracking ref the rebase step synced (`lastFetchedBranchTip`); the CI step passes `Run.HeadSHA`. Both callers also pass `Run.BaseSHA` for the `^baseSHA` exclusion. Critically, **the rebase step refreshes `origin/<branch>` only on a normal push, NOT on a force push** - on a force push it skips both the rebase-onto and the fetch, so the tracking ref stays the last-observed head. If it refreshed on a force push, `lastSeenSHA` would equal the live tip, the `current == lastSeenSHA` fast path would pass without the content check, and an out-of-band commit on the branch would be silently clobbered. Anchoring the lease to a SHA read from the remote *immediately before pushing* is the original #281 bug (it always passes and protects nothing); making the rebase always-fetch the branch was the same bug re-created for the force-push path. Never reintroduce either, and never degrade to a bare `--force`/`--force-with-lease` without an explicit anchor when ls-remote/fetch fails (fail closed instead). Regressions: `TestCIStep_CommitAndPush_RefusesToClobberUnseenUpstreamCommit` (#281), `TestPushStep_RefusesToClobberAdvancedUpstreamBranch` (#305), `TestForcePushRun_RefusesToClobberOutOfBandBranchCommit` (force-push fast-path clobber), and `TestResolveForcePushDecision_*`.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.
