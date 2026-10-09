# Method: offline replay of candidate relevance for the Jev pre-brief (excerpts)

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

Follow-up to the operator-credentialed live benchmark in this directory
(`method.md`, `results.md`, merged PR #1120 as corpus `jev-prebrief`,
`7e2c5fad..9b697a7b`). That run's on-arm listed 0 of 40 context candidates at
19,155 Jev input tokens, so the review prompt was unchanged and the run left
two questions open: were any of the 40 worth listing, and would bounded
content excerpts change the ranking? This replay answers the first offline and
builds the harness for the second.

## Label set

`candidates.jsonl` marks every candidate of the corpus change relevant or not,
with a one-line reason each: 8 relevant, 32 not. Labels were written from the
change itself (the pre-brief feature: `internal/jev`, `review_prebrief.go`,
global `Jev` config plumbing, the `review.go` hook, the `jevbench` harness,
docs) by attributing each candidate to the identifier that drove it. The
reproduction is exact: rebuilding the candidate set from the repo at the
recorded SHAs yields the same 40 paths in the same order with the same
couplings. Four identifiers drove the set - `GlobalConfig` (24 files),
`ReviewStep` (18), `globalConfigRaw` (2), `Answer` (4) - and the grep budget
(16) ran out with only 4 of 8 identifier slots spent.

Two labels document whole-word false positives worth knowing: the `Answer`
use sites (`docs/.../cli.md`, `internal/cli/axi_drive.go`) match the English
verb in drive help text, not the Jev `Answer` type. Path-only ranking cannot
tell the difference; a bounded excerpt can.

## Replay harness

`jevbench -replay` (`benchmarks/issue-1055/jevbench/replay.go`):

```
go run ./benchmarks/issue-1055/jevbench -replay -repo <repo> \
  -base <sha> -head <sha> -change <name> -labels <candidates.jsonl> \
  -response <recorded.json> -out <dir>
```

With `-response`, the harness ranks the recorded candidates through the
production listing rule (`steps.RankJevPrebrief`: probability-mass threshold,
confidence floor, use-site blend, top-10 cap) when full per-question answers
are present, or measures the recorded listed set as-is when they are not.
Every recorded candidate must have a label, and the recorded SHAs must match
`-base`/`-head`, or the replay refuses.

Without `-response`, the harness rebuilds the production state from the repo
(`steps.BuildJevReplayState`: same digest, same candidates, excerpts per
`-excerpt-bytes`) and calls the live TypeSafe API once, then persists the
full response beside the summary so later replays are score-ranked. The live
arm requires `NO_MISTAKES_JEV_LIVE=1` and `TYPESAFE_API_KEY` and refuses
without both, so plain `go test ./...` and excerpt-off replays never touch
the network. The live arm also refuses a candidate without a label, so an
unlabeled change cannot silently score.

Metrics over the ranked order: hit@10 (labeled-relevant in the first 10, with
recall against all labeled-relevant), listed precision, and the non-empty
pre-brief rate (1 when anything listed, else 0 for the single run).

## What ran here

Offline only, no key, no network:

```
go run ./benchmarks/issue-1055/jevbench -replay -repo . \
  -base 7e2c5fad6cb37f15ea8d3cd90106cb27f993dcef \
  -head 9b697a7bc9448a3d158eb6e8a65c20628535ce08 \
  -change jev-prebrief -labels benchmarks/issue-1125/candidates.jsonl \
  -response benchmarks/issue-1125/recorded-excerpts-off.json \
  -out benchmarks/issue-1125
```

`recorded-excerpts-off.json` carries the reconstructed 40 with the recorded
outcome (0 listed, model `jev-1.13.0`, 19,155 input tokens). Per-question Jev
scores were not persisted by the issue-1125 run, so this replay measures the
listed set, not a score ranking (`score_ranked: false` in the summary). The
excerpts-on live arm - rebuild the state with `-excerpt-bytes`, call, persist,
re-replay - needs an operator-credentialed run and is explicitly not claimed
here.

## Honest limits

- One change, and it is the pre-brief feature itself, which Jev treated as
  self-contained; a corpus of one cannot generalize.
- Labels are one reviewer's judgment calls, written after seeing the outcome;
  they are tracked beside the benchmark so they can be challenged.
- The recorded excerpts-off response has no scores, so hit@10 here is 0 by
  construction (nothing listed) rather than a ranking measurement.
- The excerpts-on comparison is an operator follow-up, not part of this
  delivery.
