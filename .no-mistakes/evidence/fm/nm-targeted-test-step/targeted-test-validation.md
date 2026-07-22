# Targeted local Test validation

Validated target `f027af1f66a182c00356e0a7c725e8959b321c29` against the explicit intent that local Test selects focused checks and evidence while remote CI retains broad regression.

## End-to-end product journey

The real e2e harness initialized no-mistakes, pushed through the gate, started the daemon, invoked the Codex adapter as a subprocess, persisted pipeline state, and read it back over IPC:

```text
$ go test -tags=e2e ./internal/e2e -run '^TestUserJourney/codex$' -count=1 -v
=== RUN   TestUserJourney/codex
agent invocations: 14
step outcomes:
  1 intent    skipped
  2 rebase    completed
  3 review    completed
  4 test      completed
  5 document  completed
  6 lint      completed
  7 push      completed
  8 pr        skipped
  9 ci        skipped
rerun outcome: 01KY3PGEYEAB7C4NJ8N4TQ2FX5 completed
--- PASS: TestUserJourney/codex
```

The e2e fake-agent fixture only matches the Test invocation when the runtime prompt begins:

```text
You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself.
```

That journey completed both the initial pipeline and rerun with the new prompt.

## Runtime prompt boundaries exercised

Focused `TestStep.Execute` regressions captured the actual prompts sent to the agent and verified these clauses:

| Path | Runtime contract demonstrated |
|---|---|
| Normal Test agent | Run the smallest relevant tests; do not run the complete repository suite; remote CI owns broad regression; write a focused test, gather manual evidence, or report an honest missing-evidence warning instead of running nothing. |
| Test repair agent | Reproduce the specific failing case, fix its root cause, and rerun only that focused verification. |
| Conflicting driver instruction | A previous finding containing `confirm the full suite path for this failure is green` remains visible to the agent, while the same runtime prompt explicitly says generic broad/full-suite requests do not override focused verification. |
| No targeted evidence | A warning with `action: ask-user` produces `NeedsApproval`, so the pipeline does not manufacture a pass. |
| Configured targeted command plus intent | The configured command executes first, then the evidence agent runs and persists its `tested`, `testing_summary`, and artifact fields. |

## Local and remote ownership

The repository config was loaded through the product config parser and returned an empty `commands.test`, enabling agent-selected targeted validation. The CI workflow still contains:

```yaml
- if: runner.os != 'Windows'
  run: go test -race ./...
```

The focused static guards also verified that the authoritative `commands.test` docs define targeted local validation, reject CI-parity wording, and explicitly avoid shell-command heuristics.

## Lifecycle regression protection

Focused clean-exit and escaped-pipe tests passed for the Codex agent, native agent command, and shared shell-command helper. This preserves #357 process-group reaping and Unix `WaitDelay` while returning local Test to the agent-driven path.
