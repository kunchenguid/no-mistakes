## 1. DB: persist the base branch override

- [x] 1.1 Add `base_branch TEXT NOT NULL DEFAULT ''` to the `repos` schema (`internal/db/schema.go`) as an additive, defaulted column; add/extend a migration test so existing DBs upgrade cleanly.
- [x] 1.2 Add `BaseBranch string` to the `Repo` struct (`internal/db/repo.go`) and thread it through insert/update paths (`InsertRepoWith*`, `UpdateRepoBaseBranch`) and row scans.
- [x] 1.3 Add a unit test that round-trips `base_branch` through insert -> read and through `UpdateRepoBaseBranch`.

## 2. Resolution accessor (TDD)

- [x] 2.1 Write a failing test for the precedence resolver: per-run override -> `BaseBranch` -> `DefaultBranch` -> `main`.
- [x] 2.2 Implement the accessor (`pipeline.ResolveIntegrationBranch` helper + `StepContext.IntegrationBranch()` method) and make the test pass.

## 3. init --base-branch

- [x] 3.1 Add `--base-branch` to `no-mistakes init` (`internal/cli/init.go`) and persist via the gate (`internal/gate/gate.go` `InitWithOptions`), preserving an existing value on idempotent refresh (mirror `fork_url`).
- [x] 3.2 Tests: set on init; preserved on plain re-init; changed on explicit re-init.

## 4. axi run --base (per-run override)

- [x] 4.1 Add `--base` to `newAxiRunCmd` and thread through `runAxiRun`/`triggerRun` (`internal/cli/axi_drive.go`).
- [x] 4.2 Carry the override across the push notification (`internal/cli/daemon_cmd.go` `notify-push` param) into `HandlePushReceived`/`startRun` and persist it on the run; reuse on rerun like `BaseSHA`.
- [x] 4.3 Tests for the flag wiring and run persistence.

## 5. Route consumers through the resolved base

- [x] 5.1 `internal/pipeline/steps/common_git.go` `resolveBranchBaseSHA`: callers now pass the resolved integration branch.
- [x] 5.2 `rebase.go`: fetch + rebase onto the resolved base (not hardcoded default).
- [x] 5.3 `review.go`, `lint.go`, `document.go` (+ `test.go`, `intent.go`, `ci.go`, `ci_fix.go`, `common_fix.go`): diff against the resolved base.
- [x] 5.4 `pr.go`: PR base target and PR-skip check use the resolved base.
- [x] 5.5 Re-verify force-push lease and bundled-local-default-commit protections against a non-`main` base; added `TestRebaseStep_DetectsUnpushedLocalBaseBranchCommitsForOverride`.

## 6. Trust boundary guard

- [x] 6.1 Confirm/keep `loadTrustedRepoConfig` pinned to `default_branch`, never the override; added `TestTrustRootIgnoresBaseBranchOverride` asserting the override does not move the trusted-config source.

## 7. End-to-end validation

- [x] 7.1 E2E reproducing the original bug (`TestBaseBranchOverrideScopesReviewToIntegrationBranch`): a branch one commit ahead of `develop` but many behind `main`, gated with `init --base-branch develop`, reviews against the develop tip (not the 144-commit main range).
- [x] 7.2 `gofmt -w .`, `make lint`, `go test -race ./...`, and `make e2e` all pass (run outside any gated pipeline run).

## 8. Docs

- [x] 8.1 Documented `init --base-branch` and `axi run --base` in `docs/reference/cli.md` plus a GitFlow note in `docs/concepts/gate-model.md`.
- [x] 8.2 Noted in `AGENTS.md` trust section (and a new Base Branch Override section) that `base_branch` moves only the diff/rebase/PR target, not the trust root.
- [x] 8.3 Skill kept frozen: the skill body defers to `axi run --help` for flags (which auto-lists `--base`); the `--help` Long text and driving guidance are unchanged, so no mirroring needed.
