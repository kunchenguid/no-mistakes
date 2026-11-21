---
title: Choosing an Agent
description: This build supports exactly one pipeline agent, the native omp CLI.
---

`no-mistakes` in this repository is built for exactly one pipeline agent: the native
[omp (Oh My Pi)](https://github.com/badlogic/pi-mono) CLI. There is no harness
selection, no fallback list, and no separate reviewer/fixer agent configuration.
Every pipeline step - review, fix, test evidence, document, lint, rebase conflict
resolution, and PR/CI repair - uses the same omp model and reasoning effort.

The coding agent that calls `no-mistakes axi` drives approval gates, but it does
not become the pipeline agent. Pipeline work runs in the daemon's disposable
worktree through the configured omp binary.

## Requirements

- `omp` on `PATH`, or an absolute path in `agent_path_override.omp`.
- A model and effort configured in `agent_config.omp`, or left to omp's own
  defaults.
- The `agent` key, when present, must be `omp`. Any other harness name fails the
  run at setup.

```yaml
# ~/.no-mistakes/config.yaml
agent: omp

agent_path_override:
  omp: /Users/you/.bun/bin/omp

agent_config:
  omp:
    model: dashscope/qwen4-max
    effort: xhigh
```

A repository may also set `agent: omp` in `.no-mistakes.yaml`; repo config is
trusted-only for agent selection, exactly as before. This build has only one
valid value.

## Invocation shape

no-mistakes launches omp once per pipeline turn:

```
omp --model <model> --thinking <effort> --mode json --no-session < operator prompt
```

Operator `agent_args_override.omp` flags are injected before the managed flags.
Flags no-mistakes owns (`--mode`, `--no-session`, and session-resume flags) are
refused at config load so the JSONL event stream stays parseable.

Structured results are requested by inlining the JSON Schema in the prompt; the
final assistant message is parsed and validated against it. A missing,
malformed, or schema-rejected answer fails that turn rather than passing
silently.

Sessions are always cold. Every pipeline turn - including a review turn that
receives answers to its own questions - starts a fresh omp process, so no turn
can inherit conversational state from the code change it is judging.

## Delivery constraints

The pipeline agent runs only its assigned phase; the outer executor alone performs the
other validation, push, PR, and CI phases. A step that attempts pipeline control gets
`error.code: nested_gate_context`.

If a monitored PR falls behind or conflicts after checks pass, never hand-rebase: the
live CI monitor re-pushes the branch and revalidates from Review. `no-mistakes rerun` is
only the dead-monitor recovery, and it refuses when it cannot prove continuity with the reviewed head.

Commit post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present.

Prerequisite: the native `omp` CLI on `PATH` (or `agent_path_override.omp`) and
`agent: omp`.

## Gate isolation

Under the trusted `disable_project_settings: true` opt-out, a gate turn is
launched with the target repository's project instructions suppressed:

```
omp ... --no-context-files --no-rules --no-skills --no-extensions --config <overlay>
```

The overlay turns off omp memory so repo content read by a gate turn cannot be
retained and recalled around context-file suppression. `no-mistakes doctor`
reports the resolved omp binary; each run performs the authoritative check
again after applying trusted repository configuration.
