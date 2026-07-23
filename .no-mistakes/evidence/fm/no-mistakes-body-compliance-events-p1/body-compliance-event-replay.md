# PR body compliance event replay

This evidence replays the incident-shaped sequence from the focused regression:
one signed `opened` event, one unsigned `edited` event, and one signed `edited`
event, all for PR #549 at the same head SHA. The shell body was extracted
directly from the workflow's `Verify no-mistakes signature in PR body` step and
executed once per event.

| Run ID | Run number | Action | Head SHA | Concurrency group | Status | Conclusion |
| ---: | ---: | --- | --- | --- | --- | --- |
| `29962844999` | 586 | opened | `same-head` | `no-mistakes-required-549-29962844999` | completed | success |
| `29962943078` | 587 | edited | `same-head` | `no-mistakes-required-549-29962943078` | completed | failure |
| `29965243268` | 588 | edited | `same-head` | `no-mistakes-required-549-29965243268` | completed | success |

## Reviewer-visible run titles

```text
PR #549 body compliance - opened - event 586 (run 29962844999)
PR #549 body compliance - edited - event 587 (run 29962943078)
PR #549 body compliance - edited - event 588 (run 29965243268)
```

The increasing run numbers provide event ordering, while the run IDs remain the
immutable identity surfaced in both the run title and body-event concurrency
group.

## Actual compliance-step output

### Signed opened event

```text
Found no-mistakes signature in PR #549 body.
```

Exit status: `0` (`success`)

### Unsigned edited event

```text
::error::This PR was not raised through no-mistakes.

Contributions to this repository must be submitted via 'git push no-mistakes'.
That pipeline runs the required review/test/lint/CI steps and writes a
deterministic '## Pipeline' section into the PR body containing:

    Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)

See CONTRIBUTING.md for setup and the full workflow.

PR author: first-time-fork-contributor
```

Exit status: `1` (`failure`)

### Signed edited event

```text
Found no-mistakes signature in PR #549 body.
```

Exit status: `0` (`success`)

## Focused automated contract checks

The targeted Go tests also exercised:

- GitHub's one-running/one-pending concurrency replacement behavior against all
  three same-head body events
- unique concurrency groups for `opened` and `edited`
- preserved coalescing for `synchronize` and `reopened`
- the stable check name and event identity in the run title
- the `pull_request` boundary, `contents: read`, and absence of secrets,
  checkout, and write permissions
- the unchanged signature marker and shell-injection-safe PR-body transport
