## ADDED Requirements

### Requirement: Detached fork remotes

This checkout SHALL document `origin` as the fork that receives product work and `upstream` as `kunchenguid/no-mistakes`. Local changes stay on the fork.

#### Scenario: Remotes named in docs
- **WHEN** an operator reads the detached-fork guide
- **THEN** it names `origin` as this fork and `upstream` as `git@github.com:kunchenguid/no-mistakes.git`

### Requirement: Pull requests target the fork only

Pull requests for this checkout SHALL target `origin` (the fork) and MUST NOT be opened against `upstream`.

#### Scenario: PR destination
- **WHEN** the detached-fork guide describes how to propose a change
- **THEN** it instructs opening the pull request against the fork default branch and forbids opening one against `kunchenguid/no-mistakes`

### Requirement: Sync from upstream

The guide SHALL describe fetching `upstream` and updating the fork default branch so product work can keep pulling upstream commits.

#### Scenario: Sync steps exist
- **WHEN** an operator wants new upstream commits
- **THEN** the guide lists fetch-upstream and update-fork-default-branch steps that do not open an upstream pull request
