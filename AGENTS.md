# AGENTS.md

This file is for agentic coding tools working in this repo.

This repository is a Go CLI app named `no-mistakes`.
The binary entrypoint is `cmd/no-mistakes`; implementation code lives under `internal/`, and the package names there are the layout map (CLI in `internal/cli`, daemon in `internal/daemon`, pipeline and steps in `internal/pipeline`, agent adapters in `internal/agent`, terminal UI in `internal/tui`, shared infrastructure in `internal/git`, `internal/ipc`, `internal/config`, `internal/db`, `internal/paths`, `internal/types`).
Build, test, and release commands are owned by the `Makefile`; read it for the full target list instead of relying on a copy here.

Safest local verification sequence after non-trivial changes:

- `gofmt -w .`
- `make lint` (generated-skill drift check plus `go vet`)
- `go test -race ./...` (the e2e suite is behind the `e2e` build tag and excluded)
- `make e2e` when touching agent integrations, the e2e harness, or recorded fixtures
- `go build -o ./bin/no-mistakes ./cmd/no-mistakes`

**Fork Routing**

- `repos.upstream_url` is the parent repository used for PR base routing; `repos.fork_url` is an optional GitHub fork push target.
- `no-mistakes init --fork-url <url>` expects `origin` to point at the GitHub parent repository and `<url>` at the contributor fork; plain `no-mistakes init` preserves an existing fork URL on idempotent refresh.
- Push code must use `Repo.PushURL()` so configured forks receive branch updates.
- GitHub PR code must keep `--repo` pointed at the parent and use `--head <fork_owner>:<branch>` when `fork_url` is set; existing-PR lookup must list by the bare branch and filter head-owner fields, never pass `<owner>:<branch>` to `gh pr list --head`.
- GitLab and Bitbucket fork MR/PR routing is intentionally out of scope until implemented end to end; if a legacy row has `fork_url` for those hosts, PR creation must skip instead of opening a self PR.

**GitLab Backend (`internal/scm/gitlab`)**

- The backend is pinned against `glab v1.5x`, whose flag surface drifts between versions: the auth check must be host-scoped (`--hostname <host>`, falling back to unscoped only when the host is unknown), `glab mr list` no longer accepts `--state opened`, and the daemon's detached-HEAD worktree breaks `glab ci get`, so pipeline jobs are read via the branch-independent `glab api .../pipelines/<id>/jobs` REST endpoint.
- The comments in `internal/scm/gitlab/gitlab.go` own the full rationale for each trap; extend them there when you hit new glab version drift.

**Documentation**

- Keep `README.md` concise and high-level; the bar needs to be extremely high for what shows up there.
- Most documentation lives in `docs/`, the published docs site.
- One owner per fact: `docs/src/content/docs/reference/global-config.md` and `docs/src/content/docs/reference/repo-config.md` own configuration keys, `docs/src/content/docs/reference/environment.md` owns environment variables and the telemetry local/remote split, `docs/src/content/docs/concepts/daemon.md` owns the daemon lifecycle model, and guides pages explain purpose and link to those owners instead of restating tables and examples.
- The `document.instructions` block in `.no-mistakes.yaml` states this ownership map for the pipeline's document step; update it when ownership moves.

**Agent-Guidance Surfaces**

- `skills/no-mistakes/SKILL.md` is **generated**: the source of truth is the `body` constant in `internal/skill/skill.go`. Edit the body, then `make skill`; `make lint` fails CI on drift. Never edit `SKILL.md` directly. `no-mistakes init` ships this rendering to agents at user level.
- Agent-driving guidance is owned by the skill body and the live `axi` output strings (`internal/cli/axi*.go`); `docs/src/content/docs/guides/agents.md` carries only the canonical invariant sentences pinned by `internal/cli/axi_guidance_test.go` plus a pointer to the skill. When you change driving guidance, change the skill body and the point-of-use `axi` strings together; that drift test is the sync check.
- Review auto-fix is disabled by default (`auto_fix.review: 0` in `config.go` `autoFixDefaults`), so blocking and ask-user review findings park for an agent decision; keep the skill, the live `axi` gate `note`, and docs qualified if you touch review auto-fix.

**Context, Concurrency, and Processes**

