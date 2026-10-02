# Pi schema enforcement: issue #1284

## Expanded result

The larger real benchmark is also **flat**: main and the change each produced **30/30 schema-valid first attempts and 30/30 completed Review analyzer runs**.
Neither arm needed a schema rerun or an adapter retry, and neither exhausted validation.
The reported drift was not reproduced, so these measurements do not demonstrate a reliability improvement.
The supported generation-time constraint is the mechanism change; the observed success rate is unchanged.

| Real historical workload | Changed files | Insertions / deletions | Main first-valid / completed | Strict first-valid / completed |
| --- | ---: | ---: | ---: | ---: |
| Machine-local command overrides | 20 | 1293 / 37 | 10/10 / 10/10 | 10/10 / 10/10 |
| Focused Review coverage completion | 12 | 1054 / 34 | 10/10 / 10/10 | 10/10 / 10/10 |
| Following the reviewer after an answer | 20 | 1136 / 38 | 10/10 / 10/10 | 10/10 / 10/10 |
| Total | | | **30/30 / 30/30** | **30/30 / 30/30** |

All 60 launches reported exactly the complete changed-file set.
Main returned `needs_approval: true` in all 30 runs; the change did so in 29 of 30.
The remaining changed-arm run returned no findings and did not need approval.
“Completed” still means a readable analyzer outcome, not an approved executor gate or a shipped change.
No findings were approved or fixed during measurement.
These are schema measurements, not a finding-accuracy or recall evaluation.

### Expanded method and evidence

The baseline was freshly fetched main at `667530452f6eede6989beeff224954594942d35e`.
The changed production implementation was pinned to `e07fd4ade4bea4581ccadc790849fc62fbdcce47`.
Both binaries were built from archived source, with the identical measurement harness copied into each build.
The harness and script hashes, all workload parent/head commits, Pi version, model, thinking level, and repetition count are recorded in [`large/manifest.json`](large/manifest.json).

The existing isolated task worktree was temporarily checked out at each historical workload head, so file tools saw the actual reviewed snapshot rather than today's files.
Each batch restored the task branch, and the script refused any worktree mutation instead of discarding it.
No additional checkout, registered Git worktree, worktree-pool operation, daemon, or background service was created.
The three workloads were reviewed in round-robin order for ten repetitions each.
Within every pair, main and strict ran consecutively; the first arm alternated between pairs, giving each arm five first positions per workload.
This is deterministic interleaving, not randomized ordering.

All 60 launches used the real Pi **0.99.1** CLI and personal `openai-codex/gpt-6.1-sol` OAuth credentials, with thinking **high** and cold `--no-session` invocations.
Every successful serving-model report matched that provider and model exactly.
No fake CLI, stub, mock, recorded provider reply, substituted model, or payload mutation supplied the measurement.
The same discovery flags, trusted defaults, historical-base pin, `EvalReplay` behavior, and ordinary production validation/retry path described below applied to both arms.
The parent context bounded each Review to 20 minutes; no invocation reached that limit.
The optional coverage-completion turn remained disabled and was unnecessary because every launch already reported complete coverage.

[`large/runs.jsonl`](large/runs.jsonl) preserves the 60 outcome records in launch order.
Each record contains the implementation and workload commits, repetition and sequence, concrete adapter attempts, Review-level calls, raw validated output, typed failed schema field when present, errors, retry counts, completion/exhaustion, wall time, findings, and reviewed paths.
The first-attempt metric uses the first **concrete adapter attempt**, so an internal retry cannot hide an initially rejected response.
Schema-field attribution comes from `agent.SchemaViolation`, not guessed field names parsed out of error text.
There were no failed fields in this cohort.
Unreported token usage is omitted rather than stored as zero.
Keys and raw request payloads are not part of the evidence.
The published copy replaces home-directory prefixes with `~` through the repository's existing `internal/safepath.RedactText` boundary; no measurement counters or verdicts are changed.
The unredacted capture remains local.

The measurement window was 2026-10-02 **01:07–06:44 UTC**, including machine-slot and load waits between batches.
Each batch owned the shared heavy-run slot, ran under `nice -n 10`, and waited for one-minute load at or below 10 before each Review launch.

