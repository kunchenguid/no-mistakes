---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Per-repo configuration lives in `.no-mistakes.yaml` at the root of your repository.

:::caution[Security: gate-control fields are read from the default branch]
`commands.*` execute arbitrary shell on the daemon host via `sh -c` / `cmd.exe /c`, and `agent` selects which process launches there (including ordered fallback lists, ACP aliases such as `cursor`, and `acp:` targets) with the maintainer's credentials.
To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads **`commands` and `agent` from your default branch** (e.g. `origin/main`), never from the pushed SHA, and reads them at the exact commit a fresh fetch resolved (so a stale `origin/<default>` ref cannot serve a value the live default branch removed).
The daemon also reads `document.instructions` and `disable_project_settings` only from that trusted copy.
If the default branch cannot be fetched and resolved to a readable commit, or its present `.no-mistakes.yaml` cannot be read and parsed, the run aborts before launching an agent.
A readable default-branch tree with no `.no-mistakes.yaml` is valid and uses defaults.
Commit the gate-control settings you want to your default branch.
Non-executing fields (`ignore_patterns`, `auto_fix`, `commit`, `intent`, `test`) are still read from the pushed branch.

If you genuinely want per-branch `commands` and `agent` (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch. This re-enables the previous behavior with eyes open. The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.
:::

```yaml
# .no-mistakes.yaml

agent: codex

commands:
  lint: "golangci-lint run ./..."
  # Targeted local validation only - not a full-repo CI-parity suite.
  test: "go test ./internal/cli -run '^TestDoctor' -count=1"
  format: "gofmt -w ."

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

# Optional documentation ownership policy, read only from the trusted default branch.
document:
  instructions: |
    docs/ owns detailed product guidance; README.md owns the introduction.

# For orchestration repos whose project instructions would misidentify gate agents.
# Read only from the trusted default branch. Defaults to false.
disable_project_settings: true

auto_fix:
  rebase: 3
  review: 3
  test: 3
  document: 3
  lint: 5
  ci: 3

commit:
  fix_message: "chore(no-mistakes-{{.Step}}): {{.Summary}}"

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
| Type | `string` or `string[]` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `cursor`, `acp:<target>` |
| Default | Inherits from global config |

`auto` resolves to the first supported native agent or ACP alias in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, then `cursor`.
`cursor` is an ACP alias for the `cursor` target with default command `cursor-agent acp`.
Its availability uses the global `acpx_path` and `acp_registry_overrides.cursor` settings when present.
`acp:<target>` uses the user-installed `acpx` binary configured in global config; `acp:cursor` uses the same default command as `cursor`.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If the selected explicit agent or `auto` is unavailable, the gate fails before its first pipeline step rather than reporting partial validation as passed.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
After resolving `auto`, entries that resolve to the same ACP target are deduplicated in list order, so `cursor` and `acp:cursor` provide one fallback and preserve whichever spelling appears first.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.
This per-repo `agent` value, including every fallback entry, is still read from the trusted default-branch `.no-mistakes.yaml` unless `allow_repo_commands` is enabled there.

### allow_repo_commands

Opt in to honoring the code-executing selection fields (`commands.{test,lint,format}` and `agent`) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

This field is itself read **only from the trusted default-branch copy** of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch. By default the daemon reads `commands` and `agent` from your default branch (e.g. `origin/main`) so a pushed SHA cannot inject shell or pick the launched agent on the daemon host. Leave this `false` for any repo that accepts contributions. Set it to `true` only for a single-developer environment where you trust every branch you push (for example, a personal repo gated by your own daemon).

### disable_project_settings

Suppress project-level agent settings and instructions for every gate-agent start and resumed session.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

This opt-in is intended for agent-orchestration repositories whose `AGENTS.md`, `CLAUDE.md`, or harness-specific project settings would give a validation agent an operator identity and authority that it must not adopt.
When enabled, no-mistakes suppresses the target checkout's project settings for every agent-driven gate step while preserving user-level agent configuration.
Codex, Claude, and OpenCode are the currently verified agents: Codex receives `project_doc_max_bytes=0` and `--ignore-rules`, Claude loads only its user setting source, and OpenCode is launched with `--no-project-instructions` and `--pure` (which require an OpenCode binary that supports the `--no-project-instructions` serve flag; an older binary is rejected with an upgrade diagnostic).
The setting applies to both new and resumed sessions.

The gate fails before launching an agent if any resolved agent or fallback lacks a verified suppression mechanism.
It also fails if `agent_args_override` defeats suppression, such as a nonzero Codex `project_doc_max_bytes` or Claude setting sources that include `project` or `local`.
OpenCode's managed flags are appended last so yargs last-wins enforces them, so `agent_args_override` cannot defeat its suppression.
When this option is `false`, missing, or `null`, all agents retain their existing project-setting behavior.

This field is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of `allow_repo_commands`.
A pushed branch cannot enable it or disable a trusted opt-in.
If the trusted commit or its present config file cannot be read and parsed, the run aborts rather than guessing that the option is disabled.

### commands.test

Explicit **targeted** local test command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent selects the smallest relevant tests and evidence checks) |

`commands.test` is local **targeted validation** of the change and requested intent, not a CI-parity repository-wide regression command.
Broad regression belongs in remote CI and remains mandatory before a PR is ready; do not put a complete-suite walk here just to mirror CI.
no-mistakes does not guess whether an arbitrary shell string is "too broad" - the contract is documented and dogfooded, not enforced with language- or filename-specific heuristics.

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the agent detects and runs the smallest relevant tests itself (and is instructed never to run the complete repository suite).
When user intent is available, the agent may still run after a successful baseline command to gather evidence-oriented validation, still under the same targeted-validation contract.

### commands.lint

Explicit lint command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the agent-driven lint duty is folded into the document step's combined housekeeping pass: one agent invocation covers both documentation and lint, and the lint step consumes that result, reporting lint-category findings with the same gate semantics (blocking findings park for a decision).
Neither responsibility is skipped: when the document step has nothing to run against (or its structured output cannot be trusted), the lint step runs its own agent pass as before.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
|---|---|
| Type | `string` |
| Default | Empty (no separate push-step formatter) |

This does not prevent empty `commands.lint` from detecting and running formatters during the combined housekeeping pass, or during the lint step when that pass cannot provide a result.

### document.instructions

Repository-specific documentation ownership policy for the document step.

| | |
|---|---|
| Type | `string` (multiline) |
| Default | Empty (built-in placement policy only) |

The document step always applies a built-in placement policy: every fact has exactly one authoritative owner document, stale duplicates are removed or reduced to pointers instead of synchronized, no new documentation surfaces are created merely to close perceived gaps, and incident lessons live as invariants near their owner (with a pointer to the regression test), never as AGENTS.md postmortems.
`document.instructions` states this repository's ownership map or extra placement rules (for example, which file owns which class of facts).
It augments or clarifies the built-in policy; it cannot disable documentation integrity.

Like `commands.*` and `agent`, this field steers gate behavior, so it is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`: a contributor's pushed branch cannot weaken the documentation rules that gate its own review.

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
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings pause for approval instead of starting another automatic fix loop.

`auto_fix.ci` covers the CI step's CI failure and merge-conflict auto-fix attempts.

Legacy alias: `auto_fix.babysit`.

### commit.fix_message

Override the auto-fix commit subject template for this repository.

| | |
|---|---|
| Type | `string` |
| Default | Inherits from global config, whose default is `no-mistakes({{.Step}}): {{.Summary}}` |

The value follows the [global `commit.fix_message` template syntax and validation rules](/no-mistakes/reference/global-config/#commitfix_message).
That includes the 1,024-byte template limit, 16-placeholder limit, 4,096-byte summary and rendered-subject limits, and rejection of bidi and invisible Unicode format characters.
The setting applies to the Review, Test, Document, and Lint fix path, not commits created by the Rebase, CI, or Push steps.

This non-executing field is read from the pushed branch, so a branch can adopt its own commit convention without enabling `allow_repo_commands`.

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
