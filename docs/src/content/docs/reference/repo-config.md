---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Per-repo configuration lives in `.no-mistakes.yaml` at the root of your repository.

:::caution[Security: code-executing fields are read from the default branch]
`commands.*` execute arbitrary shell on the daemon host via `sh -c` / `cmd.exe /c`, `agent` selects which process launches there (including `acp:` targets) with the maintainer's credentials, and `steps` selects which validation steps run at all. To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads **`commands`, `agent`, and `steps` from your default branch** (e.g. `origin/main`), never from the pushed SHA, and reads them at the exact commit a fresh fetch resolved (so a stale `origin/<default>` ref cannot serve a value the live default branch removed). If the fetch fails, these fields are forced empty — the run proceeds on built-in defaults rather than falling back to a potentially stale or hostile copy. Commit the `commands`, `agent`, and `steps` you want the gate to run to your default branch. Non-executing fields (`ignore_patterns`, `auto_fix`, `intent`, `test`) are still read from the pushed branch.

If you genuinely want per-branch `commands`, `agent`, and `steps` (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch. This re-enables the previous behavior with eyes open. The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.
:::

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

# Optional: enable/disable/reorder the built-in pipeline steps.
# Omit for the full default pipeline.
# steps: [rebase, test, push, pr, ci]

auto_fix:
  rebase: 3
  review: 3
  test: 3
  document: 3
  lint: 5
  ci: 3

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence
```

## Fields

### agent

Override the default agent for this repo and its setup-wizard suggestions.

| | |
|---|---|
| Type | `string` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `acp:<target>` |
| Default | Inherits from global config |

`auto` resolves to the first supported native agent found on `PATH` in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, then `copilot`.
`acp:<target>` uses the user-installed `acpx` binary configured in global config.
ACP agents are opt-in and are not considered by `agent: auto`.

### allow_repo_commands

Opt in to honoring the code-executing selection fields (`commands.{test,lint,format}`, `agent`, and `steps`) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

This field is itself read **only from the trusted default-branch copy** of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch. By default the daemon reads `commands`, `agent`, and `steps` from your default branch (e.g. `origin/main`) so a pushed SHA cannot inject shell, pick the launched agent on the daemon host, or drop validation steps from the pipeline. Leave this `false` for any repo that accepts contributions. Set it to `true` only for a single-developer environment where you trust every branch you push (for example, a personal repo gated by your own daemon).

### commands.test

Explicit test command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects tests and evidence checks) |

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the agent detects and runs relevant tests itself.
When user intent is available, the agent may still run after a successful baseline command to gather evidence-oriented validation.

### commands.lint

Explicit lint command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the agent detects relevant linters and formatters, applies safe fixes, reruns the relevant checks, commits any agent changes, and reports only unresolved issues.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
|---|---|
| Type | `string` |
| Default | Empty (no separate push-step formatter) |

This does not prevent empty `commands.lint` from detecting and running formatters during the lint step.

### Command process lifetime

All configured `commands.*` entries are scoped to their step.
After no-mistakes starts one of these commands, it terminates any remaining child processes from that command when the command exits, fails, or the step is cancelled.
Do not rely on a configured command to leave a background server or watcher running after it returns; keep that service inside the command lifetime or start it outside no-mistakes.

### ignore_patterns

Paths to exclude from review and documentation checks.

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

### steps

Enable, disable, or reorder the built-in pipeline steps for this repo. Each entry is a step name; the pipeline runs exactly the listed steps, in list order.

| | |
|---|---|
| Type | `string[]` |
| Values | `intent`, `rebase`, `review`, `test`, `document`, `lint`, `push`, `pr`, `ci` |
| Default | Empty (the full default pipeline: `intent → rebase → review → test → document → lint → push → pr → ci`) |

```yaml
steps: [rebase, test, push, pr, ci]
```

Names must be unique and name a built-in step. The push chain keeps its data-loss guarantees: `ci` requires `pr` earlier in the list, `pr` requires `push`, and `push` requires `rebase` (the rebase step's fetch anchors the push step's force-push lease). A list that violates these rules fails the run at start with an error listing every problem — there is no silent fallback. Placing `intent` after the steps that consume it, or a fixing step (`review`, `test`, `document`, `lint`) after `push`, is allowed but logged as a warning since those fixes never reach the remote.

Like `commands` and `agent`, `steps` selects which code executes, so it is read from the trusted default-branch copy of `.no-mistakes.yaml` unless [`allow_repo_commands`](#allow_repo_commands) is enabled. A `steps:` value on a pushed branch is otherwise ignored.

Per-run skips (`--skip`, `git push -o no-mistakes.skip=<steps>`) still apply on top of the configured list.

#### Custom command steps

A `steps` entry may be a mapping instead of a plain name. A mapping carrying a `command` defines a **custom command step** that runs an arbitrary shell command (e.g. `swiftlint`, `xcodebuild test`) as part of the pipeline, reporting pass/fail through the normal gate:

```yaml
steps:
  - rebase
  - name: swiftlint
    command: swiftlint lint --quiet
    findings_json: build/swiftlint.json   # optional: structured findings the step ingests
    timeout: 5m                            # optional: bounds a long/hung command
    auto_fix: true                         # optional: mark findings auto-fixable
    instructions:                          # optional: guidance injected into agent steps
      - .no-mistakes/swift-review.md
  - review
  - push
  - pr
  - ci
```

| Field | Type | Meaning |
|---|---|---|
| `name` | `string` | Step identity (lowercase letters, digits, `-`, `_`). Must be unique and must not collide with a built-in step name. |
| `command` | `string` | Shell command to run. Its presence marks the entry as a custom step. Executes on the daemon host, so — like `commands` — it is honored only from the trusted default-branch copy. |
| `findings_json` | `string` | Optional worktree-relative path the command writes findings JSON to. When present, the step parses it into real per-file/per-line findings instead of mapping a bare exit code to one finding. Accepts the full findings object (`{"findings": [...], "summary": ...}`) or a bare array of finding objects. If the file is absent, the step falls back to exit-code mapping. |
| `timeout` | `duration` | Optional per-step timeout (Go duration, e.g. `5m`, `90s`). Defaults to 30m. A step that exceeds it is killed and gates with a clear timeout finding. |
| `auto_fix` | `bool` | Optional. When `true` the step's findings are marked auto-fixable (the executor may drive an agent to resolve them, up to the built-in default of 3 attempts). Default `false`: findings park for an agent/human decision, consistent with the built-in review step. |
| `instructions` | `string[]` | Optional instruction-file paths whose contents are injected into the built-in agent steps (review, test, lint, document). See below. |

A mapping with only a `name` (no `command`) is a built-in step; it may still carry `instructions`.

#### Per-step instructions

`instructions` lets a repo inject maintainer-authored guidance into the built-in agent steps (for example, review conventions specific to your codebase). The file **contents** are read at the trusted default-branch SHA — never the pushed worktree — and sanitized before injection, so a pushed branch cannot rewrite the guidance the gate injects into its own agent steps. Instruction files absent on the trusted default branch simply contribute nothing. This trusted-SHA read is enforced regardless of `allow_repo_commands`.

### auto_fix

Override auto-fix attempt limits for specific steps. Fields not set here inherit from global config.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default |
|---|---|---|
| `auto_fix.rebase` | `int` | Inherits from global (default `3`) |
| `auto_fix.review` | `int` | Inherits from global (default `0`) |
| `auto_fix.test` | `int` | Inherits from global (default `3`) |
| `auto_fix.document` | `int` | Inherits from global (default `3`) |
| `auto_fix.lint` | `int` | Inherits from global (default `3`) |
| `auto_fix.ci` | `int` | Inherits from global (default `3`) |

Set to `0` to disable the follow-up auto-fix loop for a step (findings require manual approval).
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the agent still attempts safe fixes during the initial lint pass; unresolved lint findings then pause for approval instead of starting another automatic fix loop.

`auto_fix.ci` covers the CI step's CI failure and merge-conflict auto-fix attempts.

Legacy alias: `auto_fix.babysit`.

### intent

Override transcript-based user-intent extraction settings for this repo.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
|---|---|---|
| `intent.enabled` | `bool` | Inherits from global (default `true`) |
| `intent.threshold` | `float` | Inherits from global (default `0.2`) |
| `intent.slack_days` | `int` | Inherits from global (default `3`) |
| `intent.disabled_readers` | `string[]` | Adds to globally disabled readers |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

### test.evidence

Override where evidence artifacts from the test step are stored.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
|---|---|---|
| `test.evidence.store_in_repo` | `bool` | Inherits from global (default `false`) |
| `test.evidence.dir` | `string` | Inherits from global (default `.no-mistakes/evidence`) |

By default, test evidence stays in a temporary directory keyed by run ID and is referenced by local path.
Set `store_in_repo: true` to write evidence under `<dir>/<branch-slug>` inside the worktree so push can commit and publish it with the branch.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.
