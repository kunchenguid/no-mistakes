---
title: Choosing an Agent
description: Supported AI agents, how to pick one, and how they integrate.
---

`no-mistakes` is agent-agnostic by design. The gate should mean the same thing
regardless of which agent you prefer. The default `agent: auto` setting picks
the first supported agent available on your system.

The agent is responsible for the parts of the gate that benefit from judgment:
code review, test or lint detection when you have not configured explicit
commands, auto-fixing, and setup-wizard suggestions when you leave prompts
blank.

## How to choose quickly

- Leave `agent: auto` if one good agent is already installed and you do not need repo-specific behavior.
- Set a repo-level `agent` override when one codebase clearly works better with a different tool.
- Set explicit `commands.test` and `commands.lint` if you want deterministic command execution regardless of agent choice.

That last point matters: the agent helps fill in gaps, but explicit repo
commands are still the strongest way to make the gate predictable.

## Supported agents

| Agent | Binary | Protocol |
|---|---|---|
| Claude | `claude` | Subprocess per invocation, JSONL streaming |
| Codex | `codex` | Subprocess per invocation, JSONL events |
| Rovo Dev | `acli` | Persistent HTTP server, SSE streaming |
| OpenCode | `opencode` | Persistent HTTP server, SSE streaming |

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

## Where agent choice matters most

Changing agents most directly affects:

- review quality and tone
- test and lint detection when commands are not configured
- how good auto-fix attempts are for your stack
- branch name and commit subject suggestions in the setup wizard

It does **not** change the pipeline order or the meaning of a passed gate.

## Binary resolution

By default, `no-mistakes` resolves `agent: auto` by checking for supported agents on your `PATH` in this order:

1. `claude`
2. `codex`
3. `opencode`
4. `acli` with `rovodev` support

The default binary names are:

| Agent | Default binary name |
|---|---|
| `claude` | `claude` |
| `codex` | `codex` |
| `rovodev` | `acli` |
| `opencode` | `opencode` |

When the daemon is running through a managed service, that `PATH` comes from your login shell environment on macOS and Linux plus common user, Homebrew, and system binary directories. On Windows it reuses the current process environment instead of reloading a login shell. If agent discovery still does not resolve the binary you expect, use an explicit `agent_path_override`.

Override paths in global config:

```yaml
agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
```

You can also set extra agent-specific CLI flags in global config with
`agent_args_override`. This is useful for things like model selection,
reasoning level, or permission mode. Keep this in global config only, since it
reflects your local agent setup rather than repo policy.

## Agent interface

All agents implement the same interface. Each invocation receives:

- **Prompt** - the task description (review this diff, fix these findings, etc.)
- **CWD** - the worktree directory
- **JSONSchema** - optional structured output schema for typed responses
- **OnChunk** - callback for streaming text output to the TUI

Each invocation returns:

- **Output** - structured JSON output; native structured responses are returned as-is, while text-parsed fallbacks are validated before return and may use `null` for optional fields
- **Text** - raw text output
- **Usage** - token counts (input, output, cache read, cache creation)

Transient API and network failures are retried up to three times with exponential backoff. Retry messages are streamed through the same `OnChunk` path shown in the TUI.

## Claude

Spawns a `claude` subprocess for each invocation with `--output-format stream-json`. By default it also adds `--dangerously-skip-permissions`, unless you already set your own Claude permission flag through `agent_args_override`. Reads JSONL events from stdout. Supports native structured output via `--json-schema`.

## Codex

Spawns a `codex` subprocess for each invocation with `exec --json`. When structured output is requested, no-mistakes also writes a normalized schema file and passes it with `--output-schema`. By default it also adds `--dangerously-bypass-approvals-and-sandbox`, unless you already set your own Codex approval or sandbox flag through `agent_args_override`. Reads JSONL events. Structured output is returned from the final `agent_message` text, with fallback parsing that accepts JSON fences, inline fence markers, or a final bare JSON object after prose, then validates the result against the normalized schema.

## Rovo Dev

Starts a persistent HTTP server (`acli rovodev serve`) on first use and reuses it across invocations. If a reused server refuses a connection, no-mistakes discards it and retries with a fresh server. Any `agent_args_override.rovodev` flags are inserted before no-mistakes' managed serve flags. Communicates via REST API and SSE streaming. Each invocation creates a session, sends the prompt, streams results, then deletes the session. Structured output is handled by injecting schema instructions into a system prompt, then parsing the final text with fallback parsing that accepts JSON fences, inline fence markers, or a final bare JSON object after prose, and validates the result against the requested schema while allowing `null` for optional fields.

## OpenCode

Starts a persistent HTTP server (`opencode serve`) on first use and reuses it across invocations. If a reused server refuses a connection, no-mistakes discards it and retries with a fresh server. Any `agent_args_override.opencode` flags are inserted before no-mistakes' managed serve flags. Similar session lifecycle to Rovo Dev: create session, send message, stream SSE events until idle, delete session. Supports `json_schema` format in the message request for structured output. When native structured output is absent, it falls back to parsing the final text with the same JSON fence and bare-object fallback, validating that fallback result against the requested schema while allowing `null` for optional fields.

## Checking agent availability

Run `no-mistakes doctor` to see which agent binaries are installed and available:

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
```

`✓` = available, `–` = not found (optional), `✗` = problem detected.
