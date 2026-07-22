---
title: CLI Commands
description: Complete reference for all no-mistakes commands and flags.
---

## no-mistakes

Attach to the active pipeline run for the current branch when one exists. If none exists, bare `no-mistakes` can start the setup wizard to create a branch, commit changes, push through the gate, wait for the daemon to register the new run, and then attach. If the push succeeds but no run is registered, that wizard path now exits with an explicit error instead of silently falling through. By default this wizard path is interactive and only runs in a TTY session. In non-interactive contexts, bare `no-mistakes` falls back to showing the last 5 runs inline unless you pass `-y` or `--yes` to run the wizard and accept defaults automatically. When a TTY is available, `-y` keeps the wizard visible, shows a brief `waiting for run…` state after push, and auto-advances the default path; without a TTY it falls back to the headless path.

```sh
no-mistakes
no-mistakes --skip test,lint
```

| Flag          | Type     | Default | Description                                          |
| ------------- | -------- | ------- | ---------------------------------------------------- |
| `-y`, `--yes` | `bool`   | `false` | Run setup wizard and accept defaults automatically   |
| `--skip`      | `string` | (none)  | Comma-separated pipeline steps to skip for a new run |

Unlike `no-mistakes attach`, bare `no-mistakes` only auto-attaches to an active run on the current branch.
`--skip` only applies when bare `no-mistakes` starts a new pipeline run through the wizard; it does not skip a step on an already-active run.
Valid step names are `intent`, `rebase`, `review`, `test`, `document`, `lint`, `push`, `pr`, and `ci`.

## no-mistakes init

Initialize or refresh the gate for the current repository.

`init` requires an `origin` remote to identify the upstream repository: later pipeline steps push validated branches to the configured target and open pull requests against that upstream. If `origin` is missing, add it with `git remote add origin <url>`, replacing `<url>` with the upstream repository's URL, then re-run `init`.

```sh
no-mistakes init
no-mistakes init --fork-url git@github.com:you/my-repo.git
```

| Flag         | Type     | Default | Description                                                                   |
| ------------ | -------- | ------- | ----------------------------------------------------------------------------- |
| `--fork-url` | `string` | (none)  | GitHub fork remote URL to push branches to while opening PRs against `origin` |

Creates or refreshes a local bare repo, installs the post-receive hook, best-effort isolates the gate repo's hook path from shared git config changes when Git supports `config --worktree`, adds or repairs the `no-mistakes` git remote, detects the default branch, records or updates the repo in SQLite, installs the `/no-mistakes` agent skill at user level into `~/.claude/skills/no-mistakes/SKILL.md` and `~/.agents/skills/no-mistakes/SKILL.md`, and ensures the daemon is running, installing the managed service when available and falling back to a detached daemon otherwise.
`init` writes no skill files into the repo; the user-level copies cover every supported agent (`~/.claude/skills` for Claude Code, `~/.agents/skills` for Codex, OpenCode, Rovo Dev, and Pi) across all repos.
If the home `.claude` links to `.agents`, `.claude/skills` links to `.agents/skills`, or the reverse, `init` follows that layout and still makes the skill readable from both logical paths.
If the repo still contains a vendored skill copy written by an older no-mistakes version, `init` leaves it untouched and prints a notice that it is no longer needed and can be removed.
The gate advertises Git push-option support, so you can skip steps for one push with `git push -o no-mistakes.skip=test,lint no-mistakes <branch>`.

For GitHub fork contributions, keep `origin` pointed at the parent repository and pass `--fork-url` with your fork remote URL.
The push, rebase branch-sync, and CI auto-fix pushes use the fork, while GitHub PR and CI commands stay scoped to the parent repository and create PRs with `--head <fork-owner>:<branch>`.
Fork routing currently requires both `origin` and `--fork-url` to be GitHub remotes with owner/repo paths.

