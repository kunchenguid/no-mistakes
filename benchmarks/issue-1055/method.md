# Benchmark: Jev review pre-brief (issue #1055)

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

Ordered by the captain on 2026-09-19: real, end-to-end, off-vs-on data for the
opt-in `jev.review_assist` pre-brief, on the PR for maintainer validation.

## Question

Does the Jev pre-brief make a complete, cold, session-free review launch
cheaper or faster, without changing what the review covers or finds?

## Method

Harness: `jevbench` (this directory), run from the repository root.
Each launch drives the PRODUCTION `ReviewStep.Execute` against the real Pi CLI
and, for on-mode launches, the live TypeSafe API (`jev-1.13.0`).
No daemon and no executor wrap the launch; they do not affect what is measured
(the billed tokens and wall time of one complete review turn).
Every launch gets a fresh detached git worktree and a fresh agent invocation
with `RunSessions` nil, which is exactly the cold, session-free review the
pipeline runs; the reviewed_paths coverage obligation is identical in both
modes.

Corpus: two real changes on this repository.

- `jev-prebrief`: `4b79b6ef..5082cdc1` - this PR's own feature commit,
  11 files, +1578/-4 (new package, config wiring, step integration, tests,
  docs).
- `pi-profile-pin`: `c11dbf8a..71cd9110` - "feat(daemon): pin Pi model and
  reasoning effort per run (#1072)", 31 files, +1877/-57.

Arms: `jev.review_assist` off vs on, nothing else changed.
3 launches per arm for `jev-prebrief`, 4 per arm for `pi-profile-pin`,
alternating off/on so provider-side prompt caching and machine load spread
across both arms.
Agent: Pi, effort `xhigh` - the setup the issue measured.
Model: `openai-codex-work/gpt-6-astra` until that account's usage limit was
reached mid-benchmark (one failed off-launch, recorded with its error), then
`kimi-coding/kimi-for-coding` for the remaining `pi-profile-pin` launches.
Every row records its own `reported_model`, and the analysis splits by
provider so arms stay comparable within a provider.
The review prompt's "default branch" line is blank in the harness (the
fixture is a detached worktree, so the harness pins the base explicitly);
this is the only prompt difference from a pipeline run besides the absent
round-history section, which an initial review does not carry either.

Recorded per launch (see `launches-*.jsonl`): wall time, Pi-reported
input/output/cache-read tokens, findings count, reviewed_paths count, risk
level, and the production pre-brief log line (listed/candidate counts, model,
Jev input tokens) for on-mode launches.
The harness now sums tokens over every agent attempt of a launch and records
the attempt count; the rows in this directory predate that (see Honest
limits).
The TypeSafe key came from the environment and was never printed or
persisted; Jev usage is the API-reported `input_tokens` of the one batched
request per launch.
Token semantics: Pi reports fresh (uncached) input and cache reads as
separate counters, so `input_tokens` is the full-price input and
`cache_read_tokens` is billed at the provider's cache rate.
Early rows additionally carry a `fresh_input_tokens` field computed as input
minus cache reads; that derivation is wrong under Pi's semantics and the
field is ignored in the analysis.

Calibration note: the shipped PR originally listed a candidate when its
probability-weighted score reached 2.0.
Live probing during this benchmark showed that rule never fires: Jev spreads probability across adjacent levels,
so even a file it is quite sure is relevant scores 1.6-1.8.
The listing rule was corrected to the rubric's semantics - list when the mass
at "relevant" or "essential" (levels 2+3) is at least 0.5 - and that fix is
part of this PR; every launch below ran with the corrected rule.

## Honest limits

- 3 launches per arm is a small sample; treat deltas as indicative, not
  proven. Token counts on agentic coding launches vary widely between
  identical runs.
- Fresh-input vs cache-read billing rates are provider-specific; the JSONL
  records `input_tokens` and `cache_read_tokens` raw.
  The early rows' `fresh_input_tokens` field is the discarded derivation
  described above, not a measured quantity.
- The recorded rows were captured before the harness summed attempts: each
  carries only the usage of the launch's last successful agent call, and no
  attempt count.
  A launch whose reviewer reran after a schema rejection, or whose adapter
  retried, would be undercounted, and these rows cannot show whether any did.
- Two changes on one repository is not a corpus; the pre-brief's value should
  grow with changes whose important context is unchanged code, which both of
  these are moderate on.
- Quality comparison is findings-count and reviewed_paths parity plus a
  qualitative read of the findings, not a labeled-recall measurement.
