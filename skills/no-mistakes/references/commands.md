
## Inspecting state

```sh
no-mistakes axi               # home view: current branch, active runs, next steps
no-mistakes axi status        # full detail plus cached branch_sync when relevant
no-mistakes axi sync --check  # freshly verify an offered synchronization plan
no-mistakes axi sync          # apply only an offered guarded synchronization
no-mistakes axi sync --recover  # return custody after a terminal run left unpublished pipeline commits
no-mistakes axi sync --adopt-published  # adopt an exactly published rebased head into its stale gate lane
no-mistakes axi logs --step <name> --full   # one step's recorded findings, complete summary, and full log
no-mistakes axi abort         # cancel the current-branch active run
no-mistakes axi abort --run <id>   # cancel a specific run by id (works outside its worktree)
no-mistakes axi respond --run <id> --action approve   # answer a specific run's parked gate by id (works outside its worktree)
```

## Reading the output

- Output is TOON: `key: value` pairs, `name[N]{cols}:` tables, and `help[N]:` hints.
- `axi status` is scoped to your current branch when `--run` is omitted: with a known current branch, an implicitly resolved `run:` is this branch's. A run under `other_branch_run:` is one you named with `--run <id>` that belongs to another branch - never read its status or outcome as your own work. An explicit `--run <id>` rendered under `run:` while the current branch is unknown (detached `HEAD` or a branch-lookup failure) encodes no branch relationship. In a successful status response, no run object at all means this branch has no run yet, whatever the recent-runs table lists; an `error:` response proves nothing about run ownership, so act on the error instead of concluding the branch is idle.
- A non-terminal run object may include `awaiting_agent: parked <duration>` immediately after `status`; that means the run is parked at a gate. Only an implicitly resolved current-branch gate offers bare `axi respond`; an explicit `--run <id>` status is inspection-only even when its branch matches, because the branch may have a newer active run, and names `axi respond --run <id>` as its response command. Follow the response's `help`.
- A run object with a `running` or `fixing` step may include an `active_steps` table. `active_for` is the enclosing step duration; `round_active_for` is the displayed execution or fix round duration and resets for a fix round. Older runs without round timing leave `round_active_for` empty.
- The `help` list at the bottom of most responses tells you the next commands to run.
- Errors are printed as `error: ...` on stdout with a `help` list; act on the suggestion.
- Exit codes: `0` success, no-op, or normal decision gates, `1` failed or cancelled final outcomes, `2` bad usage.

A `gate:` waiting on you looks roughly like this - a `gate:` line naming the step, optional step-specific fields such as `note`, a `findings[N]{...}:` table with one row per finding, and a `help[N]:` list of next commands:

```
gate: review
note: Review auto-fix is disabled by default (auto_fix.review: 0; a repo or global auto_fix.review > 0 override re-enables it), so blocking and ask-user review findings park for your decision rather than being silently self-fixed.
findings[2]{id,severity,file,line,action,description}:
  r1,warning,internal/pipeline/executor.go,,auto-fix,Error from os.Remove is ignored
  r2,error,cmd/no-mistakes/main.go,,ask-user,New --force flag bypasses the confirm prompt
help[6]:
  Run `no-mistakes axi respond --action approve` to accept this step and continue
  Run `no-mistakes axi respond --action fix --findings <ids> [--ignore <ids>]` to have the pipeline fix the selected findings (do not edit files yourself); list every finding below in `--findings` or `--ignore`, unless an earlier round of this step already decided it, and a finding that round chose to fix cannot be declined by a later response
  Run `no-mistakes axi respond --action skip` to skip this step
  Run `no-mistakes axi logs --step review --full` to read the complete step summary and log
  A long-running call is working, not stalled - background it if your harness needs to, but the run never advances past a gate on its own. Read every return; on a `gate:`, respond; loop until an `outcome:`.
  Commit post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present. Never abort-and-restart, reset, or replace the branch in a way that drops prior gate-fix commits.
```

Read the `action` column per row: decide `r1` (auto-fix) on your own
judgment - `respond --action fix --findings r1` hands it to the pipeline to
fix - but stop and escalate `r2` (ask-user) to the user before responding. A
final state
instead shows `outcome: <checks-passed|passed|passed-with-override|passed-with-skips|failed|cancelled>` with no
`findings` table. Field names and exact columns can vary by step and version,
so read the actual `findings` header rather than assuming this layout.
