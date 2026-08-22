# `require-no-mistakes`

Composite action that enforces "this pull request was raised through the
no-mistakes pipeline". It is the reusable shared implementation of the check
named **`PR must be raised via no-mistakes`**; enforcing repositories can call it
instead of copying the shell into their own workflow.

It verifies, in order:

1. the PR body carries the no-mistakes signature line;
2. the body carries a parseable `<!-- no-mistakes-pipeline-attestation:v1 {...} -->`
   comment;
3. the attestation's `head_sha` equals the PR head SHA, so a later push cannot
   pass on an older attestation;
4. `review`, `test`, and `document` each recorded `status == "completed"`.
   Quota skips and agent skips are not compliant.

Missing or unparseable attestation reports the no-mistakes `>= 1.46.0` floor;
a missing signature reports the not-raised-via-no-mistakes guidance.

## Usage

Consumers pin a release tag or a commit SHA. Never `@main`: `main` is editable
by the very PR the gate is judging.

```yaml
name: Require no-mistakes
on:
  pull_request:
    types: [opened, edited, reopened]
    branches: [main]

permissions:
  contents: read

jobs:
  check:
    name: PR must be raised via no-mistakes
    runs-on: ubuntu-latest
    steps:
      - uses: kunchenguid/no-mistakes/.github/actions/require-no-mistakes@<release-tag-or-sha>
        with:
          exempt-authors: |
            github-actions[bot]
            dependabot[bot]
```

Replace `<release-tag-or-sha>` with a no-mistakes release tag or commit SHA
that contains this action.

The job name must stay exactly `PR must be raised via no-mistakes` so branch
rulesets keep matching the same check across the fleet.

An ordinary `pull_request`-triggered caller forwards no PR facts: the action
reads the body, head SHA, head branch, author, and number from the workflow
event payload. Pass the `pr-*` inputs only when driving it from another event.

## Inputs

| Input | Default | Purpose |
| --- | --- | --- |
| `exempt-authors` | `""` | Newline- or comma-separated author logins that bypass the gate (automation accounts that cannot be routed through the pipeline). |
| `exempt-bot-authors` | `false` | When true, every `*[bot]` author bypasses the gate. |
| `exempt-head-branches` | `""` | Glob patterns; a matching head branch bypasses the gate, for structural automation branches such as `release-please--*`. |
| `pr-body`, `pr-head-sha`, `pr-head-ref`, `pr-author`, `pr-number` | `""` | Override the corresponding event-payload fact. |

Which steps are required is deliberately **not** an input. A caller configures
who is exempt, never what the gate certifies, so no repository can weaken the
check while still reporting the same name.

## Outputs

| Output | Meaning |
| --- | --- |
| `compliant` | `true` when the PR satisfied the gate or was exempt. |
| `exempt` | `true` when a configured exemption bypassed the gate. |
| `exempt-reason` | Why the PR was exempt; empty when it was judged. |

## Boundary

The action never checks out or executes repository code, so it is safe on
`pull_request` runs from forks. Callers should keep `permissions: contents: read`
and stay on `pull_request` rather than `pull_request_target`.

## Rollout

This repository's own gate (`.github/workflows/no-mistakes-required.yml`) still
runs the inline enforcement it was extracted from. GitHub downloads `uses:`
actions at job setup, so a caller pinned to a tag that predates this action
fails closed on every pull request. The flip waits for the first release that
contains the action and then pins exactly that tag, which is what stops a pull
request that edits the action from certifying itself. Migrating the other
enforcing repositories follows the same rule: pin a released tag or SHA.

## Behavior is pinned by tests

`require_no_mistakes_action_test.go` in the repository root executes
`verify.py` the way a runner does and covers every verdict, the exemption
surface, and the event-payload fallback.
