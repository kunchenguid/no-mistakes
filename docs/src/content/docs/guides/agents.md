---
title: Choosing an Agent
description: Supported AI agents, how to pick one, and how they integrate.
---

`no-mistakes` is agent-agnostic by design. The gate should mean the same thing
regardless of which agent you prefer. The default `agent: auto` setting picks
the first supported native agent available on your system.

The agent is responsible for the parts of the gate that benefit from judgment:
code review, evidence-oriented test validation, test or lint detection when you
have not configured explicit commands, auto-fixing, and setup-wizard suggestions
when you leave prompts blank.

Pipeline agent prompts also include a workspace-boundary preamble.
It tells agents to keep intentional source, project, user-data, and system file writes inside the disposable worktree, avoid mutating system state such as Homebrew packages, `/Applications`, or global tool config, and treat that boundary as prompt steering rather than true enforcement.
The only intentional out-of-worktree write it allows is test evidence under the managed temporary `no-mistakes-evidence` directory when a testing prompt asks for it; when in-repo evidence is enabled, test evidence stays inside the configured evidence directory instead.
Incidental temp or cache writes from normal development tools are still allowed.
Testing prompts also ask agents to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, before reporting completion.

## How to choose quickly

- Leave `agent: auto` if one good agent is already installed and you do not need repo-specific behavior.
- Set a repo-level `agent` override when one codebase clearly works better with a different tool.
- Set explicit `commands.test` and `commands.lint` if you want deterministic baseline command execution regardless of agent choice.

That last point matters: the agent helps fill in gaps, but explicit repo
commands are still the strongest way to make the baseline gate predictable.
When user intent is available, the test step may still invoke the configured agent after `commands.test` succeeds to gather evidence that demonstrates the change.
That testing invocation is expected to leave only intentional source or test-file changes in the worktree, while preserving requested evidence files under the dedicated evidence directory.
By default that directory is temporary and local to the machine; repos can opt into committed evidence with `test.evidence.store_in_repo`.

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

### Optional ACP target

If you install `acpx` separately, you can opt into any ACP target with the `acp:` prefix.

```yaml
# ~/.no-mistakes/config.yaml or .no-mistakes.yaml
agent: acp:gemini
```

`agent: auto` only probes native agents.
It does not auto-select ACP targets.

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

Before starting validation, agents should run the `no-mistakes axi` home view.
If it shows `active_run`, inspect that current-branch run with `no-mistakes axi status`.
If it is parked at a gate, drive it with `no-mistakes axi respond`.
Reattach an in-flight run by re-running `no-mistakes axi run` when it still matches your current `HEAD`.
If it shows `other_branch_active_run`, they should leave that run alone and start validation for the current branch with `no-mistakes axi run --intent "..."`.
Use `no-mistakes axi abort --run <id>` only when you need to cancel a specific active run by id from outside its worktree.

When an agent starts a new run, `--intent` is required and should describe what the user wanted to accomplish, not what files changed.
Agents should prefer a few complete sentences over a terse summary, capturing user decisions, tradeoffs, constraints, ruled-out approaches, and explicit requests that would not be obvious from the diff alone.
If the repo is on the default branch or has uncommitted changes, direct `axi run` returns a structured error with the command the agent should run instead of silently creating a branch or commit.
Approval gates are exposed as `gate:` objects with finding IDs, severities, files, actions, descriptions, and help commands for `no-mistakes axi respond`.
While a non-terminal run is parked at an `awaiting_approval` or `fix_review` gate, the run object also includes `awaiting_agent: parked <duration>`.
Use that field in `axi status` output to tell in one read that the run is waiting for the driving agent to send `axi respond`, not actively running, fixing, or watching CI.
It is observability only: it does not auto-resume the run, change gate resolution, or make `--yes` the default.
A long-running `axi run` or `axi respond` call is working, not stalled, and an agent may background it if its harness needs to, but the run never advances past a gate on its own, so the agent must read every return and respond at each `gate:`, looping until an `outcome:`, and never idle-wait for the run to move forward by itself.
An agent should resolve `action: auto-fix` findings on its own judgment, ignore `action: no-op` findings when approving, and stop on `action: ask-user` findings unless it is running with explicit `--yes` consent.
Review auto-fix is disabled by default (`auto_fix.review: 0`; a repo or global `auto_fix.review > 0` override re-enables it), so blocking and ask-user review findings park for your decision rather than being silently self-fixed.
The review gate output flags this with a `note`.
When it stops for `ask-user`, it should relay each finding's ID, file, and full description to the user before choosing `approve`, `fix`, or `skip`.
Resolving a finding always means responding with `no-mistakes axi respond --action fix`, which has the pipeline apply the fix and re-review it - the agent must not edit the code itself while a run is active.
For the same reason, while a run is active the agent must not `abort` or `rerun` to go fix a finding itself - even a real bug in its own code - because that discards the pipeline's in-flight work and forces a full re-validation; `abort` and `rerun` are between-runs actions, correct only after a `failed` or `cancelled` outcome, never a way to circumvent a gate.
Successful outputs can be `outcome: passed` for a completed run or `outcome: checks-passed` when CI has passed and the daemon is still monitoring the unmerged PR for humans, and may include a `fixes` table when the pipeline applied fixes.

## Binary resolution

By default, `no-mistakes` resolves `agent: auto` by checking for supported native agents on your `PATH` in this order:

