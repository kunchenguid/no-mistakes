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

## Where detail lives

- User-facing semantics: the docs site under `docs/src/content/docs/` (`reference/repo-config.md` and `reference/global-config.md` own config keys, `reference/pipeline-steps.md` owns step behavior, `reference/environment.md` owns env vars and telemetry, `concepts/gate-model.md` and `concepts/daemon.md` own those models).
- Per-area implementation maps (owning functions, invariants, regression lists): `.agents/skills/<area>/SKILL.md` (`.claude/skills` is a symlink to it). Read the matching skill before changing that area.
- Provider CLI traps (glab flag drift, tea JSON shapes, GitHub user-attachments, opencode failure wire shape and retry gating) are owned by the comments in `internal/scm/gitlab/gitlab.go`, `internal/scm/gitea/gitea.go`, `internal/scm/github/attachments.go`, and `internal/agent/opencode*.go`; extend them there when you hit new drift.
- `skills/no-mistakes/SKILL.md` is generated from `internal/skill/skill.go`; never hand-edit it, `CHANGELOG.md`, or other generated files.

## Invariants every change must keep

- Repo config trust boundary (security): `commands.*` and `agent` come from the trusted default branch at a pinned, freshly fetched SHA and the run fails closed when that config cannot be read; `allow_repo_commands` is trusted-only and defaults false. Every other trusted-only field listed in `reference/repo-config.md` stays trusted-only regardless of `allow_repo_commands` (`pr.base_branch` is the one opt-in exception). No trusted-config selection may depend on a pushed-branch field, and `review.path_instructions` matches the complete changed-file set, never the `ignore_patterns`-filtered one. Owner: `repository-routing-security` skill.
- Under `disable_project_settings`, only adapters with verified effective suppression of the target repo's project instructions may launch; anything unverified fails closed (Grok is refused, and an `acp_registry_overrides` entry for `omp` fails closed). Owner: `repository-routing-security` skill.
- An existing PR's live forge base outranks a since-changed `pr.base_branch`, and PR lookup matches by branch alone so a base change never opens a duplicate PR. Owner: `pr.base_branch` in `reference/repo-config.md`.
- Repository `gates` can never skip, reorder, or replace a core step, and `--skip` refuses gate names. Only `rebase`, `review`, `test`, `document`, and `lint` are anchors; a run's gate list is pinned once and an unparseable pin fails recovery closed. Owner: `repository-routing-security` skill.
- A per-run Pi pin is immutable and fully validated before it supersedes an active run.
- Test: pass/fail scenarios must be live and untested ones need a reason; `untested` never parks; the evidence turn is unconditional; the analyzer correction turn stays correction-only (no tools, scenario reruns, or external operations). Do not weaken these to avoid the analyzer correction retry.
- Test verdicts: `no-surface` and `inconclusive` park for an operator decision, a `fail` scenario with any verdict but `no-go` is rejected, `no-surface` with any live or pass/fail scenario is rejected, and the attestation omits `live_validation` once the head moves past the validated one. Owner: `pipeline-review-and-agents` skill.
- A Test `no-go` verdict parks as an auto-fixable error. Owner: `.agents/skills/pipeline-review-and-agents/SKILL.md`.
- Review: an unreadable review never passes (bounded schema reruns, fix-mode turns never retried); a clean review certifies the head only with `reviewed_paths` covering every trusted reviewable path; a selected finding leaves the append-only outstanding set only on positive coverage or an explicit operator decision. Recovery never lets a reused positional finding ID alias an unrelated finding as selected (`retainFindingIDsByIdentity`). The loop is bounded only by `auto_fix.review` and the gate.
- Review retries: findings come only from the attempt that validates; Claude's `error_max_structured_output_retries` fails the step with no second retry layer; a crash-resumed fix round fails closed, and a validation restart at Review starts a fresh carry set. Owner: `pipeline-review-and-agents` skill.
- An approval override is never a silent clean pass: CI overrides and qualifying Test exceptions record their unresolved condition and surface as `passed-with-override`; do not reuse the configured-command `override_reason` for every Test approval. `verify.py` refuses a configured-command Test override unless trusted `test.allow_approve_over_failure` records a reason, and a CI-repair restamp never copies a previous `allow_test_command_override`. Owner: `pipeline-review-and-agents` skill.
- The CI step never counts or limits fix rounds (the executor enforces `auto_fix.ci`); review-bot checks park as ask-user and never spend a round; a published repair must call `sctx.MarkRunning()`. Classification reads provider structure only, an empty check `App` is never a review bot, and `step_results.ci_fix_attempts` is never read. Owner: `ci-monitor` skill.
- After a CI repair, a still-red check without fresh evidence keeps the monitor waiting rather than re-escalating. Owner: `.agents/skills/ci-monitor/SKILL.md`.
- `types.Finding` fields are copied explicitly by `Finding.UnmarshalJSON` and `findingWire`; a new field must be added there or it drops on every parse.
- `rebase.strategy: merge` proves the merge (`mergeWithAgent`) rather than trusting the agent, and a rejected attempt is restored to the pre-merge head, fail-closed; its additive resolver prompt must not be unified with the rebase prompt. The merge target is resolved to a SHA before `git merge`, an unrecognized `rebase.strategy` fails the config closed, and a fast-forward publishes without `--force`. Owner: `branch-sync-and-push-safety` skill.
- Push attests the proposed head in an existing PR before pushing, and an attestation write failure aborts the push; the protocol is single-publisher by design (no cross-publisher coordination without a separate requirement). Private-mirror reconciliation bypasses patch proof only for the run's exact submitted or last-pushed head (Decision 41-A in `concepts/gate-model.md`).
- Private-mirror reconciliation archives a branch's exact head before deleting its ref and restores that archive when settlement fails and no intervening ref appeared; `push.go` settles shared gate/worktree refs only once. Owner: `concepts/gate-model.md`.
- OpenCode: a failed turn that already ran a tool is never retried or sent through the prompt-only fallback (both replay its side effects in a fresh session), and retry trusts opencode's `isRetryable`, never substring matching. Errors decode from the nested `data` payload, and the general `info.error` branch stays after the tool-choice conflict checks that trigger the fallback. Owner: comments in `internal/agent/opencode*.go`.
- Provider CLIs: glab's auth check is host-scoped and `glab mr update` never gets `-y`; every tea call carries `--login`, tea's list and single-PR JSON shapes stay separate structs, retargeting uses `tea api --method PATCH`, `CreatePR` re-lists instead of scraping stdout, Gitea runs are picked by highest run ID, and Gitea `MergeableState` stays declined. Owner: comments in `internal/scm/gitlab/gitlab.go` and `internal/scm/gitea/gitea.go`.
- GitHub user-attachment uploads fail closed: any error or refusal keeps the prior PR rendering, never a dead attachment URL. Owners: `internal/scm/github/attachments.go`, `test.evidence.attach_media` in `reference/global-config.md`.
- Custody recovery fails closed without Git mutation on any missing, moved, or ambiguous evidence; plain `--recover` still refuses (`--recover --keep-local` is the explicit discard), and keep-local never selects an archive. Owner: `branch-sync-and-push-safety` skill.
- `require-no-mistakes` reads PR facts from a live API lookup and fails closed when it cannot, never from a possibly stale replayed event.
- `axi status` and `axi logs` resolve an implicit run from the caller's current branch only; there is no repo-wide fallback, and every explicit `--run <id>` selection is observation-only (no bare `axi respond` command, since a newer run on that branch could receive it). Owner: `resolveRun` and the status comments in `internal/cli/axi_query.go`.
- `agent.MemoryFilesRule` limits independently initiated memory-file edits, not review of those files or prompted fixes; the document step may only correct factually wrong content, and conflict resolvers use `agent.MemoryFilesConflictRule`. Do not reintroduce exhaustive doc-sweep language into the document prompt. Owner: `documentation-guidance` skill.
- The combined document+lint pass never silently drops the lint duty (any doubt falls back to lint's own pass), uncategorized findings fail safe to the stricter documentation gate, and a configured `commands.lint` stays a deterministic gate.
- `runs.awaiting_agent_since` is observability only and never changes gate resolution.
- `runs.awaiting_agent_since` is set exactly while a gate is parked. Owner: `.agents/skills/pipeline-review-and-agents/SKILL.md`.
- The daemon's effective environment comes from the login-shell probe at startup, never from the service definition's bootstrap `PATH`; only a missing shell binary is waited for, and only at startup. The probe runs in its own session (Setsid); do not fold it into `ConfigureShellCommand` (Setpgid and Setsid cannot be combined). Owner: `daemon-runtime` skill.
- Telemetry: read-only surfaces (`axi` home/status/logs, `status`, `runs`) emit no remote telemetry. Performance detail stays local; prompts, outputs, diffs, and raw command arguments are never stored (`TestAgentInvocations_PrivacySafeShape`), and run IDs, paths, and session identities are never sent remotely; a not-reported metric is stored as NULL, never a fabricated zero. Owner: `reference/environment.md`.
- `no-mistakes update` reads only `channels.json` from the release-asset CDN, never `api.github.com`, and `release.yml` must call the channel publisher after `finalize`. Owner: `release-signing` skill.
- The retired `jev` config key stays a parse-only tombstone; do not repurpose it.

## Test and CI conventions

- Pipeline-step tests put the non-race `internal/pipeline/fakecli` helper on PATH as `gh`/`glab`/`git`, and use `internal/testgit.RealGit` for the real binary. Never re-exec the race-instrumented test binary as those names.
- CI-monitor tests live in `internal/pipeline/steps/citest`; keep both packages under `go test ./...`, not behind the `e2e` tag.
- The rest is owned by the `testing-conventions` skill.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
