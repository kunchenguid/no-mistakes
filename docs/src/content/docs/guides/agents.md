---
title: Choosing an Agent
description: Supported AI agents, how to pick one, and how they integrate.
---

`no-mistakes` is pipeline-agent-agnostic by design: the gate should mean the same thing regardless of which supported agent backend you prefer.
It is not runner-free.
Every validation run requires either a supported native agent binary or `acpx` configured for an ACP target.
The default `agent: auto` setting picks the first supported native agent available on your system.

The coding agent that calls `no-mistakes axi` drives approval gates, but it does not automatically become the pipeline agent that performs review, evidence testing, documentation, combined documentation-and-lint housekeeping, or fixes.
Those jobs run in the daemon's disposable worktree through the configured pipeline agent.

The agent is responsible for the parts of the gate that benefit from judgment:
code review, evidence-oriented test validation, test or lint detection when you
have not configured explicit commands, auto-fixing, and setup-wizard suggestions
when you leave prompts blank.

Pipeline agent prompts also include a workspace-boundary preamble.
It tells agents to keep intentional source, project, user-data, and system file writes inside the disposable worktree, avoid mutating system state such as Homebrew packages, `/Applications`, or global tool config, and treat that boundary as prompt steering rather than true enforcement.
The only intentional out-of-worktree write it allows is fallback test evidence under the managed temporary `no-mistakes-evidence` directory when a testing prompt asks for it; by default, test evidence stays inside the configured in-repo evidence directory instead.
Incidental temp or cache writes from normal development tools are still allowed.
Testing prompts also ask agents to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, before reporting completion.

## How to choose quickly

- Leave `agent: auto` if one good agent is already installed and you do not need repo-specific behavior.
- Set a repo-level `agent` override when one codebase clearly works better with a different tool.
- Use an ordered fallback list when you prefer one agent but want no-mistakes to try another if the first process is unavailable.
- Set explicit `commands.test` and `commands.lint` if you want deterministic baseline command execution regardless of agent choice.

That last point matters: the agent helps fill in gaps, but explicit repo
commands are still the strongest way to make the baseline gate predictable.
When user intent is available, the test step may still invoke the configured agent after `commands.test` succeeds to gather evidence that demonstrates the change.
That testing invocation is expected to leave only intentional source or test-file changes in the worktree, while preserving requested evidence files under the dedicated evidence directory.
By default that directory is inside the repo and published with the branch; repos can explicitly opt out with `test.evidence.store_in_repo: false`.

## Supported agents

| Agent | Binary | Protocol |
|---|---|---|
| Claude | `claude` | Subprocess per invocation, JSONL streaming |
| Codex | `codex` | Subprocess per invocation, JSONL events |
| Rovo Dev | `acli` | Persistent HTTP server, SSE streaming |
| OpenCode | `opencode` | Persistent HTTP server, SSE streaming |
| Pi | `pi` | Subprocess per invocation, JSONL events |
| Copilot | `copilot` | Subprocess per invocation, JSONL events |
| ACP target | `acpx` | Optional user-installed ACP bridge |

## Runner requirements

A complete gate never degrades silently when its configured pipeline agent is unavailable.
The daemon resolves the effective agent before creating pipeline step records, and the run fails immediately with setup guidance if the configured binary cannot run.
This refusal also applies when deterministic test or lint commands are configured because review and documentation always require agent judgment, while rebase, PR, and CI paths may need an agent to resolve conflicts, generate content, or fix failures.

| Surface or capability | Works without a runnable pipeline agent? | Behavior |
|---|---:|---|
| Install, `init`, daemon lifecycle, `status`, `runs`, and `doctor` | Yes | Local setup and diagnostics remain available. `doctor` reports that gate validation is unavailable. |
| Start or rerun a validation gate | No | The run fails before any pipeline step starts. |
| Review | No | Requires agent judgment and structured findings. |
| Test with `commands.test` | No, as part of a full gate | The command is deterministic, but the gate refuses before steps start rather than presenting command-only validation as a complete pass. |
| Test without `commands.test`, or evidence validation with user intent | No | Requires the agent to discover checks and gather end-to-end evidence. |
| Document | No | Requires the agent to discover and update documentation gaps. |
| Lint with `commands.lint` | No, as part of a full gate | The command is deterministic, but the full gate still requires an agent. |
| Lint without `commands.lint` and all fix rounds | No | The document step performs the initial combined housekeeping pass, and an agent is still needed for fallback assessment or code changes. |
| Push, PR, and CI as part of a gate | No | They run only after the required validation steps, and PR or CI paths may invoke the agent themselves. |

