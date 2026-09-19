# Results: live TypeSafe API and Jev review pre-brief (issue #1125)

Method in `method.md`.
Raw review-launch rows in `launches-jev-prebrief.jsonl`.
Live API evidence in `live-api.json` and `live-prebrief.json`.
All live TypeSafe calls ran 2026-09-19 against `jev-1.13.0` at `https://api.typesafe.ai/v1/systemone`.
The API key never entered this tree, the JSONL, or these notes.

## Live TypeSafe scenario

| path | model | input tokens | output tokens | wall ms | notes |
| --- | --- | --- | --- | --- | --- |
| `TestEvaluate_LiveTypeSafeAPI` | jev-1.13.0 | 429 | 19 | 465 | one score question, tiny state |
| `TestReviewStep_LiveTypeSafePrebrief` | jev-1.13.0 | 1419 | (not logged) | 476 | production pre-brief, 1 of 5 fixture candidates listed, prompt carried the section |

Both calls succeeded.
The production path listed a surrounding file on the small widget fixture, so the listing rule is not inert when Jev assigns enough mass at "relevant" or "essential".

## Off/on review launches

Corpus: merged PR #1120, `7e2c5fad..9b697a7b`, 18 files, +2691/-5.
Agent: Pi `xhigh`.
Completed sample: `kimi-coding/kimi-for-coding`, 2 off / 2 on.
One earlier off-launch against `openai-codex/gpt-6-astra` failed in 5 s with `Codex error: The usage limit has been reached` and is in the JSONL; it is not in the table.

| arm | wall s | input tokens | cache reads | output tokens | findings | reviewed_paths |
| --- | --- | --- | --- | --- | --- | --- |
| off | 994, 702 | 97,234, 84,545 | 2,022,400, 1,084,672 | 31,130, 22,984 | 0, 0 | 18, 18 |
| on  | 920, 768 | 101,304, 89,565 | 1,630,720, 1,072,896 | 30,669, 24,834 | 0, 1 | 18, 18 |

On-launch Jev line item: 19,155 input tokens both times, model `jev-1.13.0`, **0 of 40 context candidates listed**.
Because nothing cleared the listing threshold, `formatJevPrebrief` added no prompt section, so the reviewer prompt was the same complete, session-free review the off arm ran.
Coverage was complete in every completed launch (18/18).
Each completed launch used one agent attempt and reported usage.

Means of the two completed launches per arm (n=2; not a median over a real sample):

| arm | wall s | input | cache reads | output |
| --- | --- | --- | --- | --- |
| off | 848 | 90,890 | 1,553,536 | 27,057 |
| on  | 844 | 95,435 | 1,351,808 | 27,752 |
| on vs off | -0.5% | +5.0% | -13% | +2.6% |
| on + Jev input | 844 | 114,590 | 1,351,808 | 27,752 |
| on + Jev vs off | -0.5% | +26% | -13% | +2.6% |

The `input` column in the first three rows is the Pi reviewer's billed input only.
The last two rows add Jev's 19,155 billed input tokens per on-launch, which is the honest total billed input of the on arm.
Wall time already includes the Jev call, because each launch times the whole `ReviewStep.Execute`.

The ranges overlap on every review-token counter and on wall time.
Total billed input does not overlap: the on arm paid roughly a quarter more fresh input for a prompt that ended up unchanged.

## Reading

The live TypeSafe client works: pin `jev-1.13.0` answers, usage is populated, and the production pre-brief path can list a file on a small coupled fixture.

On this corpus, at this sample size, the pre-brief does not measurably cut review token cost or wall time.
That is the expected result when Jev lists nothing: the assist billed ~19k input tokens and then left the review prompt unchanged, so off/on reviewer deltas are agent noise and the Jev tokens are a pure added cost (+26% total billed input).
Kimi found at most one finding in either arm; findings parity is not a quality claim.

What this run supports:

- The operator-credentialed live path succeeds (issue #1125 gap).
- Fail-closed listing still holds: 0 listed is an empty section, not a degraded review.
- Jev's line item is not negligible on fresh input: 19,155 tokens is about 21% of the off arm's mean reviewer input, and about 1.2% of its input plus cache reads.

What it does not support: a token-savings or wall-time-savings claim, including the indicative wall-time drop in `benchmarks/issue-1055/`.
This re-run cannot confirm or refute that drop: different model (Kimi after a Codex usage-limit refusal), different head (`9b697a7b` rather than the unpublished `5082cdc1`), n=2, and Jev listed nothing here.

## Recommendation

Keep the assist opt-in and off by default.
Publish these numbers with the follow-up; do not ship a savings claim.
Re-measure on a change whose important context is unchanged code if the listing rule starts firing on that corpus - this change was the pre-brief feature itself, which Jev treated as self-contained.
