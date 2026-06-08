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

`no-mistakes init` installs a `/no-mistakes` skill into `.claude/skills/no-mistakes/SKILL.md` and `.agents/skills/no-mistakes/SKILL.md`.
Re-run `no-mistakes init` in an already-initialized repo to refresh or reinstall that skill after an upgrade.
The skill tells agents to use `no-mistakes axi`, a non-interactive command surface that prints TOON to stdout and progress to stderr.
When CI is green but the PR is still open, `axi run` and `axi respond` return `outcome: checks-passed` with a help line pointing at the PR instead of waiting for a human merge.
That is a successful agent stopping point: report that the PR is ready and ask the user to review and merge it.

Agents can also call `no-mistakes axi` directly:

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi status
no-mistakes axi respond --action approve
no-mistakes axi logs --step review --full
no-mistakes axi abort
```

When an agent starts a new run, `--intent` is required and should describe what the user wanted to accomplish, not what files changed.
Agents should prefer a few complete sentences over a terse summary, capturing user decisions, tradeoffs, constraints, ruled-out approaches, and explicit requests that would not be obvious from the diff alone.
If the repo is on the default branch or has uncommitted changes, `axi run` returns a structured error with the command the agent should run instead of silently creating a branch or commit.
Approval gates are exposed as `gate:` objects with finding IDs, severities, files, actions, descriptions, and help commands for `no-mistakes axi respond`.
An agent should resolve `action: auto-fix` findings on its own judgment, ignore `action: no-op` findings when approving, and stop on `action: ask-user` findings unless it is running with explicit `--yes` consent.
When it stops for `ask-user`, it should relay each finding's ID, file, and full description to the user before choosing `approve`, `fix`, or `skip`.
Successful outputs can be `outcome: passed` for a completed run or `outcome: checks-passed` when CI has passed and the daemon is still monitoring the unmerged PR for humans.

## Binary resolution

By default, `no-mistakes` resolves `agent: auto` by checking for supported native agents on your `PATH` in this order:

1. `claude`
2. `codex`
3. `opencode`
4. `acli` with `rovodev` support
5. `pi`

The default binary names are:

| Agent | Default binary name |
|---|---|
| `claude` | `claude` |
| `codex` | `codex` |
| `rovodev` | `acli` |
| `opencode` | `opencode` |
| `pi` | `pi` |
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
Otherwise, when `intent.enabled` is true, no-mistakes reads recent local transcripts from Claude Code, Codex, OpenCode, Rovo Dev, and Pi during the `intent` pipeline step.
It matches sessions against non-deleted changed files when present, falls back to all changed files for all-deletion diffs, summarizes the likely author intent with the configured pipeline agent, includes that summary as untrusted context in rebase fixes, review checks and fixes, test detection, evidence validation, and fixes, lint detection and fixes, documentation checks and fixes, CI auto-fixes, and PR prompts, and renders it in generated PR descriptions.

Transcript readers collect user and assistant text messages but exclude tool call output.
They read Claude Code transcripts from `~/.claude/projects`, Codex metadata from `~/.codex/state_*.sqlite` plus referenced rollout files, OpenCode messages from `$XDG_DATA_HOME/opencode/opencode.db` or `~/.local/share/opencode/opencode.db`, Rovo Dev sessions from `~/.rovodev/sessions`, and Pi transcripts from `~/.pi/agent/sessions`.
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
```

`✓` = available, `–` = not found (optional), `✗` = problem detected.

For `agent: acp:<target>`, make sure `acpx` is installed on `PATH` or set `acpx_path` in global config.
`no-mistakes doctor` does not validate ACP targets.
