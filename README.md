<h1 align="center">no-mistakes</h1>
<p align="center">
  <a href="https://github.com/kunchenguid/no-mistakes/actions/workflows/ci.yml"
    ><img
      alt="CI"
      src="https://img.shields.io/github/actions/workflow/status/kunchenguid/no-mistakes/ci.yml?style=flat-square&label=ci"
  /></a>
  <a href="https://github.com/kunchenguid/no-mistakes/actions/workflows/release.yml"
    ><img
      alt="Release"
      src="https://img.shields.io/github/actions/workflow/status/kunchenguid/no-mistakes/release.yml?style=flat-square&label=release"
  /></a>
  <a href="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square"
    ><img
      alt="Platform"
      src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square"
  /></a>
  <a href="https://x.com/kunchenguid"
    ><img
      alt="X"
      src="https://img.shields.io/badge/X-@kunchenguid-black?style=flat-square"
  /></a>
  <a href="https://discord.gg/Wsy2NpnZDu"
    ><img
      alt="Discord"
      src="https://img.shields.io/discord/1439901831038763092?style=flat-square&label=discord"
  /></a>
</p>

<h3 align="center">Make <code>git push</code> earn it.</h3>

You already have CI, but it usually wakes up after the branch is upstream. You already have review, but that also tends to happen after the push. That is backwards if the goal is to stop bad code before it escapes.

`no-mistakes` puts a local gate in front of your real remote. You push to `no-mistakes`, it spins up a disposable worktree, runs a fixed validation pipeline with your agent of choice, then forwards upstream only after the branch survives the checks.

- **Push with intent** - `origin` stays untouched, and `no-mistakes` becomes the explicit path for gated pushes.
- **Agent-agnostic** - use `claude`, `codex`, `rovodev`, or `opencode`, with per-repo overrides if different codebases want different tools.
- **Human stays in charge** - review, test, document, lint, PR, and CI steps can pause for approval instead of auto-shipping surprises.

## Quick Start

```sh
$ no-mistakes init
initialized gate for /Users/you/src/my-repo
  remote: no-mistakes -> /Users/you/.no-mistakes/repos/abc123def456.git
  upstream: git@github.com:you/my-repo.git

Push through the gate with: git push no-mistakes <branch>

$ git push no-mistakes feature/login-fix
remote: no-mistakes: pipeline started. Run `no-mistakes` to review.

$ no-mistakes
# opens the TUI for the active run in this repo
```

## Install

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.ps1 | iex
```

**Go install**

```sh
go install github.com/kunchenguid/no-mistakes/cmd/no-mistakes@latest
```

**From source**

```sh
git clone git@github.com:kunchenguid/no-mistakes.git
cd no-mistakes
make build
make install
```

You will also need `git`, one supported agent binary, and `gh` if you want PR creation and CI monitoring.

To update an existing install in place:

```sh
no-mistakes update
```

## How It Works

```text
┌──────────────┐        git push no-mistakes <branch>        ┌─────────────────────┐
│ Your repo    │ ─────────────────────────────────────────► │ Local gate repo      │
│ origin       │                                            │ ~/.no-mistakes/...   │
│ no-mistakes  │ ◄──────────── added by init ────────────── │ hooks/post-receive   │
└──────┬───────┘                                            └──────────┬──────────┘
       │                                                               │
       │                                         notifies daemon        │
       │                                                               ▼
       │                                                    ┌─────────────────────┐
       │                                                    │ Daemon              │
       │                                                    │ SQLite + Unix socket│
       │                                                    └──────────┬──────────┘
       │                                                               │
       │                                                creates detached worktree
       │                                                               ▼
       │                                                    ┌─────────────────────┐
       │                                                    │ Pipeline            │
       │                                                    │ review              │
       │                                                    │ test                │
       │                                                    │ document            │
       │                                                    │ lint                │
       │                                                    │ push                │
       │                                                    │ pr                  │
       │                                                    │ ci                  │
       │                                                    └──────────┬──────────┘
       │                                                               │
       └──────────────────────────────────────────────────────────────► │ upstream
                                                                        └──────────