| Arm | Median seconds | Minimum seconds | Maximum seconds |
| --- | ---: | ---: | ---: |
| Main | 259.736 | 189.310 | 414.419 |
| Strict | 226.161 | 144.117 | 438.548 |

These timings are descriptive, not a demonstrated speed or cost improvement.
Provider caching, scheduling, hidden provider state, and the fixed small workload set limit what this one window can establish.
Thirty successes on each side do not prove a universal reliability rate or show that the original failures cannot recur under different conditions.

### Reproduce the expanded measurement

From a clean task branch, with the historical commits and usable personal credentials present:

```sh
nice -n 10 bash benchmarks/issue-1284/measure-large.sh \
  667530452f6eede6989beeff224954594942d35e \
  e07fd4ade4bea4581ccadc790849fc62fbdcce47 \
  .tmp/pi-schema-large-new 10 1
```

Claim the appropriate machine-wide heavy-run slot atomically before each invocation and release it afterward.
The final argument runs one interleaved pair per invocation to stay within the harness's foreground-command bound.
Repeat the same command until it reports 60/60 launches complete; it resumes only an intact result prefix with identical implementation, workload, CLI, and harness pins.
It stops on provider, authentication, quota, process, or unexpected serving-model failures and does not replace failed launches silently.
This spends real provider quota and temporarily changes only the existing task worktree's HEAD.
Export the completed capture before publishing it:

```sh
go run ./benchmarks/issue-1284/export \
  < .tmp/pi-schema-large-new/runs.jsonl > public-runs.jsonl
```

## Pilot result

The observed schema reliability was **flat**, not improved.
The reported schema drift was not reproduced on this workload.

| Implementation | Schema-valid first attempts | Completed Review analyzer runs | Schema reruns | Full changed-file coverage |
| --- | ---: | ---: | ---: | ---: |
| Main, prompt-only schema | 3/3 | 3/3 | 0 | 3/3 |
| Change, strict-required output tool | 3/3 | 3/3 | 0 | 3/3 |

Every run returned blocking findings and `needs_approval: true`.
“Completed” means `ReviewStep.Execute` returned a readable review outcome, not that a gate was approved or the change passed Review.
No findings were approved or fixed by this benchmark.
The sample provides no measured improvement in failure rate and is too small to establish a reliability rate.
The implementation improvement is the provider's generation-time schema constraint, not an empirically demonstrated increase over these already-valid baseline runs.

## Mechanism investigation

Installed CLI: Pi **0.99.1**.
`pi --help` exposes no built-in `--output-schema` or `--json-schema` flag.
Pi's [CLI integration documentation](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/cli-integration.md) explicitly says JSON mode is event output, not a constraint on the model's response.
The documented RPC prompt command likewise has no schema option.

