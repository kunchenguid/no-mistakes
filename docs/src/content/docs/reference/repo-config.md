---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Per-repo configuration lives in `.no-mistakes.yaml` at the root of your repository.

```yaml
# .no-mistakes.yaml

agent: codex

commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

auto_fix:
  rebase: 0
  review: 3
  test: 3
  lint: 5
  ci: 3
```

## Fields

### agent

Override the default agent for this repo.

| | |
|---|---|
| Type | `string` |
| Values | `claude`, `codex`, `rovodev`, `opencode` |
| Default | Inherits from global config |

### commands.test

Explicit test command. Run via `sh -c`.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects) |

When set, the test step runs this exact command and checks the exit code. When empty, the agent detects and runs relevant tests itself.

### commands.lint

Explicit lint command. Run via `sh -c`.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects) |

Same behavior as `commands.test` - explicit command uses exit code, empty means agent-detected.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
|---|---|
| Type | `string` |
| Default | Empty (no formatter) |

### ignore_patterns

Paths to exclude from the review step's diff.

| | |
|---|---|
| Type | `string[]` |
| Default | Empty (no ignores) |

Pattern matching rules:

| Pattern | Rule |
|---|---|
| `*.generated.go` | No slash - matches by basename |
| `vendor/**` | Ends with `/**` - matches entire subtree |
| `some/path/file.go` | Contains a slash - full path glob |

### auto_fix

Override auto-fix attempt limits for specific steps. Fields not set here inherit from global config.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default |
|---|---|---|
| `auto_fix.rebase` | `int` | Inherits from global (default `0`) |
| `auto_fix.review` | `int` | Inherits from global (default `3`) |
| `auto_fix.test` | `int` | Inherits from global (default `3`) |
| `auto_fix.lint` | `int` | Inherits from global (default `3`) |
| `auto_fix.ci` | `int` | Inherits from global (default `3`) |

Set to `0` to disable auto-fix for a step (always requires manual approval).

Legacy alias: `auto_fix.babysit`.