### Antigravity and Gemini setups

Running the gate from Antigravity or another Gemini-based coding environment does not make that calling model available to the daemon automatically.
Choose one of these supported setups:

1. Install any supported native agent CLI and leave `agent: auto`, or select it explicitly in `~/.no-mistakes/config.yaml`.
2. Install `acpx`, confirm that the Gemini ACP target works locally, and configure `agent: acp:gemini`.

```yaml
# ~/.no-mistakes/config.yaml
agent: acp:gemini

# Optional when acpx is not on PATH.
acpx_path: C:\path\to\acpx.exe
```

Run `no-mistakes doctor` afterward and look for a successful `gate validation` line.
Doctor checks the global agent configuration; each run performs the authoritative check again after applying any trusted repository-level agent override.
If the calling environment exposes neither a supported native CLI nor a working ACP target, it can still inspect and respond to existing AXI state, but it cannot start an honest validation gate by itself.

## Setting the agent

### Global default

```yaml
# ~/.no-mistakes/config.yaml
agent: auto
```

### Per-repo override

```yaml
# .no-mistakes.yaml
agent: codex
```

Repo config takes precedence over global config.

### Ordered fallback list

```yaml
# ~/.no-mistakes/config.yaml or .no-mistakes.yaml
agent: [codex, claude]
```

### Optional ACP target

If you install `acpx` separately, you can opt into any ACP target with the `acp:` prefix, for example `agent: acp:gemini`.
`agent: auto` only probes native agents and never auto-selects ACP targets.

