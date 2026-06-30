## Why

no-mistakes hardwires its review diff base, rebase target, and PR target to the repository's auto-detected default branch (`origin/HEAD`, almost always `main`).
In a GitFlow layout where `main` is the released branch and `develop` is the integration branch, this is wrong: the gate diffs `main..HEAD` and pulls in the entire "develop is ahead of main" backlog (observed: 144 commits / ~9700 insertions for a single-commit change), and the PR opens against `main` instead of `develop`.
There is no flag and no config field to retarget the base, and flipping the repo's default branch is a repo-wide change that affects everyone.

## What Changes

- Add a persistent per-repo **base branch override** stored in the daemon DB (`repos.base_branch`), set via `no-mistakes init --base-branch <branch>` and preserved on idempotent re-init (mirrors `fork_url`).
- Add a per-run override flag `no-mistakes axi run --base <branch>` that wins for that single run.
- Resolve the effective integration branch through one accessor with precedence: per-run `--base` -> repo `base_branch` -> auto-detected `default_branch` -> `main`.
- Route the integration branch through the five existing consumers that today read `default_branch`: rebase target, review diff base, lint diff base, document diff base, and PR base target / PR-skip check.
- Keep `default_branch` (the GitHub-designated default, `origin/HEAD`) as the **trust root**: `loadTrustedRepoConfig` continues to pin trusted `commands`/`agent` to `default_branch`, never to the overridden base. The override only moves the diff/rebase/PR target, never the source of trusted config.

## Capabilities

### New Capabilities
- `base-branch-routing`: Selecting which branch no-mistakes treats as the integration base for review/rebase/PR, independent of the repository's GitHub default branch, with a persistent per-repo setting, a per-run override, and a fixed precedence order; and the invariant that the config trust root stays anchored to the default branch.

### Modified Capabilities
<!-- None: no existing specs in openspec/specs/. -->

## Impact

- `internal/db`: new `base_branch` column on `repos` (schema + struct + insert/update paths).
- `internal/cli`: `init --base-branch` flag and persistence; `axi run --base` flag threaded through trigger -> push notify -> run start.
- `internal/daemon/manager.go`: thread the per-run base override into `startRun`; persist on the run.
- `internal/pipeline/steps`: `rebase.go`, `review.go`, `lint.go`, `document.go`, `pr.go`, and the shared `resolveBranchBaseSHA` in `common_git.go` switch from `Repo.DefaultBranch` to the resolved integration branch.
- `internal/gate/gate.go`: `init` persistence preserves an existing `base_branch` on refresh.
- Docs: `docs/` reference for the `init --base-branch` flag and `axi run --base`, plus a GitFlow guide note. Trust-boundary docs (AGENTS.md) note that `base_branch` does not move the trust root.
- No change to the auto-detection default: repos that never set an override behave exactly as today.
