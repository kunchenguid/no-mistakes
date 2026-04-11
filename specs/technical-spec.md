# no-mistakes: Implementation Plan

## Context

Rebuild of [airlock-hq/airlock](~/github/airlock-hq/airlock) from scratch in Go. Airlock is a local Git proxy that intercepts pushes and runs code through a validation pipeline before forwarding upstream. no-mistakes keeps the core concept but makes key design changes that dramatically simplify the architecture.

**Key design changes from Airlock:**

1. Named remote (`no-mistakes`) instead of hijacking `origin` — user explicitly chooses to push through the gate
2. Go instead of Rust
3. CLI + TUI instead of Tauri desktop app
4. Fixed opinionated pipeline instead of configurable CI-like DAG
5. Adds "babysit PR" step (monitor CI + PR comments, agent fixes them)

---

## Architecture Overview

```
User's Repo                              no-mistakes System
───────────                              ──────────────────
.git/config:                             ~/.no-mistakes/
  origin → github.com/user/repo           state.sqlite
  no-mistakes → ~/.no-mistakes/            socket
                  repos/<id>.git           repos/<id>.git/
                                             hooks/post-receive
                                           worktrees/<id>/<run-id>/
                                           logs/<run-id>/

git push no-mistakes feature
  │
  ├─► bare repo accepts refs
  │     post-receive hook notifies daemon via Unix socket
  │     prints: "no-mistakes: pipeline started. Run `no-mistakes` to review."
  │
  └─► daemon:
        1. cancel any active run for same repo+branch
        2. create run record in SQLite
        3. git worktree add --detach (from gate bare repo)
        4. execute pipeline steps sequentially in worktree
        5. pause at approval points, wait for TUI interaction
        6. on "push" step: push from worktree to upstream URL
        7. on "PR" step: create/update PR via gh CLI
        8. on "babysit" step: poll CI + comments, fix loop
        9. cleanup worktree
```

### What Airlock complexity we eliminate

| Airlock complexity                                     | no-mistakes                        | Why eliminated                   |
| ------------------------------------------------------ | ---------------------------------- | -------------------------------- |
| Remote rewiring (origin → bypass-airlock → new origin) | Add one `no-mistakes` remote       | Origin untouched                 |
| Upload-pack wrapper to intercept `git fetch`           | None                               | Fetches go to real origin        |
| Bidirectional sync (gate ↔ upstream)                   | None                               | Gate is receive-only             |
| Push coalescing / debouncing                           | Cancel-and-replace                 | Explicit pushes are infrequent   |
| Branch repointing                                      | None                               | Tracking branches stay on origin |
| Lock management for sync                               | None                               | No sync                          |
| Worktree pool (8 slots)                                | Single worktree per run            | No concurrent runs needed        |
| Workflow YAML parsing + DAG resolution                 | Fixed 6-step sequence              | Opinionated pipeline             |
| Tauri + React desktop app                              | Bubbletea TUI                      | Single binary                    |
| Artifact system (content/comments/patches)             | Step results in SQLite + log files | Fixed pipeline, known outputs    |
| 3 hooks (pre-receive, post-receive, upload-pack)       | 1 hook (post-receive)              | No fetch interception            |

---

## Tech Stack

| Area          | Choice                                        | Rationale                               |
| ------------- | --------------------------------------------- | --------------------------------------- |
| Language      | Go 1.23+                                      | User requirement                        |
| CLI framework | cobra                                         | De facto standard, nested subcommands   |
| TUI           | bubbletea + lipgloss + bubbles                | Best Go TUI ecosystem                   |
| Daemon        | Self-exec pattern (NM_DAEMON=1 env var)       | Single binary, gopls-proven             |
| IPC           | JSON-RPC over Unix socket                     | Simplest, stdlib-friendly               |
| Database      | modernc.org/sqlite                            | Pure Go, no CGO, cross-compiles         |
| Git ops       | Shell out to `git` CLI                        | Simplest, most compatible               |
| Agents        | 4 adapters: Claude, Codex, Rovo Dev, OpenCode | Same as gnhf, agent-agnostic            |
| Diff parsing  | bluekeyes/go-gitdiff                          | Parse `git diff` output for TUI display |
| IDs           | oklog/ulid                                    | Sortable, no coordination               |
| Config        | gopkg.in/yaml.v3                              | Standard YAML                           |
| Logging       | log/slog (stdlib)                             | Structured, zero deps                   |

