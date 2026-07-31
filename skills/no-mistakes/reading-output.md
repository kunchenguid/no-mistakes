# Reading AXI output

- Output is TOON: `key: value` pairs, `name[N]{cols}:` tables, and `help[N]:` hints.
- A non-terminal run object may include `awaiting_agent: parked <duration>` immediately after `status`; a run object with a `running` or `fixing` step may include an `active_steps` table. Both are described in the SKILL.md validate-and-decide loop.
- A final state shows `outcome: <checks-passed|passed|failed|cancelled>` with no `findings` table.
- Field names and exact columns vary by step and version, so read the actual `findings` header rather than assuming a layout.

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
