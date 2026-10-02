# Pi schema enforcement: issue #1284

## Result

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
The installed changelog records constrained tool sampling as introduced in **0.82.0**; the invocation extension checks the exported `VERSION` before registering the tool so older versions cannot silently ignore that request.

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

## Reproduce

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
