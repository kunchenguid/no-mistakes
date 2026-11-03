# Commit-summary and CI-fix schema proof

The real Pi result is **flat**: main and the candidate each returned **10/10 schema-valid first attempts and 10/10 completed calls**.
Each implementation ran five calls with the commit-summary schema and five with the CI-fix schema.
The candidate finished with the output tool alone in **10/10 calls**.
The request payload, completed assistant messages, and provider response confirmed **openai-codex/gpt-6.1-sol in 20/20 calls**.
These measurements do not demonstrate a reliability improvement on either schema.

| Implementation | Schema | Valid first attempts | Completed calls | Output tool alone | Median seconds |
| --- | --- | ---: | ---: | ---: | ---: |
| Main | commit-summary | 5/5 | 5/5 | 0/5 | 23.410 |
| Candidate | commit-summary | 5/5 | 5/5 | 5/5 | 25.016 |
| Main | CI-fix | 5/5 | 5/5 | 0/5 | 28.712 |
| Candidate | CI-fix | 5/5 | 5/5 | 5/5 | 30.873 |

Main's valid final assistant JSON is its expected prompt-only output, not a failed tool ending.
Every call used one concrete Pi process and one adapter attempt.
There were no adapter retries, Pi automatic retries, schema reruns, or validation exhaustion events.
All 20 streams reached `agent_settled`, and all 20 real post-call tests passed without changes to the test files.
The Review schema was benchmarked separately; its **30/30 versus 30/30** results remain in [the Review report](../results.md).
This proof does not replace that Review benchmark or certify executor, publication, or forge behavior.

## Method

The comparison used freshly fetched main `9dc5ac8bc7fb277bef00991cd731a389a3cbca94` and preserved candidate `0c16c26c64fb5f35256ebecaf459bbe5365aa28c`.
Both executables were built from pinned Git archives, with identical measurement-only exports of the production schemas, repair rules, parser, and consumers.
The production Pi adapters were unchanged in those archives.
The templates are excluded from ordinary Go package discovery and copied into each archived build by the script.

All calls used the real installed Pi **0.99.1**, existing personal OAuth credentials, **openai-codex/gpt-6.1-sol**, and thinking **high**.
No fake CLI, substitute model, stub, mock, recorded response, or request mutation supplied a measured response.
Calls were cold and disabled extension, skill, prompt-template, and context-file discovery equally.
Explicit observer and output extensions still loaded under `--no-extensions`.
Normal built-in tools remained enabled; `--no-tools` was deliberately absent because Pi applies it to extension tools too.

Each call received a fresh ordinary Git repository containing a small real Go defect and failing tests.
The implementation preserved negative counts and replaced positive counts with zero instead of enforcing a nonnegative lower bound.
Pi repaired the comparison and ran the actual tests before submitting its conclusion.
The script independently reran the tests and checked their content hashes after each call.
These repositories had no remotes and were not registered Git worktrees.

The script interleaved main and candidate, reversing their order on alternate repetitions.
The calls ran on 2026-10-05 between **14:59:32 and approximately 15:10:08 UTC**.
Total adapter time was **538.861 seconds**.
Both measurement batches ran under `nice -n 10` with the machine's heavy-run slot held.
The script waited for one-minute load at or below 10 before each invocation.

## Strict generation and validation

The observation-only extension records selected provider/model and `payload.model` through Pi's documented `before_provider_request` hook.
It records the provider's own `response.created.response.model` field through the read-only `provider_stream_event` hook before normalization.
All 20 provider-response model fields were exactly `gpt-6.1-sol`.
Every candidate request declared `no_mistakes_output` with **`strict: true`**; main declared no such tool.
Every candidate's final assistant message contained one output-tool call, matched by a successful execution with `terminate: true`.
No later assistant message displaced that call.

The transparent Python observer launches the real Pi executable and copies its stdout and stderr unchanged.
Its capture preserves model evidence even when an adapter returns an error before assigning model identity to its result.
Concrete process counts use those real launches rather than only `OnAttempt` callbacks.
No provider fallback occurred in this sample.

The production input schemas were identical between implementations and retained `maxLength: 4096` for the summary.
The CI-fix schema also required boolean `code_change_needed`.
The candidate's existing strict declaration omits unsupported keywords, including `maxLength`; the production consumer still checks the summary's byte limit.
**This proof does not show generation-time enforcement of `maxLength`.**
It verifies generation with the declared required fields and types, successful termination, and acceptance against the unchanged output contract.
Oversize summaries and other schema keywords were not exercised by these live calls.
Every raw final response separately passed the production parser and corresponding consumer.
All CI-fix conclusions reported `code_change_needed: true`, consistent with their actual repairs.

## Evidence and reproduction

[`runs.jsonl`](runs.jsonl) records all 20 outcomes, provider evidence, token counters, final-call checks, real repair diffs, and independent validation.
[`manifest.json`](manifest.json) pins the measured implementations, model, Pi version, and original source hashes.
Home paths in the public records were redacted by the existing `benchmarks/issue-1284/export` tool.
The original unredacted events, prompts, outputs, and test logs remain local.
Credentials and complete request payloads were not captured or published.

The published script was relocated to this directory and accepts `--out` for scratch artifacts.
It resolves the real Pi executable from `PATH` or `NM_PROOF_REAL_PI` instead of a machine-specific path.
Those location changes do not alter the measured invocation, workload, observer, validation, or counters.
The original source hashes remain in the measurement manifest; the three Go templates and two observers retain the measured bytes.

From this repository, with the pinned commits and usable personal credentials present:

```sh
pi auth check --provider openai-codex --model gpt-6.1-sol --json
nice -n 10 python3 benchmarks/issue-1284/repair-proof/measure.py --build --out .tmp/repair-proof-new
nice -n 10 python3 benchmarks/issue-1284/repair-proof/measure.py --run --out .tmp/repair-proof-new --limit 4
nice -n 10 python3 benchmarks/issue-1284/repair-proof/measure.py --run --out .tmp/repair-proof-new --limit 16
```

Claim the appropriate heavy-run slot atomically before each batch and release it afterwards.
The script checks load before each invocation, records failed calls without silently replacing them, and stops on missing provider or model evidence.
These commands spend real provider quota.
Export completed outcomes through `go run ./benchmarks/issue-1284/export` before publishing them.
Latency and token counters describe this sample only; no speed or cost improvement is claimed.