---

## Project Structure

```
no-mistakes/
  go.mod
  go.sum
  Makefile
  CLAUDE.md
  cmd/
    no-mistakes/
      main.go                    # Entry: daemon mode (NM_DAEMON=1) or CLI mode
  internal/
    buildinfo/version.go         # Version/Commit/Date via ldflags
    paths/paths.go               # ~/.no-mistakes/* path constants
    types/types.go               # Shared domain types (RunStatus, StepName, etc.)
    config/config.go             # Global + per-repo config loading
    db/
      db.go                      # Open, migrate, close
      schema.go                  # SQL DDL
      repo.go                    # Repo CRUD
      run.go                     # Run CRUD
      step.go                    # StepResult CRUD
    git/
      git.go                     # Shell-out wrappers (diff, log, push, worktree)
      hook.go                    # Post-receive hook script template
    agent/
      agent.go                   # Agent interface + factory
      claude.go                  # Claude Code adapter (spawn `claude` CLI, JSONL stream)
      codex.go                   # Codex adapter (spawn `codex exec`, JSONL stream)
      rovodev.go                 # Rovo Dev adapter (spawn `acli rovodev serve`, HTTP+SSE)
      opencode.go                # OpenCode adapter (spawn `opencode serve`, HTTP+SSE)
    gate/
      gate.go                    # Create bare repo, add remote, install hook, eject
    pipeline/
      pipeline.go                # Fixed step sequence definition
      executor.go                # Sequential step execution + approval coordination
      steps/
        review.go                # Code review + doc gap audit
        test.go                  # Run/write tests
        lint.go                  # Run linters + agent fix
        push.go                  # Force-push to upstream
        pr.go                    # Create/update PR
        babysit.go               # Monitor CI + PR comments, fix loop
    ipc/
      protocol.go                # JSON-RPC types (shared between client/server)
      server.go                  # Unix socket listener + dispatch
      client.go                  # Connect to daemon, send requests
    daemon/
      daemon.go                  # Startup, signal handling, shutdown
      selfexec.go                # Fork daemon via os/exec + env var
    cli/
      root.go                    # Root cobra command
      init.go                    # no-mistakes init
      eject.go                   # no-mistakes eject
      attach.go                  # no-mistakes (attach to latest run TUI)
      status.go                  # no-mistakes status
      runs.go                    # no-mistakes runs
      daemon_cmd.go              # no-mistakes daemon start/stop/status
      doctor.go                  # no-mistakes doctor
    tui/
      app.go                     # Root bubbletea model
      pipeline.go                # Pipeline progress view (step list + statuses)
      review.go                  # Findings review + action selection
      diff.go                    # Scrollable diff viewer
      babysit.go                 # Babysit monitor view
```

---

## Gate Design (Simplified)

### Init flow (`no-mistakes init`)

1. Find `.git` in cwd (walk up)
2. Read origin URL: `git remote get-url origin`
3. Generate repo ID: `sha256(absolute_working_path)[:12]`
4. Create bare repo: `git init --bare ~/.no-mistakes/repos/<id>.git`
5. Install post-receive hook in bare repo (the only hook)
6. Add remote in user's repo: `git remote add no-mistakes ~/.no-mistakes/repos/<id>.git`
7. Insert repo record in SQLite
8. Ensure daemon is running (auto-start if not)

### Post-receive hook (the only hook)

```sh
#!/bin/sh
# Notify daemon of push. Non-blocking — push always succeeds.
SOCKET="$HOME/.no-mistakes/socket"
while read oldrev newrev refname; do
  if [ -S "$SOCKET" ]; then
    printf '{"method":"push_received","params":{"gate":"%s","ref":"%s","old":"%s","new":"%s"}}\n' \
      "$(pwd)" "$refname" "$oldrev" "$newrev" | nc -U "$SOCKET" || true
  fi
done
echo "no-mistakes: pipeline started. Run \`no-mistakes\` to review." >&2
exit 0
```

### Eject flow (`no-mistakes eject`)

