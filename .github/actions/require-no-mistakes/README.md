# `require-no-mistakes`

Composite action that checks whether a pull request body declares a completed,
head-bound no-mistakes pipeline run. It is the reusable shared implementation
of the check named **`PR must be raised via no-mistakes`**; enforcing
repositories can call it instead of copying the shell into their own workflow.

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
| `compliant` | `true` only when the PR satisfied the pipeline gate. It remains `false` for an exemption because bypass is not validation. |
| `exempt` | `true` when a configured exemption bypassed the gate. |
| `exempt-reason` | Why the PR was exempt; empty when it was judged. |

## Boundary

The action never checks out or executes repository code, so it is safe on
`pull_request` runs from forks. Callers should keep `permissions: contents: read`
and stay on `pull_request` rather than `pull_request_target`.

An exemption is trusted outer-repository policy supplied by the caller's pinned
workflow. It does not claim that no-mistakes ran: exempt PRs report
`compliant=false` and `exempt=true`. This is separate from the invariant that no
standing configuration may skip a step inside a no-mistakes run.

### Non-goal: a contributor guardrail, not a forgery-proof boundary

This gate is a **contributor guardrail**. It is explicitly **not** a
forgery-proof security boundary, and it is not trying to become one.

The attestation is a deterministic, commit-bound declaration published in the
PR body, not a cryptographic signature. A pull request author can edit their own
body and reproduce the documented format by hand, and such a PR passes this
check. That is a **known and accepted limitation**, and a **pre-existing** one:
it is inherited verbatim from the inline gate this action extracts, so
consolidating the fleet onto one implementation neither introduces nor widens
it. The action emits a warning on every structural pass to keep the boundary
visible in the required check's logs.

What it does reliably catch is the case it exists for: a contributor who
bypassed the pipeline by accident, a malformed or incomplete declaration, and an
attestation left stale by a later push. It authorizes nothing against an author
who forges the format on purpose.

Authenticated (signed) attestations are the robust fix. They are tracked
separately as backlog item `nm-signed-attestations-r1` and are deliberately out
of scope for this action.

## Rollout

This repository's own gate (`.github/workflows/no-mistakes-required.yml`) is a
thin caller of this action, pinned to the commit that first published it. GitHub
downloads `uses:` actions at job setup, so the pin must always name a ref that
already carries the action; a caller pinned to a tag that predates it fails
closed on every pull request.

Pinning the gate to an already-published commit is the self-certification guard.
A pull request that edits this action is fully **tested** on its own head - the
repository's Go tests execute `verify.py` from the working tree - while the
required check judging that pull request keeps running the published pinned copy. The
gate is therefore never rewritten by the change it is judging. Bumping the pin
is a deliberate, separate pull request.

Migrating the other enforcing repositories follows the same rule: pin a released
tag or a commit SHA, never `@main`.

## Behavior is pinned by tests

`require_no_mistakes_action_test.go` in the repository root executes
`verify.py` the way a runner does and covers every verdict, the exemption
surface, and the event-payload fallback.
