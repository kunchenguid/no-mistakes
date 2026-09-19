# Results: Jev review pre-brief benchmark (issue #1055)

Method in `method.md`; raw per-launch rows in `launches-*.jsonl`;
`summary.csv` is the same data flattened.
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
arms of every change (11/11 and 31/31 reviewed_paths), so R4 held throughout.

## Reading

- Fresh input tokens - the quantity least disturbed by provider caching -
  moved within noise: +1.4% on jev-prebrief (median), -4% on the single
  codex pi-profile-pin pair, -2.5% on kimi.
- Wall time moved -12% (jev-prebrief), -19% (codex pair), -8% (kimi), all
  within the run-to-run spread of the off arm alone.
- Cache-read volume is noisy (one on-launch on jev-prebrief read 1.52M, more
  than double its arm's median; the kimi on-arm median is 42% ABOVE its off
  arm). No trustworthy direction there.
- Findings parity held on the codex arms (4-5 vs 4 on A; the same 3 on the
  B pair). Kimi found at most 1 finding in either arm; the model difference
  dwarfs the assist difference.

## Conclusion

On this corpus, at this sample size, the pre-brief does NOT measurably cut
review cost or time.
The effect the design can legally produce is bounded to shortening the cold
reviewer's unguided search, and on a repository of this size a frontier
coding agent at xhigh already finds the same surrounding files quickly; the
2-4-file reading list saves at most a few exploration rounds, which is lost
in cache-read noise.
The measured deltas (-12% to +40% depending on arm and metric) are all inside
launch-to-launch variance at n=3-4.

What the benchmark DOES support:

- The assist is safe: coverage complete in both arms, findings parity on the
  stronger model, fail-closed paths exercised (missing key, API error), and
  the Jev line item is four orders of magnitude below the review launch
  (~$0.0008 vs dollars).
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
list is worth the surface on its own merits; do not ship it on a savings
claim.
If kept, revisit value on changes whose important context is large amounts of
UNCHANGED code (big monorepos, wide refactors), where the search tail this
assist targets is a larger share of the launch - this corpus's changes were
mostly self-contained.
