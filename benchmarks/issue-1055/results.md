# Results: Jev review pre-brief benchmark (issue #1055)

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

Method in `method.md`; raw per-launch rows in `launches-*.jsonl`, which the
tables below summarize.
All launches ran 2026-09-19 on the production `ReviewStep`, Pi CLI at xhigh,
with the corrected listing rule (see the calibration note in `method.md`).

## Raw numbers

### jev-prebrief (11 files, +1578/-4), model gpt-6-astra, 3 off / 3 on

| arm | wall s (median) | fresh input (median) | cache reads (median) | output (median) | findings |
| --- | --- | --- | --- | --- | --- |
| off | 316 | 110,468 | 729,088 | 8,570 | 5, 4, 5 |
| on  | 278 | 111,971 | 645,504 | 8,140 | 4, 4, 4 |

### pi-profile-pin (31 files, +1877/-57), model gpt-6-astra, 1 off / 1 on

| arm | wall s | fresh input | cache reads | output | findings |
| --- | --- | --- | --- | --- | --- |
| off | 457 | 170,946 | 2,900,992 | 12,253 | 3 |
| on  | 369 | 164,321 | 2,148,608 | 9,662 | 3 |

The codex account's usage limit was reached after these two; one failed
off-launch is in the JSONL with its error.

### pi-profile-pin, model kimi-for-coding (fallback provider), 3 off / 3 on

| arm | wall s (median) | fresh input (median) | cache reads (median) | output (median) | findings |
| --- | --- | --- | --- | --- | --- |
| off | 1,038 | 133,005 | 3,968,000 | 37,451 | 0, 1, 0 |
| on  | 953 | 129,621 | 5,648,384 | 39,745 | 1, 0, 1 |

Jev cost per on-launch: one batched request, 17,867-18,377 input tokens
(about $0.0008 at the published $0.042/Mtok), latency about 2 s - negligible
next to a multi-minute review.
Every on-launch listed 2-4 surrounding files; coverage was complete in both
arms of every completed launch (11/11 and 31/31 reviewed_paths), so R4 held
throughout.

## Reading

Deltas are on-arm median vs off-arm median; the codex pi-profile-pin pair is
one launch per arm, so it has no spread to compare against.

- Fresh input tokens - the quantity least disturbed by provider caching -
  moved +1.4% on jev-prebrief, -4% on the codex pair, and -2.5% on kimi, and
  the on and off ranges overlap on both changes with three launches per arm.
- Wall time moved -12% (jev-prebrief), -19% (codex pair), and -8% (kimi) -
  the one metric that fell in every comparison.
  On jev-prebrief the arms overlap: two of the three on launches (278 s,
  315 s) sit inside the off arm's 259-325 s range.
  On kimi they do not: every on launch (912, 953, 980 s) was faster than every
  off launch (1,022, 1,038, 1,044 s), whose spread is only 22 s.
  That is a consistent direction, but at n=3 on one change with one model.
- Cache-read volume has no consistent direction: -12% (jev-prebrief, where one
  on-launch read 1.52M, more than double its arm's median), -26% (codex
  pair), and +42% (kimi).
- Output tokens: -5% (jev-prebrief), -21% (codex pair), +6% (kimi).
- Findings parity held on the codex arms (4-5 vs 4 on jev-prebrief; 3 and 3
  on the pi-profile-pin pair).
  Kimi found at most 1 finding in either arm; the model difference dwarfs the
  assist difference.

## Conclusion

On this corpus, at this sample size, the pre-brief does NOT measurably cut
review token cost.
Fresh input moved -4% to +1.4%, and cache reads and output moved in opposite
directions on different comparisons; the median deltas across every arm and
metric run from -26% (codex pair cache reads) to +42% (kimi cache reads).
Wall time is the exception: it fell in all three comparisons (-8% to -19%),
and on kimi every on launch beat every off launch.
With n=3, n=1, and n=3 on two changes, that is an indicative signal worth
re-measuring, not an established saving, and nowhere near a dramatic one.
The effect the design can legally produce is bounded to shortening the cold
reviewer's unguided search: the 2-4-file reading list can save at most a few
exploration rounds on a pass that still reads the complete change.

What the benchmark DOES support:

- The assist is safe on the launches measured: coverage complete in both
  arms, findings parity on the stronger model, and the Jev line item is four
  orders of magnitude below the review launch (~$0.0008 vs dollars).
  Every on launch here got a successful Jev answer, so the fail-closed paths
  (missing key, API error) are not exercised by this benchmark; the unit tests
  (`internal/pipeline/steps/review_prebrief_test.go`, `internal/jev`) and the
  e2e journey `TestJevReviewAssistJourney` cover them.
- The calibration fix is necessary: as originally shipped (weighted score
  >= 2.0), the assist listed nothing on either change - Jev's distributions
  spread across adjacent levels, so the real rule must read the mass at
  levels 2+3 (>= 0.5). Without the benchmark's probing this would have
  shipped as inert code.

What it does not support: the captain's hypothesis that Jev inside full
reviews "should already cut costs and time dramatically".
Dramatic cuts would require removing work from the pass - exactly what the
R4/one-owner constraints forbid.
The remaining honest cost lever for issue #1055 is what the scout report
already concluded: round-count bounds (#683/#986) and provider-level warmth
on a still-complete, still-cold pass.

## Recommendation

Ship the assist only if an opt-in, off-by-default, $0.0008/review reading
list is worth the surface on its own merits; do not ship it on a token-savings
claim, and re-measure the wall-time signal with more launches before claiming
a time saving.
If kept, revisit value on changes whose important context is large amounts of
UNCHANGED code (big monorepos, wide refactors), where the search tail this
assist targets is a larger share of the launch - this corpus's changes were
mostly self-contained.