Re-running `init` on an already-initialized repo succeeds and reports `Gate already initialized (refreshed)`.
It refreshes managed gate wiring, origin/default-branch metadata, hook-path isolation, and the installed agent skill, overwriting any stale `SKILL.md` content from an older binary.
When a fork URL is already recorded, re-running `init` without `--fork-url` preserves it.
Passing `--fork-url` again replaces the stored fork URL after validation.
If you rename or move an initialized working directory and the old path no longer exists, re-running `init` from the new path reattaches the existing gate, preserves the repo ID and run history, and updates the stored working path.
If you copy an initialized working directory while the original still exists, the copy is treated as a separate repo and gets a fresh gate.
Fresh init rolls back gate setup when a required gate or daemon step fails; refresh does not eject a pre-existing gate if daemon startup fails.
Skill installation is best-effort: if the skill write fails, init reports it and leaves the working gate in place.

## no-mistakes axi

Agent eXperience Interface for non-interactive agents.
Most agent workflows use the installed `/no-mistakes` skill, which drives this command surface underneath.
It prints TOON to stdout, prints progress to stderr, and uses structured stdout errors with exit code `1` for operational failures and `2` for bad usage.
At the TOON output boundary, unsupported C0 control bytes are rendered as visible `\xNN` escapes while tabs, carriage returns, newlines, printable Unicode, and the underlying durable logs remain unchanged.
If TOON encoding still fails, AXI prints a structured error instead of returning successful empty stdout.
The calling agent drives AXI approval gates but does not replace the configured pipeline agent that performs validation.

```sh
no-mistakes axi
```

With no subcommand, shows the executable path, description, repo, current branch, daemon state, recent runs, and next-step help, including a pointer to `no-mistakes axi run --help` and the installed `/no-mistakes` skill for full driving guidance.
When the current branch has an active run, that run appears as `active_run` with any approval gate and help for `axi respond` when it is parked or `axi status` when it is still running.
If an active run object is parked at a decision gate, it includes `awaiting_agent: parked <duration>` immediately after `status`.
That field is observability only; the `gate:` object still tells the agent which response to send.
If a step is actively `running` or `fixing`, the run object can also include an `active_steps` table with `active_for`, `last_activity`, native `agent_pid` when one is currently running, and the current execution or fix round.
When only another branch has an active run, that run appears as `other_branch_active_run`; the help tells agents to leave it alone and start validation for the current branch.
AXI help and outputs always repeat the preserve-prior-gate-progress contract: after a gate round has already produced fix commits, additional fixes belong on the same branch.
When a relevant `branch_sync` object is present, they also include version-matched synchronization guidance to follow before a post-pipeline local commit or fresh run.
Agents must not abort-and-restart, reset, replace the branch, or improvise Git recovery in a way that drops prior gate-fix commits.
A fresh run re-validates the current branch state, so already-resolved findings do not re-surface.

## no-mistakes axi run

