# Benchmark: live TypeSafe API and Jev review pre-brief (issue #1125)

> **Retired.** The Jev review pre-brief (`jev.review_assist`) and its
> `jev.candidate_excerpt_bytes` excerpt option were removed on 2026-09-22
> based on the evidence collected here and in the later candidate-excerpt
> trial: the candidate generator excludes changed files by construction while
> nearly all recorded review-finding locations are changed files, so the
> listing could not reach what it ranks for, and excerpts measured less
> precise than path-only at more billed input tokens. This document is kept
> as a historical record of what was measured; it does not describe current
> behaviour, and the `jevbench` harness it references is no longer in the
> tree.

Follow-up to merged PR https://github.com/kunchenguid/no-mistakes/pull/1120 (issue #1055).
That delivery published an off/on method under `benchmarks/issue-1055/`, but the implementing environment could not complete a real TypeSafe run because it lacked `TYPESAFE_API_KEY`.
This directory is the operator-credentialed live re-run: real TypeSafe System One (`jev-1.13.0`) plus an honest off/on billed-token and wall-time comparison of cold reviews.

## Question

Does a live TypeSafe evaluation succeed with the production client, and does the opt-in `jev.review_assist` pre-brief change billed tokens or wall time of a complete, cold, session-free review launch?

## Live TypeSafe scenario

Two gated tests, skipped unless `NO_MISTAKES_JEV_LIVE=1` and `TYPESAFE_API_KEY` are set, so `go test ./...` never hits the network.

- `TestEvaluate_LiveTypeSafeAPI` (`internal/jev`) posts one batched score question to `https://api.typesafe.ai/v1/systemone` with the pinned model.
- `TestReviewStep_LiveTypeSafePrebrief` (`internal/pipeline/steps`) drives the production `ReviewStep` pre-brief path (real client, no fake) on the `setupJevRepo` fixture, with a mock reviewer so the measurement is Jev billed tokens and latency, not an agent review.

The key was loaded into the process environment from the operator's gitignored home `.env` and was never printed, logged, or written under this tree.
Evidence JSON in this directory records model, token counts, and wall time only.

## Off/on review launches

Harness: `jevbench` (`benchmarks/issue-1055/jevbench`), run from the repository root.
Each launch drives the production `ReviewStep.Execute` against the real Pi CLI and, for on-mode launches, the live TypeSafe API (`jev-1.13.0`).
No daemon and no executor wrap the launch.
Every launch gets a fresh detached git worktree and a fresh agent invocation with `RunSessions` nil.

Corpus: one real change on this repository.

- `jev-prebrief`: `7e2c5fad..9b697a7b` - merged PR #1120 (`feat(pipeline): add opt-in TypeSafe Jev context pre-brief to review turns`), 18 files, +2691/-5.
  The #1055 JSONL named `4b79b6ef..5082cdc1`, which is not in this clone (a pre-merge local tip); the merged commit is the change that actually landed.

Arms: `jev.review_assist` off vs on, nothing else changed.
Target: 2 completed launches per arm, alternating off/on.
Agent: Pi, effort `xhigh`.
Model: `openai-codex/gpt-6-astra` until that account's usage limit refused the first off-launch (recorded with its error), then `kimi-coding/kimi-for-coding` for the completed sample.
Every row records its own `reported_model`.
Failed launches do not count toward the per-arm total.

Recorded per launch (see `launches-jev-prebrief.jsonl`): wall time, attempt count, Pi-reported input/output/cache-read tokens, findings count, reviewed_paths count, risk level, and the production pre-brief log line for on-mode launches.
Token counters sum every agent attempt of a launch; `usage_reported` is false when any attempt reported no usage.
The TypeSafe key came from the environment and was never printed or persisted; Jev usage is the API-reported `input_tokens` of the one batched request per on-launch.

## Honest limits

- 2 launches per arm is a small sample; treat deltas as indicative, not proven.
- The completed arms share one model (Kimi) after the Codex usage-limit refusal; they are not compared against the #1055 gpt-6-astra rows.
- Both on-launches listed 0 of 40 candidates, so the review prompt had no pre-brief section.
  Off/on review-token deltas on this corpus are agent noise, not a reading-list effect.
  The small widget fixture in the live pre-brief test did list 1 of 5, which is why listing is not called inert.
- One change on one repository is not a corpus.
- Quality comparison is findings-count and reviewed_paths parity, not labeled recall.
