---
title: Evaluation toolkit
---

`no-mistakes eval` is an opt-in, **local-only** toolkit for comparing review candidates against review passes your own pipeline has already recorded.

It is separate from normal pipeline operation. It does not start or use the shared daemon, alter a gate, add configuration, emit remote telemetry, push a branch, open a PR, or run CI. Case bundles, source findings, decisions, candidate outputs, and metrics stay under `<NM_HOME>/eval/`.

## Capture a run

```sh
no-mistakes eval capture <run-id>
```

Capture exports one case for each persisted Review pass in the run. A case includes:

- a self-contained Git bundle for the reviewed commit, base, and pinned trusted-config commit
- agent-neutral global configuration and the effective repository configuration
- the original run, step, review-round, decision, and local invocation-metric records
- a manifest with commit pins, changed-file counts, build identity, and a hash of the redacted remote URL
- a local `labels.json` file that can grow in later evaluation phases

The manifest never stores a remote URL. Capture is read-only against the existing local database and gate. It does not fetch from the network.

## Inspect case sets before spending tokens

```sh
no-mistakes eval sets
```

The command shows counts, verdict-label coverage, queued candidate findings, and composition by repository fingerprint, dominant language, change-size bucket, and source severity.

Three logical sets are available to replay:

- `all` - every captured review pass
- `labeled` - only cases with a verdict label derived from a recorded human gate decision
- `diversified` - a deterministic representative, retaining one earliest case per repository, language, size, and expected-verdict bucket

## Replay a candidate

```sh
no-mistakes eval run \
  --cases diversified \
  --candidate codex+gpt-5.4 \
  --repeats 3
```

A candidate is always explicit: `agent+model`. The replay restores each case into a fresh temporary bare gate and worktree, then invokes only the existing Review step. Push, PR, CI, test, lint, document, and fix loops are outside this MVP.

The captured human gate decision supplies the verdict policy:

- a recorded user-selected fix means the candidate should park
- a recorded human approval or skip means the candidate should pass
- incomplete or ambiguous historical decisions are left unlabeled and excluded from verdict scoring

If a candidate parks on a human-pass case, the finding is queued locally for later adjudication. It is not automatically called wrong. This protects potentially good, unexpected findings until finding-level labeling exists.

`--repeats` defaults to `3` and must be at least `1`. Replays are intentionally isolated from the production `NM_HOME`; they do not contact the shared no-mistakes daemon. The selected agent still communicates with its configured model provider in the normal way.

## Report results

```sh
no-mistakes eval report
```

The report groups all local replays by candidate and shows:

- confirmed verdict agreement and its conservative lower bound
- queued unexpected parks and failed candidate invocations
- reported fresh-input plus output token cost
- average wall time
- a 95% paired bootstrap confidence interval over cases, with repeats averaged inside each case
- whether a candidate lies on the observed accuracy-versus-token-cost frontier

The report is deliberately cautious. It never treats an unadjudicated candidate finding as a false positive, and it distinguishes missing token instrumentation from a real zero.

## MVP boundary

The MVP measures verdict-level agreement only. Finding-level valid/invalid labels, an adjudication CLI, matching candidate findings to labels, PR-comment miss scanning, precision/recall/F1, holdouts, sharing, sync, and full-pipeline replay are not part of this command surface.
