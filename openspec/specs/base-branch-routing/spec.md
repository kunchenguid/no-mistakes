# base-branch-routing Specification

## Purpose

Define how no-mistakes resolves the integration branch for a run - the branch used as the rebase target, the review/lint/document diff base, and the pull request base - so that it can be overridden per repository and per run while keeping the trusted-config source anchored to the auto-detected default branch.

## Requirements

### Requirement: Effective integration branch resolution

no-mistakes SHALL resolve a single effective integration branch for each run and use it as the base for the rebase target, the review/lint/document diff range, and the PR target.
Resolution SHALL follow a fixed precedence: the per-run `--base` override, then the repository's persisted `base_branch`, then the auto-detected `default_branch`, then the literal `main`.
The first non-empty value in that order SHALL win.

#### Scenario: No override configured
- **WHEN** a repo has no `base_branch` and a run supplies no `--base`
- **THEN** the effective integration branch SHALL equal the auto-detected `default_branch`
- **AND** behavior SHALL be identical to the pre-change default

#### Scenario: Persisted repo override
- **WHEN** a repo has `base_branch` set to `develop` and a run supplies no `--base`
- **THEN** the effective integration branch SHALL be `develop`

#### Scenario: Per-run override wins
- **WHEN** a repo has `base_branch` set to `develop` and a run supplies `--base release/1.4`
- **THEN** the effective integration branch SHALL be `release/1.4` for that run only
- **AND** the persisted `base_branch` SHALL be unchanged

#### Scenario: Empty fallback
- **WHEN** neither override nor `default_branch` is set
- **THEN** the effective integration branch SHALL be `main`

### Requirement: Persistent per-repo base branch via init

`no-mistakes init` SHALL accept `--base-branch <branch>` and persist it as the repository's `base_branch`.
An idempotent re-init without `--base-branch` SHALL preserve the existing persisted value rather than clearing it.
`init` SHALL verify that `--base-branch` resolves to a branch on origin before persisting it, and SHALL reject a branch origin does not have rather than store an unusable value.

#### Scenario: Set on init
- **WHEN** the user runs `no-mistakes init --base-branch develop`
- **THEN** the repo row SHALL record `base_branch = develop`

#### Scenario: Rejected when absent from origin
- **WHEN** the user runs `no-mistakes init --base-branch <typo>` and origin has no such branch
- **THEN** `init` SHALL fail with an error naming the missing branch
- **AND** no `base_branch` value SHALL be persisted from that invocation

#### Scenario: Preserved on idempotent refresh
- **WHEN** `base_branch` is already `develop` and the user re-runs plain `no-mistakes init`
- **THEN** `base_branch` SHALL remain `develop`

#### Scenario: Changed on explicit re-init
- **WHEN** `base_branch` is `develop` and the user runs `no-mistakes init --base-branch main`
- **THEN** `base_branch` SHALL become `main`

### Requirement: Per-run base override flag

`no-mistakes axi run` SHALL accept `--base <branch>` that overrides the integration branch for that run only.
The flag SHALL be threaded from the CLI through the push notification into the run record so every pipeline step of that run reads the same overridden base.

#### Scenario: Run-scoped diff base
- **WHEN** a run is started with `--base develop`
- **THEN** the review, lint, and document steps SHALL diff against `develop..HEAD`
- **AND** the rebase step SHALL rebase onto `origin/develop`

#### Scenario: Run-scoped PR target
- **WHEN** a run started with `--base develop` reaches the PR step
- **THEN** the PR SHALL be created (or matched) with `develop` as its base branch

### Requirement: Trust root stays anchored to the default branch

The base branch override SHALL move only the diff/rebase/PR target.
It SHALL NOT move the source of trusted repo config: `loadTrustedRepoConfig` SHALL continue to read `commands` and `agent` from the auto-detected `default_branch` at its pinned SHA, never from the overridden base branch.

#### Scenario: Trusted config unaffected by override
- **WHEN** a repo sets `base_branch = develop`
- **THEN** trusted `commands`/`agent` SHALL still be read from the pinned `default_branch` SHA
- **AND** a contributor SHALL NOT be able to alter the trusted config source by changing the base branch