- Thread `context.Context` through long-running, subprocess, and networked work; prefer `exec.CommandContext`; use derived contexts and timeouts for cleanup and HTTP calls.
- Route every long-lived subprocess spawned for a cancellable step or agent invocation through `shellenv.ConfigureShellCommand(cmd)`: it creates a process-tree boundary and installs `cmd.Cancel` to kill the whole tree, so grandchildren (test workers, build watchers) cannot outlive cancellation and hold the next run's worktree locked.
- `cmd.Cancel` covers only cancellation; on clean exit or error the group is not reaped, and leaked grandchildren accumulate until the OS OOM-kills the daemon (surfacing as `daemon crashed during execution` with no stack trace). Use `shellenv.RunShellCommand` / `OutputShellCommand` / `CombinedOutputShellCommand` for one-shot commands, or `StartShellCommand` plus `TerminateShellCommandGroup` when handling pipes manually; the helper doc comments in `internal/shellenv` own the details. `ConfigureShellCommand` also installs a 5s `cmd.WaitDelay` backstop so a grandchild holding an inherited pipe cannot wedge `cmd.Wait` forever. Regressions: `TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit`, `TestRunShellCommandWithEnv_ReapsGrandchildOnCleanExit`, `TestTerminateShellCommandGroup_*`.
- On Windows the daemon runs console-less, so route every console child through `winproc.Harden(cmd)` (no-op elsewhere, idempotent, preserves existing creation flags) or a console window flashes per child (#287). `shellenv.ConfigureShellCommand` already calls it; one-shot commands built directly must call it themselves. Regressions: `TestHarden*` in `internal/winproc`.
- Protect shared mutable state with the standard sync/atomic tools, and be explicit about ownership and cleanup of goroutines, worktrees, temp dirs, and channels.

**Filesystem and Paths**

- Use `filepath.Join`; respect `NM_HOME` for app state; directories are `0o755` and files `0o644` by convention.
- On macOS, path comparisons may need symlink resolution (`/var` vs `/private/var`).

**Git on Bare Gate Repos (`safe.bareRepository`)**

- Agent harnesses and hardened CI inject `safe.bareRepository=explicit`, which forbids cwd-based discovery of bare repositories. Route every gate git call through `git.Run`, which detects a bare git dir and prepends `--git-dir=<dir>`; never shell out to git in a bare gate repo relying on `cmd.Dir` or `-C` discovery (issue #362).
- Regressions: `TestRunOnBareRepoUnderSafeBareRepositoryExplicit`, `TestWorktreeAddRemoveOnBareRepoUnderSafeBareRepositoryExplicit`, `TestInitUnderSafeBareRepositoryExplicit`.

**Post-Receive Hook Gate Path Resolution (`internal/git/hook.go`)**

- The hook's `--gate` value must never come from a bare `$(pwd)`: Git can invoke `post-receive` from a cwd that collapses to `.` (issue #269), which the daemon rejects and the pipeline silently never starts. The hook script resolves an absolute gate dir (git first, hook location fallback), and `normalizeNotifyGatePath` in `internal/cli/daemon_cmd.go` is an independent second layer that absolutizes whatever an already-installed older hook sends.
- Regressions: `TestPostReceiveHook_ResolvesAbsoluteGateDir`, `TestPostReceiveHook_FallsBackToHookLocationForGateDir`, `TestNormalizeNotifyGatePathResolvesLegacyDotGate`.

**Daemon Singleton Lock (`internal/daemon/lock.go`)**

- Only one live daemon may own an `NM_HOME`: an exclusive OS file lock on `<NM_HOME>/daemon.lock` is acquired as the very first action in `RunWithOptions`, strictly before stale-run recovery and socket bind, and held for the process lifetime. The kernel releases it on any process death, so a held lock always means a live holder and no staleness heuristic is needed. Without it, a second daemon stole the socket and ran global crash recovery against the live daemon's runs and worktrees.
- Independent layers: `internal/ipc` `listen()` dials the socket before unlinking it and refuses to steal a live one; client probes bound the dial with `daemon_connect_timeout` and fail fast on a dead or wedged socket instead of starting a replacement daemon (`EnsureDaemon` surfaces the error with a `daemon start` recovery hint; the health RPC itself is bounded separately by `ipc.DefaultDialTimeout`).
- Daemon execution is explicit-only (`no-mistakes daemon run --root`); never let inherited environment reinterpret probes like `--version` or `status` as daemon workers.
- Startup worktree cleanup is DB-aware: never remove a worktree whose run row is `pending` or `running`; `startRun` inserts the run row before creating the worktree, so a no-row directory is safe to remove immediately.
- The user-facing model lives in `docs/src/content/docs/concepts/daemon.md`; the lock rationale lives in the `internal/daemon/lock.go` and `daemon.go` comments. Regressions: `TestAcquireSingletonLock_*`, `TestRunWithResources_SecondDaemonForSameRootFailsWithoutStealingSocket`, `TestRunWithOptions_RequiresSingletonLockBeforeRecovery`, `TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree`, `TestServe_SecondListenerForLiveSocketDoesNotStealIt`, `TestDialConnectTimeoutFailsFastAndNamesSocket`, `TestIsRunningFailsFastWhenSocketAcceptsButDoesNotRespond`, `TestIsRunningSurfacesExistingDeadSocket`, `TestDaemonRunRootFromArgs_EnvDoesNotForceDaemonModeForProbes`, `TestValidateDaemonPIDFallback_RefusesToKillOwnProcess`.

**Destructive Daemon Lifecycle Guard (`internal/lifecycle/guard.go`)**

- `daemon stop`, `daemon restart`, and `update` refuse by default while pending/running runs exist (the daemon is machine-wide, so stopping it can fail every active pipeline), list the runs via the shared `lifecycle.ActiveRuns`/`lifecycle.RunList` helpers, and require an explicit `--force`. `update -y` answers only the different-executable prompt and deliberately does not bypass this guard.
- Every invocation of the three commands is logged with caller attribution (PID, PPID, parent command line) via `logLifecycleInvocation` to `<NM_HOME>/logs/cli.log`; this is the incident forensic trail, do not remove or weaken it.
- Regressions: `TestDaemonStopRefusesWithActiveRunsAndListsThem`, `TestDaemonStopForceOverridesActiveRunGuard`, `TestDaemonRestartRefusesWithActiveRuns`, `TestLifecycleCommandsWriteCallerAttributionToCLILog` (`internal/cli/daemon_lifecycle_test.go`), `TestUpdaterRunRefusesWithActiveRunsAndListsThem`, `TestUpdaterActiveRunGuardAllowsForce` (`internal/update`).

**Testing Conventions**

- Prefer e2e tests for behavior that crosses a process or I/O boundary (CLI flags, config loading, git operations, agent spawning, daemon coordination, stdout/stderr, recorded fixtures); unit-test pure helpers where speed and failure localization matter. Prefer creating real git repos in temp dirs over heavy mocking.
- The e2e suite is behind the `e2e` build tag; `make e2e` sweeps `./internal/e2e/...` and `./internal/pipeline/steps/...`, so keep new step-local e2e tests behind the tag too.
- Packages whose tests shell out to git unset `GIT_CONFIG_COUNT` in `TestMain` so ambient `GIT_CONFIG_*` injection from agent harnesses cannot leak in; a test exercising injected config re-sets it with `t.Setenv` (see `internal/git`, `internal/gate`, `internal/daemon`, `internal/pipeline/steps`).
- Packages whose tests can start a daemon or touch ambient state (`cmd/no-mistakes`, `internal/cli`, `internal/update`) use a package-wide `TestMain` that points `NM_HOME` and `HOME` at fresh temp dirs and disables telemetry/update-check env vars, so a full test run never touches a real `~/.no-mistakes`. Follow the same pattern in new such packages.
- Isolate filesystem and environment state with `t.TempDir()` and `t.Setenv()`.

**Repo Config Trust Boundary (security)**

- The daemon runs `commands.*` from `.no-mistakes.yaml` verbatim via `sh -c`, and `agent` selects which process launches with the maintainer's credentials. The code-executing selection fields (`commands.{test,lint,format}` and `agent`) are therefore loaded from the trusted default branch at a **pinned SHA** resolved by a fresh fetch, never from the pushed SHA. The run aborts when the trusted commit or its present config cannot be read and parsed; a readable tree with no config is valid. See `internal/daemon/manager.go` `startRun`, `loadTrustedRepoConfig`, and `assertGateTrustedConfigReadable`.
- `document.instructions` (the repo's documentation placement policy) and `disable_project_settings` (the gate-agent project-instruction opt-out) are also trusted-only: a pushed branch must not weaken either boundary. When the opt-out is enabled, only adapters with verified effective suppression may launch. Non-executing fields (`ignore_patterns`, `auto_fix`, `intent`, `test`) are still read from the pushed branch.
- `allow_repo_commands` is per-repo, read only from the trusted default-branch copy, and defaults `false`; a contributor cannot self-enable it from a pushed branch. The e2e harness models a trusted single-developer environment and commits `allow_repo_commands: true` via `SetupOpts.AllowRepoCommands`; security tests pass `false`.
- Regressions: `TestLoadTrustedRepoConfig_FailClosedOnFetchFailure`, `TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch`, `TestEffectiveRepoConfig_DocumentPolicyTrustedOnly`, `TestEffectiveRepoConfig_DisableProjectSettingsTrustedOnly`, `TestAssertGateTrustedConfigReadable_*`, `TestNewPipelineAgent_OptOut_*`, `TestLoadRecoveredConfig_BoundsFetchAndFailsClosed`, e2e `TestRepoConfigCommandsFromDefaultBranch` (incl. `pushed_branch_cannot_self_enable`).

**CI Monitor Lifecycle**

- `ci_timeout` is an idle timeout, not an absolute deadline: only `timeoutAnchor` re-arms when the upstream default-branch tip advances, `started` stays fixed for poll pacing, and re-arm only ever extends the deadline (fail-safe on transient base-tip failures). Value semantics (`0` unset, negative unlimited sentinel, keyword parsing) live in `config.go`; keep `config.DefaultCITimeout` and `defaultConfigYAML` in sync (`TestDefaultConfigYAML_MatchesGoDefaults`). User-facing semantics are owned by `docs/src/content/docs/reference/global-config.md`.
- Reap an orphaned monitor from outside its worktree with `no-mistakes axi abort --run <id>`; it needs only `NM_HOME` plus the daemon, and an unknown id or stopped daemon is an idempotent no-op, not an error. Bare `axi abort` stays worktree/branch-scoped.

**Parked / Awaiting-Agent Signal**

- `runs.awaiting_agent_since` is non-nil **iff** a step is actually parked at an `awaiting_approval`/`fix_review` gate: the executor sets it on gate entry, clears it when `waitForApproval` returns, and `RecoverStaleRuns` clears it on crash recovery. It is observability only (rendered as `awaiting_agent: parked <duration>` in `axi status`) and never changes gate resolution, auto-resume, or the `--yes` default.
- Tests: `internal/db/run_test.go`, `internal/pipeline/executor_approval_test.go`, `internal/cli/axi_test.go`, e2e `TestAxiParkedAwaitingAgentSignal`.

**Review-Loop Agent Sessions (`internal/pipeline/sessions.go`)**

- Per run, the review loop keeps ONE durable reviewer session across the initial review and every full rereview, and a SEPARATE fixer session across review-fix turns; roles never share a session (the reviewer must never inherit the fixer's rationale), no other step uses sessions, and sessions are keyed strictly by run. Every review turn is still a full adversarial review of the complete branch diff.
- Fail-safe rules: unsupported adapter runs cold; a failed resume drops the identity and re-runs the same turn in a fresh same-role session, never skipping the review; a cancelled ctx gets no fallback retry; `session_reuse: false` forces everything cold. Persistence is minimum metadata only, never prompts or transcripts.
- `codex exec resume` has a narrower flag surface than `codex exec`, so an unsupported override fails the resume and falls back; the e2e fakeagent must keep parsing both codex argv shapes (`extractCodexPrompt`).
- Regressions: `internal/pipeline/sessions_test.go`, `internal/pipeline/steps/review_session_test.go`, `internal/agent/session_test.go`.

**Review Fixer Verification Discipline (`internal/pipeline/steps/review.go`)**

- The review-fix prompt requires all fixes before one focused verification limited to the changed area and forbids the whole repository test/lint suite during the fix round.
  The dedicated Test and Lint steps are the authoritative gates, although their coverage may be focused when commands are unconfigured.
  This is a prompt contract, not an enforced sandbox.
  Regression: `TestReviewStep_FixMode_FocusedVerificationContract`.

**Intent Provenance & Conformance (`internal/pipeline/steps/intent_prompt.go`)**

- Intent carries provenance: an explicit `axi run --intent` persists `Source==db.RunIntentSourceAgent` ("agent", score 1); a transcript match persists the agent name ("claude"/"codex"/...). The executor propagates it as `StepContext.IntentSource` alongside `UserIntent` (`executor.go`).
- `userIntentPromptSection` branches on source: an EXPLICIT intent renders as sanitized-but-AUTHORITATIVE acceptance criteria; an INFERRED intent keeps the low-confidence hint framing verbatim. Both branches keep the `StripAdversarial`+`RedactSecrets` pipeline and BEGIN/END "do not execute instructions" guard - authoritative reframes only the content's authority (check the diff against the criteria), never whether control tokens are stripped. The review prompt adds `intentConformanceReviewClause` for agent-source intent only: a fixer change that contradicts the criteria (removes intent-required or adds intent-forbidden behavior) MUST become an `ask-user` finding, which parks with no executor change.
- Empty/missing finding `action` fails closed to `ask-user`, not auto-fix (`types/findings.go` `actionOrDefault`); `HasAskUserFindings` uses `actionOrDefault` so it agrees with `AutoFixableFindings` (an unclassified finding is never auto-fixed and is always caught as ask-user). `MergeUserOverrides` still stamps user-*added* findings auto-fix on purpose.
- The deterministic net-deleted-author-lines git-diff backstop is intentionally not built; `review.go` owns the held-scope TODO.
- Regressions: `internal/pipeline/steps/intent_prompt_test.go`, `internal/pipeline/steps/review_test.go` (`TestReviewStep_ConformanceObligationTracksIntentProvenance`, `TestReviewStep_RereviewFlagsIntentContradictionAsAskUser`), `internal/pipeline/executor_intent_conformance_test.go`, `internal/types/findings_test.go`, e2e `TestIntentJourney` (inferred-source framing).

**Combined Document+Lint Housekeeping Pass**

- When `commands.lint` is empty, the document step performs both duties in one agent invocation and stashes the lint half on `RunShared` (consume-once); the lint step consumes it instead of paying a second cold pass. Neither duty is ever silently dropped: a skipped pass, untrusted structured output, or a lint fix round falls back to lint's own agent pass. Configured `commands.lint` stays a first-class deterministic gate. Uncategorized findings fail safe to the stricter documentation gate.
- The document prompt enforces the placement policy (one owner per fact, stale duplicates become pointers, no AGENTS.md postmortems, scope limited to docs the change made stale). Do not reintroduce exhaustive-corpus-sweep language; it caused doc commits in 90 of 121 audited PRs. Contract test: `TestDocumentStep_PromptAppliesPlacementPolicy`; behavior tests: `internal/pipeline/steps/housekeeping_test.go`.

**Telemetry Shape**

- Read-only surfaces (`axi` home/status/logs, `status`, `runs`) emit NO pageview and gate their command event through `telemetry.ReadSurfaceGate` (emit on state-fingerprint change, else at most once per 10 min, persisted at `<NM_HOME>/telemetry-gate.json`). Never reintroduce the pageview+command double emit for read surfaces - `axi-status` alone was 42% of all remote event rows. Mutation surfaces stay full-fidelity via `trackAxiSurface`/`trackCommand`.
- Detailed performance evidence is LOCAL-ONLY (`agent_invocations` rows plus `runs.parked_ms`); never store prompts, outputs, diffs, or raw command arguments there (shape-guard test `TestAgentInvocations_PrivacySafeShape`) and never send run IDs, paths, session identities, or per-invocation records to Umami - the only remote perf data is three bounded counts on the terminal `run finished` event. The local/remote split is documented in `docs/src/content/docs/reference/environment.md`; read locally with `no-mistakes stats`.
- Session-fidelity metric counts and timing boundaries have ONE authoritative home, `internal/agent/invocationmetrics.go` (tool-category classifier, `InvocationMetrics`, `FreshInputTokens`, `PerRoundTokens`, `ModelTimeMS`); the codex adapter fills them from its live `exec --json` event stream (`codex_metrics.go`) and the additive fidelity fields plus cache-creation usage are nullable so a not-reported datum is stored as NULL, never a fabricated zero. Codex's live stream exposes neither the model (resolved best-effort from the `~/.codex/sessions` rollout) nor internal model-request counts (it batches one exec into a single `turn.completed`, so round-trips are counted from completed items and subprocess wait is the reader-timed tool-item interval); codex usage is cumulative across a resumed session, so per-round deltas subtract the same session's prior cumulative (`Result.SessionUsageCumulative`). Regressions: `internal/agent/invocationmetrics_test.go`, `internal/agent/codex_metrics_test.go`, `internal/pipeline/instrument_fidelity_test.go`, `internal/db/agent_invocation_test.go` (`TestOpenMigratesSessionFidelityColumns`).

**Rebase Base & Force-Push Safety (data-loss prevention)**

- The whole job of this tool is to not lose people's code; favor refusing the push and surfacing a finding over any clever recovery. The comments in `internal/pipeline/steps/forcepush.go` own the full reasoning; the invariants are the next three bullets.
- Rebase bases come from the freshly fetched authoritative remote refs, never local or stale state; and a branch built on unpushed local-default-branch commits parks with `NeedsApproval` + `AutoFixable=false` instead of silently widening the PR (`detectBundledLocalDefaultCommits`, #283).
- Every force-push routes through `resolveForcePushDecision`, which re-reads the live remote head and allows the push only for a new branch, an already-equal remote, an unchanged `lastSeenSHA`, or remote commits already incorporated by patch-id (excluding `^baseSHA` history the run knowingly rewrites). Anything else refuses, and a failed ls-remote/fetch fails closed; never degrade to a bare `--force`/`--force-with-lease` without an explicit anchor.
- `lastSeenSHA` must stay the head the run last **observed**, never the live remote tip: the rebase step refreshes `origin/<branch>` only on a normal push, NOT on a force push, and the CI step passes `Run.HeadSHA`. Anchoring the lease to a SHA read immediately before pushing is the original #281 bug (it always passes and protects nothing); always-fetching the branch on force push recreates it. Never reintroduce either.
- Regressions: `TestCIStep_CommitAndPush_RefusesToClobberUnseenUpstreamCommit` (#281), `TestPushStep_RefusesToClobberAdvancedUpstreamBranch` (#305), `TestForcePushRun_RefusesToClobberOutOfBandBranchCommit`, `TestRebaseStep_DetectsUnpushedLocalDefaultBranchCommits` (#283), `TestResolveForcePushDecision_*`.

**macOS Release Signing (permanent identity)**

- Every official macOS release artifact - both `darwin/arm64` and `darwin/amd64` - is Developer ID Application signed on a macOS runner with a fixed identifier, hardened runtime, secure timestamp, and no entitlements, then strictly verified before it is archived or checksummed; the Linux and Windows release paths are unchanged.
- The executable identifier `com.kunchenguid.no-mistakes` and Team ID `9T2J7MNUP9` are the permanent Developer ID identity and MUST NEVER change: they are the invariant of the identity-based designated requirement that lets macOS permission grants survive `no-mistakes update`, so changing either resets every grant once.
- Signing runs only in the darwin build job gated behind the `release-signing` GitHub environment; the certificate is the base64 `CSC_LINK` secret unlocked with `CSC_KEY_PASSWORD`, imported into an ephemeral keychain with a runtime-generated password that is deleted on success and failure, and no other job may reference those secrets.
- Signing happens before tarball creation and checksum generation, and the verify gate fails the release closed on any missing or ambiguous signature, wrong Team ID, non-permanent identifier, content-based (`cdhash`) requirement, missing hardened runtime or timestamp, or wrong architecture.
- Mechanics live in `.github/workflows/release.yml`; the contract is pinned by the root `TestReleaseWorkflow*` static tests in `workflow_release_signing_test.go`, and secret values are never recorded here or in any test fixture.
- Notarization, stapling, a PKG, Homebrew, and universal binaries are intentionally out of scope for this phase.

**When Making Changes**

- Whenever you must bring in new dependencies, check latest documentation for knowledge, and discuss with the user.
- Always use test driven development for bug fixes and feature development.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