1. `claude`
2. `codex`
3. `opencode`
4. `acli` with `rovodev` support
5. `pi`
6. `copilot`

The default binary names are:

| Agent | Default binary name |
|---|---|
| `claude` | `claude` |
| `codex` | `codex` |
| `rovodev` | `acli` |
| `opencode` | `opencode` |
| `pi` | `pi` |
| `copilot` | `copilot` |
| `acp:<target>` | `acpx` |

When the daemon is running through a managed service, that `PATH` comes from your login shell environment on macOS and Linux plus common user, Homebrew, and system binary directories. If login-shell resolution fails, the daemon logs a warning and uses a degraded fallback `PATH` that may omit version-manager shim directories. On Windows it reuses the current process environment instead of reloading a login shell. If native agent discovery still does not resolve the binary you expect, check `~/.no-mistakes/logs/daemon.log` and use an explicit `agent_path_override`.

Override paths in global config:

```yaml
agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi
  copilot: /usr/local/bin/copilot
```

For ACP targets, set `acpx_path` instead of `agent_path_override`:

```yaml
acpx_path: /Users/you/bin/acpx
```

You can also set extra CLI flags for native agents in global config with
`agent_args_override`. This is useful for things like model selection,
reasoning level, or permission mode. Keep this in global config only, since it
reflects your local agent setup rather than repo policy.

## Agent interface

All agents implement the same interface. Each invocation receives:

- **Prompt** - the task description (review this diff, fix these findings, etc.), prefixed during pipeline runs with the workspace-boundary steering described above
- **CWD** - the worktree directory
- **Environment** - the daemon environment plus non-interactive Git overrides (`GIT_EDITOR=true`, `GIT_SEQUENCE_EDITOR=true`, and `GIT_TERMINAL_PROMPT=0`) so agent-invoked Git commands do not hang on editors or credential prompts
- **JSONSchema** - optional structured output schema for typed responses
- **OnChunk** - callback for streaming text output to the TUI

Each invocation returns:

- **Output** - structured JSON output; native structured responses are returned as-is, while text-parsed fallbacks are validated before return and may use `null` for optional fields
- **Text** - raw text output
- **Usage** - token counts (input, output, cache read, cache creation)

Transient API and network failures are retried up to three times with exponential backoff. Retry messages are streamed through the same `OnChunk` path shown in the TUI.

## Intent extraction

When an agent starts a run through `no-mistakes axi run --intent`, no-mistakes uses that supplied intent verbatim and skips transcript-based inference, even if `intent.enabled` is false.
Otherwise, when `intent.enabled` is true, no-mistakes reads recent local transcripts from Claude Code, Codex, OpenCode, Rovo Dev, Pi, and the GitHub Copilot CLI during the `intent` pipeline step.
It matches sessions against non-deleted changed files when present, falls back to all changed files for all-deletion diffs, summarizes the likely author intent with the configured pipeline agent, includes that summary as untrusted context in rebase fixes, review checks and fixes, test detection, evidence validation, and fixes, lint detection and fixes, documentation checks and fixes, CI auto-fixes, and PR prompts, and renders it in generated PR descriptions.

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

## Codex

Spawns a `codex` subprocess for each invocation with `exec --json`. When structured output is requested, no-mistakes also writes a normalized schema file and passes it with `--output-schema`. By default it also adds `--dangerously-bypass-approvals-and-sandbox`, unless you already set your own Codex approval or sandbox flag through `agent_args_override`. Reads JSONL events. Structured output is returned from the final `agent_message` text, with fallback parsing that accepts JSON fences, inline fence markers, or a final bare JSON object after prose, then validates the result against the normalized schema.

## Rovo Dev

Starts a persistent HTTP server (`acli rovodev serve`) on first use and reuses it across invocations. If a reused server refuses a connection, no-mistakes discards it and retries with a fresh server. Any `agent_args_override.rovodev` flags are inserted before no-mistakes' managed serve flags. Communicates via REST API and SSE streaming. Each invocation creates a session, sends the prompt, streams results, then deletes the session. Structured output is handled by injecting schema instructions into a system prompt, then parsing the final text with fallback parsing that accepts JSON fences, inline fence markers, or a final bare JSON object after prose, and validates the result against the requested schema while allowing `null` for optional fields.

## OpenCode

Starts a persistent HTTP server (`opencode serve`) on first use and reuses it across invocations. If a reused server refuses a connection, no-mistakes discards it and retries with a fresh server. Any `agent_args_override.opencode` flags are inserted before no-mistakes' managed serve flags. Similar session lifecycle to Rovo Dev: create session, send message, stream SSE events until idle, delete session. Supports `json_schema` format in the message request for structured output. When native structured output is absent, it falls back to parsing the final text with the same JSON fence and bare-object fallback, validating that fallback result against the requested schema while allowing `null` for optional fields.

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

Run `no-mistakes doctor` to see which native agent binaries are installed and available:

```
$ no-mistakes doctor
  ✓ git
  ✓ gh
  ✓ data directory
  ✓ database
  ✓ daemon running
  ✓ claude
  – codex (not found)
  – acli (not found)
  – opencode (not found)
  – pi (not found)
  – copilot (not found)
```

`✓` = available, `–` = not found (optional), `✗` = problem detected.

For `agent: acp:<target>`, make sure `acpx` is installed on `PATH` or set `acpx_path` in global config.
`no-mistakes doctor` does not validate ACP targets.
