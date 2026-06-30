## Context

`Repo.DefaultBranch` is auto-detected from `origin/HEAD` at `init` (`internal/git/git.go` `DefaultBranch`, persisted in `repos.default_branch`, default `main`).
It is consumed as the integration base in five places: rebase target (`rebase.go`), review/lint/document diff base (via `resolveBranchBaseSHA` in `common_git.go`), and the PR base target plus PR-skip check (`pr.go`).
There is no override, so a GitFlow repo whose integration branch is `develop` cannot gate against it without flipping the repo-wide GitHub default.

## Goals / Non-Goals

**Goals:**
- Let a repo gate/rebase/PR against a non-default branch.
- A persistent per-repo setting (`init --base-branch`) and a per-run override (`axi run --base`).
- One resolution path so every consumer agrees on the base.
- Zero behavior change for repos that set no override.
- Keep the config trust root on `default_branch`.

**Non-Goals:**
- Changing how the default branch is auto-detected.
- GitLab/Bitbucket-specific MR/PR base wiring beyond what already mirrors the GitHub path.
- A committed `.no-mistakes.yaml` `base_branch` field (rejected below).

## Decisions

**Storage: DB column, not committed config.**
Add `base_branch TEXT NOT NULL DEFAULT ''` to `repos`, with a `Repo.BaseBranch` field, set via `no-mistakes init --base-branch` and preserved on idempotent refresh exactly like `fork_url`.
A committed `.no-mistakes.yaml` field was considered for team-sharing but rejected for this change: the base branch controls the review diff surface, so a committed field would have to be loaded from the trusted default-branch copy (to stop a pushed branch shrinking its own review), which couples it into the `EffectiveRepoConfig`/`loadTrustedRepoConfig` trust machinery. The DB-column path is local, mirrors the established `fork_url` pattern, and needs no trust-boundary changes.

**Resolution: one accessor, fixed precedence.**
Effective base = first non-empty of: per-run override, `Repo.BaseBranch`, `Repo.DefaultBranch`, `"main"`.
Steps have both the run and the repo in `sctx`, so the accessor lives where both are reachable (a method on the step context, or a helper taking `(run, repo)`). The per-run override is carried on the run record so it survives the push -> daemon -> step hops; the existing `Run.BaseSHA` is a commit SHA and is not repurposed.

**Per-run plumbing.**
`axi run --base` -> `triggerRun` -> `daemon notify-push` param -> `HandlePushReceived`/`startRun` -> persisted on the run row -> read by steps. On a rerun, the override is reused like `BaseSHA` is.

**Trust root unchanged.**
`loadTrustedRepoConfig` keeps pinning `commands`/`agent` to `default_branch`. The override never feeds trusted-config selection. This separates "where trust comes from" (the protected GitHub default) from "what we merge into" (the integration branch), which is the exact GitFlow split.

## Risks / Trade-offs

- **DB migration**: adding a column to `repos` must be additive with a default so existing rows and the schema-version path keep working; covered by a migration test.
- **Per-developer setup**: the DB-column choice means each clone must run `init --base-branch` (not auto-shared via the repo). Accepted: it matches `fork_url`, and `axi run --base` covers ad-hoc needs. A committed-config follow-up remains possible later.
- **PR-skip semantics**: the "skip PR if pushed branch == default" check moves to "== effective base". Pushing the integration branch itself (e.g. `develop`) through the gate correctly skips PR; an unusual push of `main` when base is `develop` would attempt a `main -> develop` PR. Low-likelihood, acceptable.
- **Rebase onto a non-default base**: rebasing onto `origin/develop` is the intended behavior; the existing force-push lease and bundled-local-default-commit protections still apply and must be re-verified against a non-`main` base.