Pi does expose a supported generation-time mechanism through extensions.
The [Pi AI constrained-tool contract](https://github.com/earendil-works/pi/tree/main/packages/ai#constrained-sampling-for-tools) specifies `constrainedSampling: { type: "json_schema", strict: "require" }`, which fails rather than falling back when the provider/model cannot honor strict sampling.
Pi's [structured-output example](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/examples/extensions/structured-output.ts) demonstrates `terminate: true` for a final result tool without a follow-up model call.
The installed changelog records constrained tool sampling as introduced in **0.82.0**, and earlier releases silently ignore the request.
The adapter therefore probes `pi --version` in Go and loads the strict output-tool extension only for 0.82.0 or later (a prerelease such as `0.82.0-beta` sorts before its release).
For an older or unidentifiable Pi, or when the provider/model refuses the strict tool with Pi's `requires JSON-schema constrained sampling` error (which it raises before sending the request), the adapter falls back to the prompt-inlined schema and validates the final text against the same schema afterwards; a refusal is retried once on that path and remembered for the agent's lifetime, and the step log names which path ran.
On the strict path, a run that ends without calling the output tool alone is a structured-output rejection, so Review's bounded schema rerun applies.
The measured runs below used Pi 0.99.1 on openai-codex, which takes the strict path, so this fallback does not change them.

Before changing the adapter, a real Pi request on the target model accepted a tiny strict-required output tool.
A documented `before_provider_request` hook observed `strict: true` on that tool's OpenAI request declaration.
The JSON stream then reported a successful terminating tool execution followed by settlement, without a follow-up assistant response.
That probe established feasibility, but is not counted as a Review measurement.
The production implementation uses the supported constrained-sampling API, not request-payload mutation or a custom provider.

## Method

Harness: [`reviewbench/main.go`](reviewbench/main.go), calling the production `ReviewStep.Execute` and production Pi adapter with the real installed Pi CLI and its real personal `openai-codex` OAuth credentials.
No fake executable, mocked model, recorded provider response, or fabricated Review result stands in for either measurement arm.
Keys and raw request payloads were not printed or stored in the committed evidence.

Baseline implementation: current main at `667530452f6eede6989beeff224954594942d35e`.
The baseline binary was built before changing `internal/agent/pi.go`.
The changed binary was built from this implementation.
Both arms reviewed the same real seven-file change, `728ffe0f226527a77358bb265be6073c0786367e..667530452f6eede6989beeff224954594942d35e`, containing 407 insertions and 30 deletions.
That change keeps submodules populated by preparation and prevents pipeline staging from recording submodule pointer changes.
The reviewed files and their contents did not change between arms.

Model: **openai-codex/gpt-6.1-sol**, thinking **high**.
All six serving-model reports matched that exact model and provider.
The harness disables extension, skill, and prompt-template discovery equally in both arms; the changed adapter's explicitly supplied invocation extension still loads under `--no-extensions`.
Every launch is cold and uses `--no-session`.
Project context-file loading is unchanged.
No daemon, executor, or mutating pipeline gate wraps the analyzer run.

The harness pins the base through `Config.PR.BaseBranch` and sets `EvalReplay` to prevent an upstream fetch from replacing the historical comparison base with today's main.
That also disables the optional focused coverage-completion turn.
Every run reported all seven reviewable files, so that disabled turn was not needed by either arm.
The ordinary schema-validation retry loop remains active in both arms.
The benchmark therefore measures first-attempt schema validity and complete analyzer outcomes, not push, PR creation, CI, or gate-resolution behavior.

Measurements ran locally on 2026-10-01, under `nice -n 10`, with the shared heavy-run lock held and one-minute load below 10 at launch.
The recorded sample ran three baseline launches followed by three changed launches, not a randomized or alternating trial.
Provider cache state and elapsed-time changes are therefore confounders; no speed or cost gain is claimed.

## Per-launch evidence

Raw outcome records: [`baseline.jsonl`](baseline.jsonl) and [`strict.jsonl`](strict.jsonl).
The pilot predates concrete-attempt instrumentation, so its first-attempt counts refer to the first Review-level call and do not separately measure adapter-internal retries.
Each row records UTC start time, workload commits, adapter attempts, schema-validation success, reported model/provider, token usage, elapsed time, complete findings, and reviewed paths.
Token counters retain Pi's raw input, output, and cache-read semantics; input is not computed by subtracting cache reads.

| Arm | UTC start | Seconds | Attempts | Findings | Reviewed files |
| --- | --- | ---: | ---: | ---: | ---: |
| Main | 21:47:24 | 363.305 | 1 | 1 | 7 |
| Main | 21:57:58 | 252.255 | 1 | 1 | 7 |
| Main | 22:02:10 | 263.160 | 1 | 1 | 7 |
| Strict | 22:09:21 | 261.661 | 1 | 1 | 7 |
| Strict | 22:19:20 | 311.626 | 1 | 1 | 7 |
| Strict | 22:24:32 | 304.342 | 1 | 2 | 7 |

Findings mostly concern the same nested-submodule cleanup ordering, with normal model variation in wording and severity.
This is not a labeled finding-recall or correctness evaluation.

## Reproduce the pilot

From this branch's repository root, with usable personal credentials and the workload commits present:

```sh
pi --version
pi auth check --provider openai-codex --model gpt-6.1-sol --json
bash benchmarks/issue-1284/measure.sh \
  667530452f6eede6989beeff224954594942d35e \
  .tmp/pi-schema-results 3
```

The script builds main from an archived source tree inside `.tmp`, builds the selected implementation, and alternates the two real Review binaries on the unchanged workload.
It refuses to overwrite existing results and stops on provider, credential, or other non-format errors rather than substituting another model.
The script does not create or administer Git worktrees.
Respect local machine load and concurrency limits before running it; it spends real provider quota.
