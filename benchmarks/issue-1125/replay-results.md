# Results: offline replay of candidate relevance (excerpts)

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

Method in `replay-method.md`. Labels in `candidates.jsonl` (8 relevant, 32
not). Recorded excerpts-off response in `recorded-excerpts-off.json`.
Replay summary in `replay-jev-prebrief.json`.

## Offline replay of the recorded excerpts-off response

| metric | value |
| --- | --- |
| candidates | 40 |
| labeled relevant | 8 |
| ranked order | [] (nothing listed) |
| hit@10 | 0 (recall 0.00) |
| listed | 0 |
| listed precision | 0 (no listing) |
| non-empty pre-brief rate | 0 |
| score_ranked | false |

The 0-of-40 outcome surfaced none of the 8 labeled-relevant candidates. That
is the expected result when Jev lists nothing, and it bounds the claim
accordingly: on this corpus, at path-only, the pre-brief's recall of
surrounding context a reviewer would benefit from was zero - not because the
ranking was measured and failed, but because no score cleared the listing
threshold. The ranking itself was never recorded, so this replay cannot say
whether the 8 sat just below the threshold or far from it; reruns that
persist full answers (`-response` with an answers map, or the live arm) get
that measurement.

## Where the 8 relevant candidates sat

By construction of the code-only ranking, the candidates the pre-brief was
most likely to surface were the tightest-coupled ones: the global-config test
(coupling 0.54, the only `globalConfigRaw` use site) and the `ReviewStep`
use sites (0.056), including the review contract tests (`review_test.go`,
`review_session_test.go`, `review_schema_retry_test.go`), the delivery-path
test, the worktree search-boundary test, and the eval replay driver. The
sibling tail (coupling 0) and the two `Answer` false positives were never
going to clear a relevance bar on coupling alone - and under path-only
ranking, Jev could not see that `Answer` in `axi_drive.go` is a verb.

## Recommendation

The offline half of the idea is done: labels tracked, replay harness
published, excerpts-off baseline recorded. The excerpts-on live arm needs an
operator-credentialed run (`jevbench -replay` without `-response`, with
`-excerpt-bytes` set and the live gate variables), and its result - hit@10,
non-empty rate, and Jev input tokens against this baseline - belongs in this
file when it exists. Do not ship an excerpts effectiveness claim before then.
