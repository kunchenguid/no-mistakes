---
title: Agents
description: Supported AI agents and how they integrate.
---

`no-mistakes` is agent-agnostic. It supports four agents and uses a common interface for all of them. The default `agent: auto` setting picks the first supported agent available on your system. The agent handles code review, test/lint detection (when no explicit command is configured), and auto-fixing.

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

When the daemon is running through a managed service, that `PATH` comes from your login shell environment on macOS and Linux rather than the service manager's default environment. On Windows it reuses the current process environment instead of reloading a login shell. If agent discovery still does not resolve the binary you expect, use an explicit `agent_path_override`.

Override paths in global config:

```yaml
agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
```

## Agent interface

All agents implement the same interface. Each invocation receives:

- **Prompt** - the task description (review this diff, fix these findings, etc.)
- **CWD** - the worktree directory
- **JSONSchema** - optional structured output schema for typed responses
- **OnChunk** - callback for streaming text output to the TUI

Each invocation returns:

- **Output** - structured JSON output (when schema was provided)
- **Text** - raw text output
- **Usage** - token counts (input, output, cache read, cache creation)

## Claude

Spawns a `claude` subprocess for each invocation with `--output-format stream-json` and `--dangerously-skip-permissions`. Reads JSONL events from stdout. Supports native structured output via `--json-schema`.

## Codex

Spawns a `codex` subprocess for each invocation with `exec --json --dangerously-bypass-approvals-and-sandbox`. Reads JSONL events. Structured output is extracted by parsing JSON from the agent's text output.

## Rovo Dev

Starts a persistent HTTP server (`acli rovodev serve`) on first use and reuses it across invocations. Communicates via REST API and SSE streaming. Each invocation creates a session, sends the prompt, streams results, then deletes the session. Structured output is handled by injecting schema instructions into a system prompt.

## OpenCode

Starts a persistent HTTP server (`opencode serve`) on first use. Similar session lifecycle to Rovo Dev: create session, send message, stream SSE events until idle, delete session. Supports `json_schema` format in the message request for structured output.

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
