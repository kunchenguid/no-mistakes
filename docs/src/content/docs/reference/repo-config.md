---
title: Repo config reference
description: All fields for .no-mistakes.yaml.
---

Per-repo configuration lives in `.no-mistakes.yaml` at the root of your repository.

:::caution[Security: commands are read from the default branch]
`commands.*` execute arbitrary shell on the daemon host via `sh -c` (or `cmd.exe /c` on Windows) with the maintainer's credentials.
To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads `commands` from your default branch (for example `origin/main`), never from the pushed SHA.
It reads them at the exact commit a fresh fetch resolved, so a stale `origin/<default>` ref cannot serve a value the live default branch removed.
If the fetch fails, `commands` is forced empty and the run proceeds on built-in defaults rather than falling back to a potentially stale or hostile copy.
Commit the `commands` you want the gate to run to your default branch.
Route overrides under `routes` and documentation placement rules under `document.instructions` are also read only from the default branch.
Non-executing fields (`ignore_patterns`, `intent`, and `test`) are still read from the pushed branch.

If you genuinely want per-branch `commands` (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch.
The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.
:::

```yaml
# .no-mistakes.yaml

commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

intent:
  enabled: true

test:
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence

document:
  instructions: |
    README.md owns the user-facing introduction.
    docs/reference/ owns detailed contracts.

routes:
  documentation_authoring: fix_balanced
```

Parsing is strict.
An unknown key is a load error, and a key that tries to select an agent or define execution mechanics fails with actionable guidance.
See [keys a repository may not define](#keys-a-repository-may-not-define) below.

## Fields

### commands.test

Explicit test command.
Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (the routed agent auto-detects tests and evidence checks) |

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the routed agent detects and runs relevant tests itself.
When user intent is available, the agent may still run after a successful baseline command to gather evidence-oriented validation.

### commands.lint

Explicit lint command.
Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (Document combines lint with documentation housekeeping) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the Document authoring pass also detects relevant linters and formatters, applies safe fixes, reruns the checks, and categorizes unresolved lint findings for the Lint gate.
Lint consumes that result once.
If no combined result is available, Lint runs a standalone agent pass rather than skipping lint.
Malformed combined author output fails Document, and a present malformed lint result fails Lint instead of triggering the fallback.

### commands.format

Formatter command run at the start of the Lint step before it commits agent fixes.

| | |
|---|---|
| Type | `string` |
| Default | Empty (no separate push-step formatter) |

This does not prevent empty `commands.lint` from detecting and running formatters during the combined housekeeping pass or Lint's standalone fallback.

### allow_repo_commands

Opt in to honoring the code-executing selection fields (`commands.{test,lint,format}`) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

This field is itself read only from the trusted default-branch copy of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch.
By default the daemon reads `commands` from your default branch so a pushed SHA cannot inject shell on the daemon host.
Leave this `false` for any repo that accepts contributions.
Set it to `true` only for a single-developer environment where you trust every branch you push, for example a personal repo gated by your own daemon.

`allow_repo_commands` never affects `routes` or `document.instructions`: both always come from the trusted default-branch copy.

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

### document.instructions

Repository-specific ownership and placement rules for the Document step.

| | |
|---|---|
| Type | `string` |
| Default | Empty (the built-in placement policy applies) |

Use this field to name the authoritative owner for repository-specific facts.
For example:

```yaml
document:
  instructions: |
    README.md owns the quickstart.
    docs/reference/api.md owns the public API contract.
```

These instructions augment the built-in policy and cannot weaken it.
The built-in policy requires one authoritative owner per fact.
It removes stale duplicate prose or reduces it to a pointer instead of synchronizing copies.
It also limits the pass to documentation the change made stale.

`document.instructions` always comes from the trusted default-branch copy of `.no-mistakes.yaml`.
A pushed branch cannot change the rules used to gate its own documentation, even when `allow_repo_commands` is `true`.
If no trusted copy is available, pushed-branch instructions are discarded and the built-in policy remains active.
A malformed trusted config is a load error and stops the run.

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
Set `store_in_repo: true` to write evidence under `<dir>/<branch-slug>` inside the worktree so Test can stage and commit it before Push transports it with the branch.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.

### routes

Map a registered purpose to the name of one existing global profile.

| | |
|---|---|
| Type | `map[purpose]profile-name` |
| Default | Empty (the global routing contract applies unchanged) |

Example:

```yaml
routes:
  documentation_authoring: fix_balanced
  initial_review: authority_strong
```

This is the only routing surface a repository has:

- each override maps one purpose to exactly one profile that must already exist in the effective global contract
- applying an override replaces that purpose's complete global cascade with a one-element route
- the resolved contract is validated before any model launch, so a reference to a missing profile fails the run closed
- overrides are read only from the trusted default-branch copy of `.no-mistakes.yaml`, never from a pushed branch, and `allow_repo_commands` does not change that

The registered purposes and global profiles are listed in the [routing reference](/no-mistakes/reference/routing/).

## Keys a repository may not define

A repository can never select an agent or define model-selection execution mechanics.
Runners, profiles, and candidates are owned exclusively by global configuration.

A repo config that sets one of these keys fails to load with:

```
repo config may not define "<key>": <guidance>
```

The guidance column quotes the emitted guidance text exactly.

| Rejected key | Guidance |
|---|---|
| `agent` | model selection is global-only through the routing contract; a repository cannot select an agent |
| `agent_args_override` | native agent arguments are derived from global routing profile candidates and cannot be overridden by a repository |
| `agent_path_override` | runner executables are configured globally via `routing.runners.<name>.executable` |
| `acp_registry_overrides` | acp agents were removed; repositories cannot define runner commands |
| `acpx_path` | acp agents were removed; repositories cannot define runner executables |
| `auto_fix` | per-step numeric auto-fix limits were removed; repair escalates through the routing cascade |
| `candidates` | candidates are owned exclusively by global configuration |
| `fallback_agents` | provider fail-over is configured through global routing profile candidates, not a repository fallback-agent list |
| `profiles` | profiles are owned exclusively by global configuration |
| `routing` | repositories may only set 'routes' mapping purposes to existing global profiles |
| `runners` | runners are owned exclusively by global configuration |

See [migrating to routing](/no-mistakes/guides/migrating-to-routing/) for what to put in place of removed keys.