```

- **Named remote** - `origin` is never hijacked. If you want the gate, you push to `no-mistakes` on purpose.
- **Disposable worktrees** - each run happens in its own detached worktree, so the daemon can inspect and modify safely before pushing upstream.
- **Fixed pipeline** - this is opinionated on purpose: `review -> test -> document -> lint -> push -> pr -> ci`. The `document` step checks whether README/docs/comments need updates for the code you changed.
- **Local state** - metadata lives under `~/.no-mistakes/` by default, or `${NM_HOME}` if you want to relocate it.

## CLI Reference

| Command                     | Description                                            |
| --------------------------- | ------------------------------------------------------ |
| `no-mistakes`               | Attach to the active pipeline run for the current repo |
| `no-mistakes init`          | Initialize the gate for the current repository         |
| `no-mistakes update`        | Update the installed binary from GitHub Releases       |
| `no-mistakes eject`         | Remove the gate from the current repository            |
| `no-mistakes attach`        | Attach to the active pipeline run                      |
| `no-mistakes rerun`         | Rerun the pipeline for the current branch              |
| `no-mistakes status`        | Show repo, daemon, and active run status               |
| `no-mistakes runs`          | List recorded pipeline runs for the current repo       |
| `no-mistakes doctor`        | Check system health and dependencies                   |
| `no-mistakes daemon start`  | Start the daemon in the background                     |
| `no-mistakes daemon stop`   | Stop the running daemon                                |
| `no-mistakes daemon status` | Check whether the daemon is running                    |

### Flags

| Command  | Flag          | Description                       |
| -------- | ------------- | --------------------------------- |
| `attach` | `--run <id>`  | Attach to a specific run ID       |
| `runs`   | `--limit <n>` | Maximum number of runs to display |

## Configuration

Config is optional and split across two files:

- Global: `~/.no-mistakes/config.yaml`
- Repo-local: `<repo>/.no-mistakes.yaml`
- Home override: set `NM_HOME` and the global file becomes `${NM_HOME}/config.yaml`

### Global config

```yaml
# ~/.no-mistakes/config.yaml

# Default agent for all repos.
agent: claude # claude | codex | rovodev | opencode

# Optional binary path overrides.
agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode

# How long the CI step monitors GitHub checks before timing out.
ci_timeout: "4h"

# Optional auto-fix attempt limits per step (0 = require approval).
# auto_fix:
#   rebase: 0
#   lint: 3
#   test: 3
#   review: 3
#   document: 3
#   ci: 3

# debug | info | warn | error
log_level: "info"

# Maximum auto-fix attempts per step (0 = disabled, requires manual approval)
auto_fix:
  rebase: 0
  lint: 3
  test: 3
  review: 3
  document: 0
  ci: 3
```

### Repo config

```yaml
# .no-mistakes.yaml

# Optional override for this repo only.
agent: codex

commands:
  # If set, use this exact lint command.
  lint: "golangci-lint run ./..."

  # If set, use this exact test command.
  test: "go test -race ./..."

  # Optional formatter run before the push step commits agent fixes.
  format: "gofmt -w ."

# Ignore these paths during review and documentation checks.
ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

# Optional per-repo auto-fix overrides.
auto_fix:
  review: 3
  document: 0
```

### Precedence and defaults

- Repo `agent` overrides global `agent`.
- `commands` and `ignore_patterns` are repo-only.
- Missing global config defaults to `agent: claude`, `ci_timeout: 4h`, `log_level: info`.
- `ci_timeout` replaces `babysit_timeout`, and `auto_fix.ci` replaces `auto_fix.babysit`; legacy keys are still accepted for existing configs.
- `auto_fix` can be set globally or per repo. All steps default to `3`.
- `agent_path_override` changes which binary path is launched for a given agent.
- Default binaries are `claude`, `codex`, `acli` for `rovodev`, and `opencode`.
- If `commands.test` is empty, the agent detects and runs relevant tests itself.
- If `commands.lint` is empty, the agent detects and runs lint/format checks itself.
- If `commands.format` is empty, no formatter is run automatically.
- All `auto_fix` steps default to `3`. Set a step to `0` to require manual approval.

### Ignore pattern rules

- `*.generated.go` matches by basename.
- `vendor/**` matches an entire subtree.
- Patterns containing a slash use full-path glob matching.

## Development

```sh
make build   # Build bin/no-mistakes with version info
make dist    # Cross-compile release archives into dist/
make install # Install the built binary into GOPATH/bin
make test    # Run go test -race ./...
make lint    # Run go vet ./...
make fmt     # Run gofmt -w .
make clean   # Remove bin/
```
