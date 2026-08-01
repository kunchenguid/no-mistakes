# Reading AXI output

- Output is TOON: `key: value` pairs, `name[N]{cols}:` tables, and `help[N]:` hints.
- The `help` list at the bottom of most responses tells you the next commands to run.
- Errors are printed as `error: ...` on stdout with a `help` list; act on the suggestion.
- A final state shows `outcome: <checks-passed|passed|failed|cancelled>` with no `findings` table.
- Field names and exact columns vary by step and version, so read the actual `findings` header rather than assuming a layout.
- A successful outcome may carry a `fixes[N]{step,summary}:` table - one row per fix round the pipeline applied, in step then round order, where `summary` describes what that round changed (a round that recorded no summary shows `fix applied (no summary recorded)`). Acknowledge those misses and list each fix for the user.

## A gate block

A `gate:` waiting on you looks roughly like this - a `gate:` line naming the step, optional step-specific fields such as `note`, a `findings[N]{...}:` table with one row per finding, and a `help[N]:` list of next commands:

```
gate: review
note: Review auto-fix is disabled by default, so blocking and ask-user review findings park for your decision.
findings[2]{id,severity,file,line,action,description}:
  r1,warning,internal/pipeline/executor.go,,auto-fix,Error from os.Remove is ignored
  r2,error,cmd/no-mistakes/main.go,,ask-user,New --force flag bypasses the confirm prompt
help[4]:
  Run `no-mistakes axi respond --action approve` to accept this step and continue
  Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself)
  Run `no-mistakes axi respond --action skip` to skip this step
  Run `no-mistakes axi logs --step review --full` to read the full step log
```

## Run-progress fields

`axi status` and other non-terminal run objects report progress with these fields:

- `awaiting_agent: parked <duration>`, immediately after `status`, means the run is parked at an approval or fix-review gate and waiting for you to send `axi respond`. It is observability only: it does not change gate resolution, auto-resume the run, or make `--yes` the default.
- `active_steps` appears while a step is `running` or `fixing`, with `active_for`, `last_activity`, a native `agent_pid` when a subprocess agent is running, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
- A `last_activity` prefixed with `quiet` means no step log or native-agent lifecycle activity has arrived for longer than `step_quiet_warning`. Treat that as a liveness clue only.