The [`agent` field reference](/no-mistakes/reference/global-config/#agent) owns the exact resolution order, fallback-list filtering and retry semantics, and the failure behavior when no entry is runnable.

## Where agent choice matters most

Changing agents most directly affects:

- review quality and tone
- test evidence collection, plus test and lint detection when commands are not configured
- how good auto-fix attempts are for your stack
- branch name and commit subject suggestions in the setup wizard

It does **not** change the pipeline order or the meaning of a passed gate.

## Driving no-mistakes as an agent

The primary way to put a change through the gate from inside a coding agent is the `/no-mistakes` skill.
A skill-aware tool like Claude Code supports two invocation modes.
Use bare `/no-mistakes` to validate existing committed work.
Use `/no-mistakes <task>` to have the agent first do the task, commit only that task's changes on a feature branch, then run the pipeline with the task text as `--intent`.
In both modes, it resolves low-risk findings on its own and stops to relay anything that needs your decision.

`no-mistakes init` installs that skill at user level: `~/.claude/skills/no-mistakes/SKILL.md` for Claude Code and `~/.agents/skills/no-mistakes/SKILL.md` for Codex, OpenCode, Rovo Dev, and Pi.
One install makes the skill available to every supported agent in every repo, without committing tool-generated files to any repo.
If your home directory consolidates `.claude` and `.agents` with symlinks, `init` follows the links and keeps the skill reachable from both logical paths.
Re-run `no-mistakes init` after an upgrade to refresh that skill, including overwriting stale `SKILL.md` content from an older binary.
Older versions vendored the skill into each initialized repo's `.claude/skills` and `.agents/skills`; those copies are no longer needed, and `init` prints a notice when it finds one so you can remove it.
The skill drives `no-mistakes axi`, a non-interactive command surface that prints TOON to stdout and progress to stderr.
When CI is green but the PR is still open, `axi run` and `axi respond` return `outcome: checks-passed` with a help line pointing at the PR instead of waiting for a human merge.
That is a successful agent stopping point: report that the PR is ready and ask the user to review and merge it.
Successful outcomes also instruct the agent to summarize the run for the user.
When the pipeline applied fixes, successful outcomes include a `fixes` table listing each fix so the agent can acknowledge what it missed and the user can review them.

If that PR later falls behind the default branch or hits a merge conflict - commonly because another PR merged first - the agent runs no command and must never hand-rebase.
The CI monitor stays live in the background after checks pass, and when it sees an actual conflict it rebases onto the base, resolves it, and re-pushes the branch itself, so no agent or user action is needed.
A PR that is merely behind but still clean needs nothing either, since the platform merges it.
The one exception is when that monitor is no longer running - the PR was closed, the run was aborted or superseded, it idle-timed-out, or its auto-fix attempts were exhausted - in which case the agent recovers with `no-mistakes rerun`, which cancels the stale monitor and re-runs the full pipeline including a deterministic rebase step.
The agent must not use `no-mistakes axi run` to refresh a still-active PR: after `checks-passed` it reattaches to the running monitor with HEAD unchanged and returns the monitor output without rebasing.

In task-first mode, if the repo is on the default branch, the skill tells the agent to create a feature branch before committing because the gate validates committed history on a non-default branch.
The agent should inspect `git status` before changing or committing anything, preserve unrelated pre-existing uncommitted changes, and commit only the changes that belong to the user's task.

Agents can also call `no-mistakes axi` directly:

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi status
no-mistakes axi respond --action approve
no-mistakes axi logs --step review --full
no-mistakes axi abort
no-mistakes axi abort --run <id>
```

When an agent makes an additional fix after a gate round has already produced fix commits - a newly surfaced finding, a reviewer or pre-merge request, or any other post-completion change - it should commit the fix on top of the existing branch and run `no-mistakes axi run --intent "..."` with the original user intent.
Never abort-and-restart, reset the branch, or open a new branch in a way that drops prior gate-fix commits, including the pipeline's own `no-mistakes(review|document|lint): ...` commits.
A fresh run re-validates the branch's current state, so already-resolved findings do not re-surface.

The full driving protocol - how to read the home view and `gate:` objects, when to respond, fix, approve, or relay `ask-user` findings, and how to interpret `axi status` fields like `awaiting_agent` and `active_steps` - is owned by the skill itself and by the live `axi` output.
Each `axi` response carries version-matched `help` lines for its state, and `no-mistakes axi run --help` and `no-mistakes axi respond --help` describe the loop authoritatively for the installed binary, so agents driving a gate never need this page open.
The [CLI reference](/no-mistakes/reference/cli/) documents each `axi` command and output field for humans.

## Binary resolution

When the daemon is running through a managed service, its `PATH` comes from your login shell environment on macOS and Linux plus common user, Homebrew, and system binary directories; on Windows it reuses the current process environment.
If native agent discovery does not resolve the binary you expect, check `~/.no-mistakes/logs/daemon.log` and set an explicit override; [Environment the daemon sees](/no-mistakes/reference/environment/#environment-the-daemon-sees) owns the full resolution story.

Three global config fields tune resolution and invocation, and the [Global Config Reference](/no-mistakes/reference/global-config/) owns each one:

- [`agent_path_override`](/no-mistakes/reference/global-config/#agent_path_override) - custom binary paths per native agent, plus the default binary-name table ([`acpx_path`](/no-mistakes/reference/global-config/#acpx_path) is the ACP equivalent).
- [`agent_args_override`](/no-mistakes/reference/global-config/#agent_args_override) - extra CLI flags per native agent for model selection, service tier, reasoning depth, or permission mode, including the reserved-flag rules and smart defaults. Keep it global-only; it reflects your local agent setup rather than repo policy.
- [`agent`](/no-mistakes/reference/global-config/#agent) - the `auto` resolution order and ordered fallback-list semantics.

## Review session reuse

With the default `session_reuse: true`, Claude and Codex keep one durable reviewer session and a separate review-fixer session per run, every rereview still evaluates the entire branch diff, and resume failures fall back to fresh same-role sessions instead of skipping review.
The [`session_reuse` field reference](/no-mistakes/reference/global-config/#session_reuse) owns the exact reuse, fallback, privacy, and restart-recovery semantics.

## Agent interface

All agents implement the same interface. Each invocation receives:

- **Prompt** - the task description (review this diff, fix these findings, etc.), prefixed during pipeline runs with the workspace-boundary steering described above
- **CWD** - the worktree directory
- **Environment** - the daemon environment plus non-interactive Git overrides (`GIT_EDITOR=true`, `GIT_SEQUENCE_EDITOR=true`, and `GIT_TERMINAL_PROMPT=0`) so agent-invoked Git commands do not hang on editors or credential prompts
- **JSONSchema** - optional structured output schema for typed responses
- **OnChunk** - callback for streaming text output to the TUI
- **OnLifecycle** - callback for native subprocess start, exit, and retry activity that is recorded in step logs and AXI active-step status
- **Session** - optional no-mistakes-owned native session identity for review-loop reuse
- **Purpose** - local performance label for the pipeline duty served

Each invocation returns:

- **Output** - structured JSON output; native structured responses are returned as-is, while text-parsed fallbacks are validated before return and may use `null` for optional fields
- **Text** - raw text output
- **Usage** - token counts (input, output, cache read, cache creation)
- **SessionID** and **Resumed** - the adapter-native session identity and whether this invocation resumed it, when supported
- **Model** and **Provider** - adapter-reported serving metadata when available

One-shot subprocess agents (Claude, Codex, Pi, Copilot CLI, and acpx) are invocation-scoped.
After no-mistakes starts one, it terminates any remaining child processes when the invocation exits, fails, or is cancelled, so agent-spawned test workers, build watchers, and dev servers do not survive the step.
Step logs record their process lifecycle, including start and exit lines with the PID, and AXI status exposes that PID while the subprocess is still active.
Persistent server agents (Rovo Dev and OpenCode) use their managed server lifecycle instead.

Transient API and network failures are retried up to three times with exponential backoff. Retry messages are recorded as lifecycle activity for native subprocess agents, falling back to the streaming text path for direct callers that do not supply `OnLifecycle`.

## Intent extraction

When an agent starts a run through `no-mistakes axi run --intent`, no-mistakes uses that supplied intent verbatim as authoritative acceptance criteria and skips transcript-based inference, even if `intent.enabled` is false.
Review checks the diff against those criteria, and a change that removes required behavior or adds forbidden behavior becomes an `ask-user` finding instead of being resolved automatically.
Otherwise, when `intent.enabled` is true, no-mistakes reads recent local transcripts from Claude Code, Codex, OpenCode, Rovo Dev, Pi, and the GitHub Copilot CLI during the `intent` pipeline step.
It matches sessions against non-deleted changed files when present, falls back to all changed files for all-deletion diffs, summarizes the likely author intent with the configured pipeline agent, includes that summary as an untrusted, low-confidence hint in rebase fixes, review checks and fixes, test detection, evidence validation, and fixes, lint detection and fixes, documentation checks and fixes, CI auto-fixes, and PR prompts, and renders it in generated PR descriptions.

Transcript readers collect user and assistant text messages but exclude tool call output.
They read Claude Code transcripts from `~/.claude/projects`, Codex metadata from `~/.codex/state_*.sqlite` plus referenced rollout files, OpenCode messages from `$XDG_DATA_HOME/opencode/opencode.db` or `~/.local/share/opencode/opencode.db`, Rovo Dev sessions from `~/.rovodev/sessions`, Pi transcripts from `~/.pi/agent/sessions`, and GitHub Copilot CLI sessions from `~/.copilot/session-state`.
Sessions are eligible when they come from the same working directory or an equivalent Git checkout with the same common Git directory or normalized remote URL.
ACP transcripts are not currently read for intent extraction.
When deterministic matching leaves multiple plausible sessions, no-mistakes may ask the configured pipeline agent to choose among them using the matching file paths and sanitized transcript packet files.
The selected transcript text is then sent to the configured pipeline agent for summarization during the `intent` step, so intent extraction may incur additional agent or API invocations.
Before disambiguation or summarization, no-mistakes excludes tool output, redacts likely secrets, strips common prompt-control markers, and clamps long transcripts while preserving the beginning and end.
no-mistakes stores derived intent summaries and matching metadata in `~/.no-mistakes/state.sqlite`, including the source, session ID, and match score on each run plus cached summaries for matching transcript sessions.
It does not store raw transcript text in its database.
The step logs accepted candidate match diagnostics, then logs the matched source, score, and sanitized inferred intent when a transcript matches.

Use `intent.disabled_readers` to disable specific transcript sources, or set `intent.enabled: false` to opt out entirely.

## Claude

Spawns a `claude` subprocess for each invocation with `--output-format stream-json`. By default it also adds `--dangerously-skip-permissions`, unless you already set your own Claude permission flag through `agent_args_override`. Reads JSONL events from stdout. Supports native structured output via `--json-schema`.
For review-loop reuse, Claude starts a stream-json session and resumes it with `claude -p --resume <id>`.

## Codex

Spawns a `codex` subprocess for each invocation with `exec --json`. When structured output is requested, no-mistakes also writes a normalized schema file and passes it with `--output-schema`. By default it also adds `--dangerously-bypass-approvals-and-sandbox`, unless you already set your own Codex approval or sandbox flag through `agent_args_override`. Reads JSONL events. Structured output is returned from the final `agent_message` text, with fallback parsing that accepts JSON fences, inline fence markers, or a final bare JSON object after prose, then validates the result against the normalized schema.
Codex model and config overrides, such as `-m gpt-5.4`, `-c service_tier="priority"`, or `-c model_reasoning_effort="low"`, belong in global `agent_args_override.codex`.
For review-loop reuse, Codex resumes the reported thread with `codex exec resume <id> <prompt>`.
That resume command has a narrower flag surface than `codex exec`, so a resume that rejects an override falls back to a fresh same-role session rather than skipping the review turn.

## Rovo Dev

Starts a persistent HTTP server (`acli rovodev serve`) on first use and reuses it across invocations. If a reused server refuses a connection, no-mistakes discards it and retries with a fresh server. Any `agent_args_override.rovodev` flags are inserted before no-mistakes' managed serve flags. Communicates via REST API and SSE streaming. Each invocation creates a session, sends the prompt, streams results, then deletes the session. Structured output is handled by injecting schema instructions into a system prompt, then parsing the final text with fallback parsing that accepts JSON fences, inline fence markers, or a final bare JSON object after prose, and validates the result against the requested schema while allowing `null` for optional fields.

## OpenCode

Starts a persistent HTTP server (`opencode serve`) on first use and reuses it across invocations. If a reused server refuses a connection, no-mistakes discards it and retries with a fresh server. Any `agent_args_override.opencode` flags are inserted before no-mistakes' managed serve flags. Similar session lifecycle to Rovo Dev: create session, send message, stream SSE events until idle, delete session. Supports `json_schema` format in the message request for structured output, with `retryCount: 2` so the model gets a second chance to emit a structured response. When opencode reports `info.error.name = "StructuredOutputError"` (the model did not call the StructuredOutput tool after those retries), no-mistakes surfaces a clean error including the retry count rather than falling through to text-parsing the streamed reasoning prose. When native structured output is genuinely absent, it falls back to parsing the final text with the same JSON fence and bare-object fallback, validating that fallback result against the requested schema while allowing `null` for optional fields.

## Pi

Spawns a `pi` subprocess for each invocation with `--mode json --no-session`.
Any `agent_args_override.pi` flags are inserted before no-mistakes' managed flags.
Reads JSONL events from stdout and streams incremental text deltas to the TUI.
When structured output is requested, no-mistakes injects the JSON schema into the prompt and validates the final text response.

## Copilot CLI

Spawns a `copilot` subprocess for each invocation with `-p <prompt> --output-format json`.
It also adds `--no-color` and `--no-ask-user` so the run is non-interactive, plus `--allow-all-tools` (required for non-interactive mode) unless you already set your own Copilot permission flag through `agent_args_override`.
Any `agent_args_override.copilot` flags are inserted before no-mistakes' managed flags, so user choices such as `--model` or `--effort` take effect.
Reads JSONL events from stdout, streaming incremental `assistant.message_delta` text to the TUI and capturing the final `assistant.message` content.
The Copilot CLI has no output-schema flag, so when structured output is requested no-mistakes injects the JSON schema into the prompt and validates the final text response with the same JSON fence and bare-object fallback used by Pi and Rovo Dev.

## ACP via acpx

ACP support is optional and requires a separately installed `acpx` binary.
Use `agent: acp:<target>` to run a target known to acpx, for example `agent: acp:gemini`.

For custom ACP target commands, define a global override:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

no-mistakes invokes acpx with JSON output, approve-all permissions, denied non-interactive permission prompts, and the repo worktree as `--cwd`.
Structured output is handled by appending the requested JSON schema to the prompt and validating the final assistant text.

## Checking agent availability

Run `no-mistakes doctor` to inspect individual native and ACP runner binaries and to check the effective global agent configuration:

```
$ no-mistakes doctor
  ✓ git
  ✓ gh
  ✓ data directory
  ✓ database
  ✓ daemon running
  ✓ claude
  – codex (not found)
  – rovodev (not found)
  – opencode (not found)
  – pi (not found)
  – copilot (not found)
  – acpx (not found)
  ✓ gate validation claude is runnable
```

`✓` = available, `–` = not found (optional), `✗` = problem detected.
The `gate validation` line is the decisive result: when the configured global runner is unavailable, doctor fails because a complete gate cannot validate without it.

For `agent: acp:<target>`, doctor verifies that `acpx` is installed on `PATH` or resolves through `acpx_path` in global config.
It does not invoke the target or test its credentials.
Every new validation run resolves its effective agent again after applying any trusted repository-level override.