1. `git remote remove no-mistakes` (in user's repo)
2. Delete bare repo at `~/.no-mistakes/repos/<id>.git`
3. Delete any worktrees
4. Remove repo record from SQLite (cascade deletes runs + steps)

---

## Pipeline Design

### Fixed sequence (not configurable)

```
Step 1: review    — Agent reviews diff for bugs, risks, doc gaps
Step 2: test      — Agent runs/writes tests
Step 3: lint      — Run linters, agent fixes issues
Step 4: push      — Force-push to upstream (--force-with-lease)
Step 5: pr        — Agent creates/updates PR with description
Step 6: babysit   — Monitor CI + PR comments, agent fix loop
```

### Step interaction model

Each step follows this state machine:

```
pending → running → [NeedsApproval?]
                      ├─ no → completed → next step
                      └─ yes → awaiting_approval
                                  ├─ [approve] → completed → next step
                                  ├─ [fix] → fixing → fix_review → [approve/fix again]
                                  ├─ [skip] → skipped → next step
                                  └─ [abort] → failed → pipeline aborted
```

### Which steps pause for approval

| Step    | Pauses?     | When                                                                             |
| ------- | ----------- | -------------------------------------------------------------------------------- |
| review  | Conditional | Only if findings have severity=error or severity=warning                         |
| test    | Conditional | Only if tests fail or agent wrote new tests                                      |
| lint    | Conditional | Only if lint errors found + agent proposes fixes                                 |
| push    | Never       | Auto-proceeds                                                                    |
| pr      | Never       | Auto-creates/updates PR, no human review needed                                  |
| babysit | Split       | CI failures: auto-fix (no approval). PR comments: human selects which to address |

### Fix loop

When user selects "fix":

1. Agent runs with edit permissions in the worktree
2. Agent makes changes, daemon computes `git diff`
3. TUI shows the fix diff for review
4. User can: approve (accept fix, continue) or fix again (re-validate → new findings → re-fix)

### Babysit step (novel)

After PR creation, enters a polling loop with two distinct behaviors:

**CI failures — auto-fix (no human approval):**

1. Detect CI failure via `gh pr checks <number>`
2. Fetch failure logs
3. Agent automatically diagnoses + fixes + commits + pushes
4. TUI shows what happened (informational, not blocking)
5. Loop continues polling for new CI results

**PR comments — human selects:**

1. Detect new PR comments from other users
2. Show all comments in TUI
3. User selects which comments are worth addressing (checkbox selection)
4. Agent addresses selected comments → commits → pushes
5. Daemon posts reply to addressed comments: "Addressed in {sha}"

**Exit conditions:** user says done, PR merged, PR closed, or timeout (4hr default)

Poll interval: 30s for first 5min, 60s for 5-15min, 120s after. Resets on new push.

---

## Daemon Design

### Single binary, self-exec

```go
// cmd/no-mistakes/main.go
func main() {
    if os.Getenv("NM_DAEMON") == "1" {
        daemon.Run()  // blocks until shutdown
        return
    }
    cli.Execute()  // cobra
}
```

### Daemon start (`no-mistakes daemon start`)

1. Check liveness: connect to `~/.no-mistakes/socket`, send health check
2. If alive, print "already running" and exit
3. Re-exec self: `os/exec.Command(os.Executable())` with `NM_DAEMON=1`, `Setsid: true`, stdout/stderr → `~/.no-mistakes/logs/daemon.log`
4. Write PID to `~/.no-mistakes/daemon.pid`
5. Poll socket for up to 5s to confirm startup

### Auto-start

Any CLI command that needs the daemon calls `ipc.EnsureDaemon()` which auto-starts if not running. The post-receive hook also prints a nudge but doesn't block on daemon availability.

### IPC methods

```
push_received(gate, ref, old, new)    — from hook
get_run(run_id) → Run                 — from CLI
get_runs(repo_id) → []Run             — from CLI
get_active_run(repo_id) → Run|nil     — from CLI/TUI
subscribe(run_id) → event stream      — from TUI
respond(run_id, step, action) → ok    — from TUI (approve/fix/skip/abort)
health() → ok                         — liveness check
shutdown() → ok                       — graceful stop
```

### Attach/detach model

- TUI calls `get_active_run` to find current run, then `subscribe` for live updates
- If terminal closes, daemon continues running; pipeline pauses at next approval point
- User re-attaches by running `no-mistakes` again
- If no active run, `no-mistakes` shows status/history

---

## Database Schema

```sql
CREATE TABLE repos (
    id            TEXT PRIMARY KEY,
    working_path  TEXT NOT NULL UNIQUE,
    upstream_url  TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at    INTEGER NOT NULL
);

CREATE TABLE runs (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch     TEXT NOT NULL,
    head_sha   TEXT NOT NULL,
    base_sha   TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    pr_url     TEXT,
    error      TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE step_results (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name     TEXT NOT NULL,
    step_order    INTEGER NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    exit_code     INTEGER,
    duration_ms   INTEGER,
    log_path      TEXT,
    findings_json TEXT,
    error         TEXT,
    started_at    INTEGER,
    completed_at  INTEGER
);
```

---

## Multi-Agent Design

Port the adapter pattern from [gnhf](~/github/kunchenguid/gnhf) (Go port of the TypeScript originals).

### Agent interface

```go
type Agent interface {
    Name() string
    Run(ctx context.Context, opts RunOpts) (*Result, error)
    Close() error  // cleanup for server-based agents (rovodev, opencode)
}

type RunOpts struct {
    Prompt     string
    CWD        string
    JSONSchema json.RawMessage  // for structured output
    OnChunk    func(text string) // streaming callback
    LogPath    string            // write raw events to file
}

type Result struct {
    Output json.RawMessage  // parsed structured output (matches JSONSchema)
    Text   string           // raw text output
    Usage  TokenUsage
}
```

### Four adapters (same as gnhf)

| Agent        | Binary     | Invocation pattern                                                   | Streaming       |
| ------------ | ---------- | -------------------------------------------------------------------- | --------------- |
| **Claude**   | `claude`   | `claude -p "<prompt>" --output-format stream-json --json-schema ...` | JSONL on stdout |
| **Codex**    | `codex`    | `codex exec "<prompt>" --json --output-schema ...`                   | JSONL on stdout |
| **Rovo Dev** | `acli`     | Start HTTP server (`acli rovodev serve`), send via REST, stream SSE  | HTTP SSE        |
| **OpenCode** | `opencode` | Start HTTP server (`opencode serve`), send via REST, stream SSE      | HTTP SSE        |

Claude and Codex are spawn-per-invocation. Rovo Dev and OpenCode start a persistent HTTP server (reused across step invocations within a run, closed at end of run).

### Agent selection

Configured globally in `~/.no-mistakes/config.yaml` with `agent` field. Can be overridden per-repo in `.no-mistakes.yaml`. Binary paths can be overridden via `agent_path_override` map.

---

## Configuration

### Global: `~/.no-mistakes/config.yaml` (optional)

```yaml
agent: claude # claude | codex | rovodev | opencode
agent_path_override: # custom binary paths (optional)
  claude: ~/bin/claude
  codex: /usr/local/bin/codex
babysit_timeout: "4h"
log_level: "info"
```

No lint/test/format commands at global level — these are repo-specific.

### Per-repo: `.no-mistakes.yaml` in repo root (optional)

```yaml
agent: codex # override agent for this repo (optional)

commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"
```

All commands are optional. If empty, the agent auto-detects how to lint/test/format the project.

---

## Implementation Phases

### Phase 1: Foundation

- `go.mod`, `main.go`, `Makefile`, `CLAUDE.md`
- `buildinfo`, `paths`, `types`
- `db/` — schema, migrations, CRUD
- `config/` — load global + per-repo YAML
- **Ref**: Airlock DB schema at `~/github/airlock-hq/airlock/crates/airlock-core/src/db/schema.rs`
- **Ref**: Airlock paths at `~/github/airlock-hq/airlock/crates/airlock-core/src/paths.rs`

### Phase 2: Gate + Git

- `gate/` — init (bare repo, hook, remote), eject
- `git/` — diff, log, worktree add/remove, push
- `cli/init.go`, `cli/eject.go`
- **Ref**: Airlock init/eject at `~/github/airlock-hq/airlock/crates/airlock-core/src/init.rs`
- **Ref**: Airlock hook templates at `~/github/airlock-hq/airlock/crates/airlock-core/src/git/hooks.rs`
- **Ref**: Airlock remote management at `~/github/airlock-hq/airlock/crates/airlock-core/src/git/remote.rs`
- **Ref**: Airlock worktree ops at `~/github/airlock-hq/airlock/crates/airlock-core/src/worktree.rs`
- **Ref**: Airlock push (force-with-lease) at `~/github/airlock-hq/airlock/crates/airlock-core/src/git/push.rs`

### Phase 3: Daemon + IPC

- `ipc/protocol.go` — JSON-RPC types
- `ipc/server.go` — Unix socket listener
- `ipc/client.go` — connect + send
- `daemon/` — self-exec, startup, shutdown, signal handling
- `cli/daemon_cmd.go` — start/stop/status
- Post-receive hook integration (hook → daemon notification)
- **Ref**: Airlock IPC types at `~/github/airlock-hq/airlock/crates/airlock-core/src/ipc/`
- **Ref**: Airlock daemon server at `~/github/airlock-hq/airlock/crates/airlock-daemon/src/server.rs`
- **Ref**: Airlock push handlers at `~/github/airlock-hq/airlock/crates/airlock-daemon/src/handlers/push.rs`

### Phase 4: Pipeline Core + Agents

- `agent/agent.go` — Agent interface + factory
- `agent/claude.go`, `codex.go`, `rovodev.go`, `opencode.go` — 4 adapters (port from gnhf)
- `pipeline/pipeline.go` — step sequence definition
- `pipeline/executor.go` — sequential execution, approval coordination
- Step implementations: `review.go`, `test.go`, `lint.go`, `push.go`, `pr.go`
- **Ref**: gnhf agent interface at `~/github/kunchenguid/gnhf/src/core/agents/types.ts`
- **Ref**: gnhf agent factory at `~/github/kunchenguid/gnhf/src/core/agents/factory.ts`
- **Ref**: gnhf Claude adapter at `~/github/kunchenguid/gnhf/src/core/agents/claude.ts`
- **Ref**: gnhf Codex adapter at `~/github/kunchenguid/gnhf/src/core/agents/codex.ts`
- **Ref**: gnhf Rovo Dev adapter at `~/github/kunchenguid/gnhf/src/core/agents/rovodev.ts`
- **Ref**: gnhf OpenCode adapter at `~/github/kunchenguid/gnhf/src/core/agents/opencode.ts`
- **Ref**: gnhf stream utilities at `~/github/kunchenguid/gnhf/src/core/agents/stream-utils.ts`
- **Ref**: gnhf process management at `~/github/kunchenguid/gnhf/src/core/agents/managed-process.ts`
- **Ref**: Airlock pipeline executor at `~/github/airlock-hq/airlock/crates/airlock-daemon/src/pipeline/executor.rs`
- **Ref**: Airlock default steps at `~/github/airlock-hq/airlock/defaults/` (review=critique, test, lint, push, create-pr, describe)
- **Ref**: Airlock run queue at `~/github/airlock-hq/airlock/crates/airlock-daemon/src/run_queue.rs`

### Phase 5: TUI

- `tui/app.go` — root model, subscribe to daemon events
- `tui/pipeline.go` — step progress list
- `tui/review.go` — findings display + action selection
- `tui/diff.go` — scrollable diff viewer
- `cli/attach.go` — attach to run, launch TUI

### Phase 6: Babysit

- `pipeline/steps/babysit.go` — CI polling, comment detection, fix loop
- `tui/babysit.go` — babysit-specific view
- **Ref**: Airlock create-pr step (gh/glab detection) at `~/github/airlock-hq/airlock/defaults/create-pr/step.yml`

### Phase 7: Polish

- `cli/status.go`, `cli/runs.go`, `cli/doctor.go`
- Error recovery (daemon crash → resume paused pipelines)
- Logging, error messages, edge cases
- **Ref**: Airlock doctor at `~/github/airlock-hq/airlock/crates/airlock-cli/src/commands/doctor.rs`
- **Ref**: Airlock daemon crash recovery at `~/github/airlock-hq/airlock/crates/airlock-daemon/src/server.rs` (startup initialization)

---

## Verification

Use /test-driven-development skill as our approach to testing in each iteration