Start or reattach to validation for the current branch, blocking until the first approval gate, CI-ready decision point, or final outcome.
An active run on another branch does not block starting validation for the current branch.

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi run --intent "the user's goal" --skip test,lint
no-mistakes axi run --intent "the user's goal" --yes
```

| Flag          | Type     | Default | Description                                                      |
| ------------- | -------- | ------- | ---------------------------------------------------------------- |
| `--intent`    | `string` | (none)  | What the user set out to accomplish; required to start a new run |
| `-y`, `--yes` | `bool`   | `false` | Auto-resolve every gate until a decision point or outcome        |
| `--skip`      | `string` | (none)  | Comma-separated pipeline steps to skip                           |

`--intent` is not a description of the diff.
It is the user's goal or request, and no-mistakes uses it verbatim instead of transcript inference.
Err on the side of completeness: include the goal, important decisions and tradeoffs, constraints or approaches ruled in or out, and explicit requests that might otherwise look surprising in the diff.
When starting a new run, `axi run` refuses the default branch and uncommitted working trees with actionable errors instead of auto-branching or auto-committing.
Reattaching to an in-flight run does not require `--intent`.
Reattachment accepts either the run's immutable submitted head or its current pipeline head, so pipeline-created fix commits do not detach an unchanged submitting worktree.
When neither identity matches, `axi run` keeps the fresh-run path but refuses a gate push while `branch_sync` says the pipeline still owns the branch.
That refusal returns the complete structured state and its `continue_active_run` or `recover_custody` next action instead of a raw Git non-fast-forward.
Reattaching to an in-flight run can proceed while the daemon is already running even if the global config file has become invalid, but starting a fresh run still requires valid global config.
Starting a fresh run also requires a runnable effective pipeline agent.
If the configured native agent or ACP runner is unavailable, the run fails before any pipeline step starts instead of reporting command-only validation as a passed gate.
With `--yes`, `axi run` treats both `action: auto-fix` and `action: ask-user` findings as standing consent for the pipeline to fix them by selecting every finding, then accepts the resulting fix review.
Gates with no findings or only `action: no-op` findings are approved as-is, and each step is fixed at most once so unresolved findings do not loop forever.
Without `--yes`, an agent driving `axi run` should stop when a gate contains `action: ask-user` findings and relay each finding's ID, file, and full description to the user before responding.
Review gates include a `note` field reminding agents that `auto_fix.review` defaults to `0`, so blocking and ask-user review findings park for a decision unless configuration explicitly opts back into review auto-fix.
Long-running `axi run` calls are working, not stalled; if one returns a `gate:`, read that output and answer it with `axi respond`.
Backgrounding a call is fine for an agent harness, but the run never advances past a gate on its own.
When the CI step is still monitoring an open PR and checks are green, `axi run` exits successfully with `outcome: checks-passed` instead of waiting for a human merge.
Treat that as the agent stopping point: ask the user to review and merge the PR from the `help` line.
If that PR later falls behind the default branch or hits a merge conflict, do not run `axi run`, `rerun`, or a manual rebase while the CI monitor is still running.
The monitor auto-rebases onto the base, resolves actual conflicts, and re-pushes the branch; a PR that is merely behind but clean needs no command.
Use `no-mistakes rerun` only after that monitor is no longer running, such as a closed PR, aborted or superseded run, idle timeout, or exhausted CI auto-fix attempts.
Successful outcomes (`checks-passed` and `passed`) also carry `help` instructions telling the agent to summarize the run.
When the pipeline applied fixes, they include a `fixes` table and a `help` instruction to acknowledge the misses and list those fixes for the user's review.

## no-mistakes axi respond

Answer the current approval gate and continue until the next gate, CI-ready decision point, or final outcome.

```sh
no-mistakes axi respond --action approve
no-mistakes axi respond --action fix --findings F1,F2 --instructions "optional guidance"
no-mistakes axi respond --action fix --add-finding '{"description":"...","action":"auto-fix"}'
no-mistakes axi respond --action skip
```

| Flag             | Type     | Default       | Description                                                          |
| ---------------- | -------- | ------------- | -------------------------------------------------------------------- |
| `--action`       | `string` | (none)        | `approve`, `fix`, or `skip`; required                                |
| `--step`         | `string` | awaiting step | Step to respond to                                                   |
| `--findings`     | `string` | (none)        | Comma-separated finding IDs for `--action fix`                       |
| `--instructions` | `string` | (none)        | Guidance applied to selected findings                                |
| `--add-finding`  | `string` | (none)        | JSON finding object to add and fix                                   |
| `-y`, `--yes`    | `bool`   | `false`       | Auto-resolve every subsequent gate until a decision point or outcome |

After the explicit response, `--yes` uses the same auto-resolution behavior as `axi run --yes`: have the pipeline fix `auto-fix` and `ask-user` findings once, approve the fix review, approve gates that only contain non-actionable `no-op` findings, and stop at `outcome: checks-passed` when CI is green but the PR still needs a human merge.
Each `axi respond` blocks until the next gate, CI-ready decision point, or final outcome.
If it returns another `gate:`, answer that gate; do not idle-wait for the run to move forward by itself.
When the daemon is already running, `axi respond` can continue an active run even if the global config file has become invalid, because it is not starting a fresh run.
The same successful-output reporting instructions apply to `axi respond` results.

## no-mistakes axi status

Show a run, preferring the current branch's active or most recent run before falling back to repo-wide active or recent runs.

```sh
no-mistakes axi status
no-mistakes axi status --run <id>
```

| Flag    | Type     | Default      | Description               |
| ------- | -------- | ------------ | ------------------------- |
| `--run` | `string` | resolved run | Inspect a specific run ID |

When the resolved run is parked at an `awaiting_approval` or `fix_review` gate, its top-level `run:` object includes `awaiting_agent: parked <duration>` immediately after `status`.
The field disappears after `axi respond`, on cancel, and on terminal outcomes; use it to distinguish a run waiting for the driving agent from one actively running, fixing, or watching CI.
When the resolved run has a `running` or `fixing` step, the run object includes `active_steps`.
Each row reports how long the step has been active, the latest meaningful log or native-agent lifecycle activity, the native agent PID if one is currently running, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
If no activity arrives for longer than `step_quiet_warning`, `last_activity` is prefixed with `quiet`; this is only a liveness signal and does not cancel the step.
For older active runs with no recorded activity timestamp, AXI falls back to the step log file modification time.
Gate summaries and finding descriptions are bounded in this default status view; truncated values disclose their original length, and the gate help points to `no-mistakes axi logs --step <step> --full` for the complete step log.
Relevant current-branch states also include a cached `branch_sync` object with full SHAs, the run's status, the persisted pipeline push binding, target kind and ref, relation, safety result, PR lifecycle, and a structured next action.
Cached home and status rendering performs no network read and labels the remote observation `pipeline_push`; only explicit sync check or apply reports `live` freshness.

## no-mistakes axi sync

Freshly check or apply the guarded synchronization offered by a `branch_sync.next_action`.

```sh
no-mistakes axi sync --check
no-mistakes axi sync
no-mistakes axi sync --recover
no-mistakes axi sync --recover --keep-local
```

| Flag           | Type   | Default | Description                                                                  |
| -------------- | ------ | ------- | ---------------------------------------------------------------------------- |
| `--check`      | `bool` | `false` | Verify the live target and exact plan without changing `HEAD`                |
| `--recover`    | `bool` | `false` | Return custody of a branch stranded by a terminal run with unpublished pipeline commits |
| `--keep-local` | `bool` | `false` | With `--recover`: keep the current local head; never touches the worktree   |

The default command is an explicit non-interactive apply request and never prompts.
All modes return the complete `branch_sync` object as TOON.
Exit code `0` means an eligible check, applied synchronization or recovery, already-synchronized or custody-returned no-op, or expected merged-and-removed no-op; blocked operational states return `1`.
The ordinary worktree mutation is either a strict fast-forward of the invoking clean checked-out branch to the freshly verified pipeline-owned pushed SHA, or an equivalent-diverged advance.
When a clean local branch and the pipeline-pushed head are diverged but the local unique work is content-equivalent to work already represented in the live pipeline head, `sync` reports `safety: safe_equivalent_advance`, anchors the pre-sync head under `refs/no-mistakes/sync-anchor/<run>`, and moves to the pipeline head with reset semantics.
Genuine divergence still reports `safety: blocked_diverged` and changes nothing.
Under `--recover`, the possible worktree mutation is a strict fast-forward to the preserved pipeline head after relation-specific preservation checks.
Fork configurations verify the configured fork URL and exact feature ref rather than assuming `origin`.
Dirty, in-progress, ahead, genuinely diverged, detached, wrong-branch, offline, changed-target, rewritten, deleted, legacy, or retired states fail closed without destructive recovery.
Run `axi sync` only when structured output offers `next_action.code: sync`; process any blocked state instead of substituting reset, stash, merge, rebase, force, or branch replacement.

### Custody recovery

A run that goes terminal (cancelled, failed, or completed without a push stage) after moving the pipeline head leaves the branch `pipeline_owned` with `safety: blocked_pipeline_owned_recoverable`, the run's terminal `pipeline.status`, and `next_action.code: recover_custody`.
While the run is still active, the same state stays blocked and reports `next_action.code: continue_active_run` with `no-mistakes axi status`.
`--recover` verifies the run is terminal, anchors the preserved head under `refs/no-mistakes/recover/<run>` in the invoking repository, and stamps custody returned so a fresh run can start.
For equal or ahead worktrees where the preserved head is already locally reachable, recovery writes that anchor locally without gate access.
For behind or diverged worktrees, recovery verifies the preserved head at the local gate branch and fetches it into the anchor before fast-forwarding only a clean behind worktree or refusing with the anchor named.
A dirty or diverged worktree refuses with explicit choices.
When you explicitly keep a behind or diverged local head instead of taking the preserved head, `--keep-local` returns custody at the current head without touching the worktree and atomically points the gate branch at it, so a concurrent gate push wins and the recovery refuses instead.
`no-mistakes rerun` is the alternative exit that resumes validating the preserved head instead of taking the branch back.
A recovered never-pushed run reports `state: custody_returned`; a recovered pushed run reports its ordinary classification against the last push binding, typically `local_ahead`.

## no-mistakes axi logs

Show the log output of one pipeline step.

```sh
no-mistakes axi logs --step review
no-mistakes axi logs --step review --full
no-mistakes axi logs --step review --run <id>
```

| Flag     | Type     | Default      | Description                             |
| -------- | -------- | ------------ | --------------------------------------- |
| `--step` | `string` | (none)       | Step name; required                     |
| `--run`  | `string` | resolved run | Run ID to inspect                       |
| `--full` | `bool`   | `false`      | Show the entire log instead of the tail |

Without `--full`, long logs show the last 40 lines and a help hint for the full log.
Step logs include native subprocess agent lifecycle lines such as `codex started pid=4242`, `codex exited pid=4242 status=success`, and transient retry messages when the selected agent supports lifecycle events.
They also include fix-loop markers such as `auto-fix round 1/3 starting after round 1` and `user-fix round starting after round 2`.

## no-mistakes axi abort

Cancel the active run for the current branch.
Active runs on other branches are left alone.

```sh
no-mistakes axi abort
```

If there is no active run, this succeeds as a no-op.

Pass `--run <id>` to cancel a specific run by its id instead of resolving the current branch:

```sh
no-mistakes axi abort --run <id>
```

`--run` does not need a repo, branch, or worktree, so it works from anywhere.
Use it to reap an orphaned CI monitor whose worktree was torn down before the PR merged - the run id is shown in `axi run` output and in the `axi` home view.
Aborting an id that is not an active run is a successful no-op.
When the daemon is already running, `axi abort` can cancel an active run even if the global config file has become invalid, because it is not starting a fresh run.
Branch-scoped abort waits for the cancellation state to persist, then renders the refreshed `branch_sync` object and its exact next action.
Pipeline-created commits remain preserved in the gate and a recoverable cancellation points directly to `no-mistakes axi sync --recover`.
While a run is active, do not use `axi abort` or `no-mistakes rerun` to go fix a finding yourself.
That cancels the pipeline's in-flight work and forces a full re-validation; use `axi respond --action fix` at the gate so the pipeline applies and re-checks the fix.

## no-mistakes eject

Remove the gate from the current repository.

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo directory, cleans up worktrees, and deletes the database record (cascades to runs and steps).
It does not remove any legacy repo-local agent skill files left by older versions; current `init` installs the skill at user level instead.

## no-mistakes attach

Attach to the active pipeline run.

```sh
no-mistakes attach [--run <id>]
```

| Flag    | Type     | Default | Description                                           |
| ------- | -------- | ------- | ----------------------------------------------------- |
| `--run` | `string` | (none)  | Attach to a specific run ID instead of the active run |

Opens the TUI for the active run anywhere in the current repo. If `--run` is specified, attaches to that specific run regardless of branch. Unlike bare `no-mistakes`, this does not stay branch-scoped before falling back.

## no-mistakes rerun

Rerun the pipeline for the current branch.

```sh
no-mistakes rerun
```

Starts a new pipeline run using the last-known head SHA on the current branch.
If another run is active on that branch, rerun cancels it before starting over.
Treat rerun as a between-runs action after a failed or cancelled outcome, or after you have committed a separate fix outside an active run; do not use it to bypass a gate.

## no-mistakes sync

Freshly verify and, with confirmation, safely move the invoking branch to an exact pipeline-owned push binding.

```sh
no-mistakes sync
no-mistakes sync --check
no-mistakes sync --yes
no-mistakes sync --recover
no-mistakes sync --recover --keep-local
```

| Flag           | Type   | Default | Description                                                     |
| -------------- | ------ | ------- | --------------------------------------------------------------- |
| `--check`      | `bool` | `false` | Verify and print the fresh plan without changing `HEAD`         |
| `-y`, `--yes`  | `bool` | `false` | Apply an eligible guarded synchronization without an interactive prompt |
| `--recover`    | `bool` | `false` | Return custody of a branch stranded by a terminal run with unpublished pipeline commits |
| `--keep-local` | `bool` | `false` | With `--recover`: keep the current local head; never touches the worktree |

Without `--yes`, apply prints the exact full-SHA plan and requires TTY confirmation; `--recover` prompts the same way before returning custody.
A non-TTY apply or recovery refuses with a direct `--yes` hint.
The command uses the same service and safety contract as `no-mistakes axi sync`, including the guarded equivalent advance and custody recovery documented there; it never stashes, rebases, creates a merge commit, switches branches, deletes a branch, or updates an external remote.

## no-mistakes status

Show repo, daemon, active run, and relevant cached local-branch synchronization status.

```sh
no-mistakes status
```

Displays:

- Repo path, upstream URL, and fork URL when configured
- Gate path
- Daemon status (running/stopped, PID)
- Active run details: ID, branch, status, head SHA, start time

## no-mistakes runs

List recorded pipeline runs for the current repo.

```sh
no-mistakes runs [--limit <n>]
```

| Flag      | Type  | Default | Description                       |
| --------- | ----- | ------- | --------------------------------- |
| `--limit` | `int` | `10`    | Maximum number of runs to display |

Shows runs newest-first with branch, status (styled), short SHA, timestamp, and PR URL if set.

## no-mistakes stats

Show historical usage stats across all repos.

```sh
no-mistakes stats
```

Displays total changes, rescued changes, rescue rate, reported and fixed mistakes, fixes by pipeline step, and the top repos by rescue activity.

Use `--agents` for local, per-purpose agent performance aggregates: duration and the subprocess-vs-model time split, session mode, errors, the token totals (input, output, cache-read, cache-creation, fresh input, reasoning), and the model round-trip and tool-category activity histogram, with a `METRICS` coverage count that tells a real zero apart from missing instrumentation.
Use `--run <id>` to inspect the individual agent invocations for one run - including each invocation's per-round token deltas next to the raw (cumulative for resumed sessions) counters, tool-category breakdown, workload size, finding count, and fallback reason - plus the total time parked at approval gates; it implies `--agents`.
Nullable fields an adapter did not report render as `-` (unknown), which is distinct from a recorded `0`; the legacy raw input, output, and cache-read counters remain numeric.

```sh
no-mistakes stats --agents
no-mistakes stats --run <id>
```

This detailed performance evidence stays local in `state.sqlite`; it is not sent to telemetry.
The field definitions and their local/remote split are owned by [the environment reference](/reference/environment/#what-stays-local-and-what-leaves-the-machine).

## no-mistakes doctor

Check system health and dependencies.

```sh
no-mistakes doctor
```

Checks:

- `git` binary
- `gh` CLI (optional, needed for GitHub PR and CI steps)
- `az` CLI (optional, needed for Azure DevOps PR and CI steps)
- Data directory (`~/.no-mistakes/`)
- SQLite database
- Daemon status
- Agent runners: native binaries `claude`, `codex`, `acli`, `opencode`, `pi`, and `copilot`, plus the optional ACP bridge `acpx`
- ACP alias default binaries: `cursor-agent` plus `acpx` for `cursor`
- Effective global agent configuration, reported as `gate validation`; an unavailable configured runner is a failed check because the gate cannot validate without it

Uses indicators: `✓` (available), `–` (not found, optional), `✗` (problem detected).

The standalone runner rows inspect default binary names; the `cursor` row reports whichever of `cursor-agent` and `acpx` are missing.
The [Global Config Reference](/no-mistakes/reference/global-config/) owns ACP gate-validation availability and probing semantics.
Each validation run performs the authoritative agent resolution again after applying any trusted repository-level override.

`doctor` checks `gh` and `az` availability. For GitLab PR and CI steps, install and authenticate `glab`. For Bitbucket Cloud PR and CI steps, set `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`. For Azure DevOps PR and CI steps, install the `azure-devops` extension and provide a PAT.

## no-mistakes update

Update the installed binary and reset the daemon.

```sh
no-mistakes update
no-mistakes update --beta
no-mistakes update -y
no-mistakes update --force
```

Downloads the latest release, verifies the SHA-256 checksum, atomically replaces the running binary, and resets the daemon when it is running or stale daemon artifacts exist so the new executable is picked up, preferring the managed service path and falling back to a detached daemon if service startup is unavailable or fails.
By default this installs the latest stable release.
Pass `--beta` to include prereleases and install the latest beta when one is newer than the current stable release.
If pending or running pipeline runs exist, update refuses to restart the daemon by default, prints each active run's ID, status, branch, and short head SHA. Pass `--force` to restart the daemon anyway and accept that those runs may fail; `-y`/`--yes` does **not** bypass this guard.
If the daemon is running from a different executable path, update still prompts before replacing it; pass `-y`/`--yes` to answer that prompt non-interactively.
If the daemon executable path cannot be determined, the update aborts before replacement.
If the daemon does not come back cleanly after a successful replacement, the command reports that failure.
On macOS, removes the quarantine extended attribute.

Because `update` installs the latest official release binary, the replacement binary includes the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

Background update checks run automatically on each CLI invocation (except `update` itself and version queries `--version` / `-v`, which stay side-effect-free). If a newer version is available, a notification is printed to stderr. Suppressed for dev builds or when `NO_MISTAKES_NO_UPDATE_CHECK=1` is set.

## no-mistakes daemon start

Start the daemon, installing or refreshing the managed service when possible.

```sh
no-mistakes daemon start
```

Prefers the managed service path and falls back to a detached daemon if service install or startup is unavailable or fails. If the daemon is already running, the command refreshes a stale macOS `launchd` or Linux `systemd` service definition and restarts through the managed service; if the definition is unchanged, it reports that the daemon is already running.

Only one live daemon can own an `NM_HOME`: at startup the daemon takes an exclusive OS lock on `$NM_HOME/daemon.lock`, so a second daemon started against the same root - however it was launched - fails with "a no-mistakes daemon is already running for this NM_HOME" instead of stealing the running daemon's socket.

## no-mistakes daemon stop

Stop the running daemon process.

```sh
no-mistakes daemon stop
no-mistakes daemon stop --force
```

If pending or running pipeline runs exist, `daemon stop` refuses by default and prints each active run's ID, status, branch, and short head SHA. Pass `--force` to stop the daemon anyway and accept that those runs may fail.

This does not remove the managed service. A later `no-mistakes`, `no-mistakes daemon start`, `init`, `attach`, `rerun`, or `update` can start the daemon again through the same service manager when available, or as a detached daemon otherwise.

## no-mistakes daemon restart

Restart the daemon.

```sh
no-mistakes daemon restart
no-mistakes daemon restart --force
```

Stops the current daemon and starts it again. This works whether the daemon is currently running or not.
If pending or running pipeline runs exist, `daemon restart` refuses by default and prints each active run's ID, status, branch, and short head SHA. Pass `--force` to restart the daemon anyway and accept that those runs may fail.

## no-mistakes daemon status

Check whether the daemon is running.

```sh
no-mistakes daemon status
```

Shows the PID if the daemon is running.
