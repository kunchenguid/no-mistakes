---
title: Evaluation toolkit
---

`no-mistakes eval` is a **local-only** toolkit for comparing review candidates against review passes your own pipeline has already recorded.

The corpus collects itself: eligible finished runs' decided review passes become cases, so the sets are populated by the time you want to compare something. Replay and reporting stay explicit commands you run.

The `eval` commands do not start or use the shared daemon, alter a gate, emit remote telemetry, push a branch, open a PR, or run CI. Cases, source findings, decisions, candidate outputs, and metrics are stored only under `<NM_HOME>/eval/`; there is no export, sharing, synchronization, or remote case store.

Replay does invoke the selected agent normally, so that agent may send the restored code and review context to its configured model provider. The local-only guarantee concerns eval storage and transport added by no-mistakes, not the selected agent's ordinary provider traffic.

## How cases are collected

Cases arrive on their own. When an eligible run finishes, its decided Review passes are frozen into the local corpus - one case per pass. Collection happens after the pipeline has already reported its outcome, so it can never change or fail the run; a problem is logged and nothing else.

Two settings in `config.yaml` govern it, both on by default and both documented in [Global configuration](/no-mistakes/reference/global-config/#eval):

- `eval.capture_provenance` records the exact commit and configuration inputs a replay needs. It is written when the review round is written and **cannot be added afterwards**, so a run reviewed with it off is never capturable - not by the automatic path and not by hand.
- `eval.auto_capture` performs the collection. Turning it off leaves provenance recorded, so runs stay capturable by hand.

You can also capture a specific run yourself:

```sh
no-mistakes eval capture <run-id>
```

A confirmed post-PR miss - review passed green, and a later human-vetted finding showed a real defect - is ingested as false-negative gold through the same local corpus:

```sh
no-mistakes eval miss ingest <run-id> \
  --finding '{"id":"stable-id","file":"path.go","line":12,"severity":"error","description":"one-sentence defect"}'
```

`--finding` is repeatable. The command captures the run if needed (recapture is a no-op, so existing labels survive), then writes false-negative gold onto the last completed non-blocking review pass. Duplicate finding IDs are no-ops. A parked or blocking review is refused: that class found something, so it is not a post-PR miss.

The ingest payload is the source of truth. Eval does not scrape GitHub review comments and does not read an external markdown ledger. The curator (a human, or an automation that already vetted the miss) supplies the structured finding.

Automatic collection and `eval capture` do the same freeze, so a case is equally trustworthy either way. Capturing a run that was already collected relabels gold from later merge evidence and otherwise leaves the frozen case in place. `eval miss ingest` can still attach confirmed post-PR-miss gold afterwards.

A run is skipped when there is nothing honest to freeze: no Review step, no finished pass, a gate decision the human has not made yet, or rounds recorded before provenance was on. An incomplete later review round (no recorded findings) is skipped so a completed sibling of the same run can still be captured. Capturing a run with nothing capturable reports the reason instead of freezing an incomplete label; for a parked Review, retry after the decision is recorded.

A case includes:

- the reviewed commit, base, and trusted-config commit pinned at capture
- agent-neutral global configuration and the effective repository configuration frozen at capture
- the original run, step, review-round, decision, and local invocation-metric records
- a manifest with commit pins, changed-file counts, build identity, and a hash of the redacted remote URL
- a local `labels.json` file that stores finding-level gold and queued unmatched candidate findings

The manifest never stores a remote URL. Capture is read-only against the existing local database and gate. It does not fetch from the network.

## Finding-level gold

The unit of truth is whether a review **finding** was a real issue, scored with scientific terms, not whether the run parked or passed.

Capture writes gold from recorded gate evidence: human Fix and add-finding decisions, plus the merge-derived auto-fix and shipped-unfixed rules below. A merged PR is not a case-level pass or fail:

- A finding the human selected for Fix (`selected_finding_ids` with a user source) is **true-positive** gold: that finding is a true issue. Merge is not required.
- A finding the human added (`user_findings_json`, source `user`) is **false-negative** gold: the original review missed a real issue.
- A finding the pipeline auto-fixed that later **landed in a merged PR** is **true-positive** gold (`recorded-auto-fix-merged`). Closed-not-merged, still-open, reverted, and superseded auto-fixes stay unlabeled.
- A finding that was raised (`auto-fix` or `ask-user`, including a missing action that defaults to `ask-user`) and then **shipped unfixed in a merged PR** is **false-positive** gold (`recorded-shipped-unfixed`). If a later review round exists and the last of those rounds no longer raises the same issue, earlier rounds stay unlabeled - an intermediate re-raise that was gone before merge is a fix, not a false positive. Informational `no-op` findings are not labeled this way.
- A confirmed post-PR miss ingested with `eval miss ingest` is also **false-negative** gold (`recorded-post-pr-miss`): review passed green, and a later vetted finding showed a real defect.
- Skip, and approve-with-findings on an unmerged PR, stay **unlabeled / pending** until later adjudication.
- A later replay that raises a new issue absent from the gold set is queued as an unmatched candidate finding. It is never auto-scored as a false positive.

If a PR merges after the first capture, already-captured cases are relabeled. The daemon does this best-effort when it observes the merge; `eval relabel [run-id]` or recapture is the CLI path. Relabel adds merge-derived labels onto previously unlabeled findings and drops obsolete derived merge labels that the current rounds no longer support. Adjudicated, user-fix, and ingested post-PR-miss labels are never overwritten.

A case with no finding-level gold is unlabeled / pending, never a pass. True-negative also stays unlabeled because the current capture evidence cannot establish that a finding is invalid without the shipped-unfixed or adjudication paths above.

## Disk use and retention

Cases from the same repository share one local Git object pool under `<NM_HOME>/eval/pools/`. The first case from a repository stores its history once; every later case adds only the objects its own commits introduced, which is normally a few kilobytes.

`eval.max_cases` (default 200) is the retention target enforced after automatic collection. When it is exceeded the oldest unprotected cases are dropped first. A case that has a replay in progress or already has recorded candidate replays is never dropped - an eval report's cohort pins the case IDs it compared, so reclaiming one would invalidate a comparison you already paid for. Protected cases can therefore keep the corpus above the target. Set it to `0` to keep every case.

Because the objects live in the pool rather than inside each case, a case directory is not a portable archive: copying it elsewhere does not carry the code it replays.

Finding-level gold uses `labels.json` schema version 2. There is no migration from labels that store a park/pass verdict, and manifest version 1 cases are also incompatible. If an eval command reports an unsupported case or labels version, remove `<NM_HOME>/eval/` to start a fresh corpus; automatic collection will refill it from later runs.

## Inspect case sets before spending tokens

```sh
no-mistakes eval sets
```

The command shows counts, finding-level gold coverage, unlabeled / pending cases, queued candidate findings, and composition by repository fingerprint, dominant language, change-size bucket, source severity, and finding type.

Four logical sets are available to replay:

- `all` - every captured review pass
- `labeled` - only cases with at least one finding-level gold label
- `diversified` - the official gold-only holdout: a pinned, size-capped stratified sample of labeled cases (repository, language, size, severity, finding-type). Empty gold produces an empty set and a warning, never a silent unlabeled fill. Rebuild pins with `eval sets --refresh-diversified`.
- `tune` - leftover labeled cases after the diversified pins. Iterate matcher thresholds and prompt experiments here, never on `diversified`.

`eval.diversified_size` (default 32, documented in [Global configuration](/no-mistakes/reference/global-config/#eval)) caps the official set. `0` keeps one gold case per stratum. Pins stay until a case is pruned, loses its gold, or an explicit refresh. Lowering the cap takes effect on the next `eval sets` / `ListCases` read: oldest pins are trimmed to the live cap, at most one per stratum, without waiting for `--refresh-diversified`. Seats freed by collapsing duplicates fill new strata at most one case each.

Do not fit matcher thresholds or review product prompts against `diversified`. That set is the held-out official measurement; `tune` is the only labeled leftover it is safe to iterate on.

## Replay a candidate

```sh
no-mistakes eval run \
  --cases diversified \
  --candidate codex+gpt-5.4 \
  --repeats 3
```

A candidate is always explicit: `agent+model`. The replay restores each case into a fresh temporary bare gate and worktree, then invokes only the existing Review step. Push, PR, CI, test, lint, document, and fix loops are outside this subject under test.

Replay scores each candidate finding against that gold:

- **true-positive**: the candidate raises the same underlying issue as a true-issue gold finding (user Fix, auto-fix-merged, human-added miss, or a confirmed post-PR miss)
- **false-negative**: the candidate misses a true-issue gold finding
- **false-positive**: only when a candidate finding matches explicit false-positive gold (adjudicated invalid, or shipped-unfixed). Unmatched candidate findings are never treated as false positives
- **pending / unlabeled**: unmatched candidate findings, and cases with no finding-level gold yet

Matching is a documented cascade of strengths: the same finding ID, the same file and description after whitespace and case normalization, the same file with lines within 3 and token-Jaccard ≥ 0.5, then gated containment (same file, one normalized description contains the other, shorter side ≥ 8 tokens). Assignment is maximum matching per strength tier, preferring exact over fuzzy, so gold-label order cannot undercount. Headline recall uses the full cascade. Reports also show recall-if-exact-only so a fuzzy-threshold change is visible. File-less or description-less findings do not match on the text, location, or containment strengths.

The report prints recall, precision bounds (adjudicated vs pending-as-FP), and F1 as the headline metric **only when false-positive gold exists** so precision is real. Otherwise F1 is withheld rather than reported as recall-in-disguise.

`--repeats` defaults to `3` and must be at least `1`. Candidates must use an agent that can enforce an explicit model; ACP targets such as `cursor` and `acp:<target>` are rejected. Replays are intentionally isolated from the production `NM_HOME`; they do not contact the shared no-mistakes daemon. The selected agent still communicates with its configured model provider in the normal way.

## Report results

```sh
no-mistakes eval report
```

The report groups local replays by candidate and cohort. A cohort pins the selected case IDs and repeat count, so frontier comparisons only compare candidates run over the same corpus and repeat plan. It shows:

- finding-level true-positive, false-negative, false-positive, and pending counts
- recall over gold issues, or unlabeled / pending when a case has no finding-level gold
- precision bounds, and F1 only when false-positive gold exists
- queued unmatched candidate findings, which are not scored as false positives
- failed candidate invocations
- reported fresh-input plus output token cost
- average wall time
- a finite-sample case-level recall range, with repeats averaged inside each case
- whether a candidate lies on the observed recall-versus-token-cost frontier

The report is deliberately cautious. It never treats an unadjudicated candidate finding as a false positive, excludes candidates with failed replays from the frontier, and distinguishes missing token instrumentation from a real zero.

## Current boundary

Finding-level gold is derived from recorded Fix, add-finding, auto-fix-merged, and shipped-unfixed evidence, plus confirmed post-PR misses ingested through `eval miss ingest`. An adjudication CLI, PR-comment miss scanning, sharing, sync, and full-pipeline replay are not part of this command surface. A live merge, `eval relabel`, or recapture backfills merge-derived labels onto already captured cases.
