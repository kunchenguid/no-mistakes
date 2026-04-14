---
title: Auto-Fix
description: How the automatic fix loop works.
---

When a pipeline step finds issues, `no-mistakes` can automatically ask the agent to fix them before pausing for your approval. This is controlled by the `auto_fix` configuration.

## How it works

1. A step executes and returns findings (e.g., test failures, lint warnings, review issues)
2. If `auto_fix` is enabled for that step (limit > 0) and the attempt count is below the limit, the executor re-runs the step with `fixing=true`
3. The agent receives the previous findings and applies fixes
4. The step re-runs to verify the fixes
5. If issues remain and attempts are left, the loop continues
6. Once the limit is reached or all issues are resolved:
   - If issues remain, the step pauses for user approval
   - If everything passes, the step completes and the pipeline moves on

## Configuration

Set limits in global or repo config:

```yaml
auto_fix:
  rebase: 3
  review: 3    # up to 3 auto-fix attempts
  test: 3
  document: 3
  lint: 3
  ci: 3
```

Setting a step to `0` means the pipeline always pauses for human input when that step finds issues.

Repo config overlays global config - you can set `auto_fix.lint: 5` in a repo's `.no-mistakes.yaml` to override just that step while inheriting the rest from global.

## Finding actions

Agent-driven findings now use an `action` field instead of `requires_human_review`:

- `auto-fix` - objective issues that can be fixed automatically
- `ask-user` - intent-sensitive or ambiguous issues that always require manual approval and are never auto-fixed
- `no-op` - informational notes that do not need a fix

`ask-user` is meant for findings that challenge the author's intent - for example, questioning an intentional product or design choice, or arguing that an intentional addition, removal, or guard should be undone. Routine correctness, reliability, or security fixes still stay `auto-fix` even if the smallest fix reintroduces a small amount of previously deleted logic.

The `review`, `test`, and `lint` steps use this shared model directly. The `document` step also uses the same `action` field, but any documentation finding still pauses for approval because even objective doc gaps need an explicit documentation pass before the pipeline continues.

Documentation findings use the same approval loop, but the `document` step treats any finding as a documentation gap that should pause for approval. When auto-fix is enabled, the agent can update docs or doc comments, then the step re-runs and proceeds only after reassessment returns no findings.

## User-triggered fixes

When the pipeline pauses for approval, you can manually trigger a fix from the TUI:

1. The findings panel shows all findings with checkboxes
2. Toggle individual findings with `space`, or use `A` (all) / `N` (none)
3. Press `f` to fix the selected findings

The agent receives only the selected findings. Deselected findings are passed as "dismissed" so the agent knows not to re-report them.

After a user-triggered fix, the step re-runs and pauses again to show you the results (`fix_review` status). You can then approve, fix again, skip, or abort.

## Fix commits

Each auto-fix cycle commits its changes with a descriptive message:

| Step | Commit prefix |
|---|---|
| Rebase | `no-mistakes(rebase): <summary>` |
| Review | `no-mistakes(review): <summary>` |
| Test | `no-mistakes(test): <summary>` |
| Document | `no-mistakes(document): <summary>` |
| Lint | `no-mistakes(lint): <summary>` |

The push step commits any remaining uncommitted changes with `no-mistakes: apply agent fixes`.

## Step rounds

Each execution of a step (initial run, auto-fix, user-fix) is recorded as a "round" in the database. The PR body's pipeline section is built from these rounds, giving reviewers visibility into what was fixed and how many attempts it took.

Round trigger types:
- `initial` - first execution
- `auto_fix` - triggered by the automatic fix loop
- `user_fix` - triggered by the user pressing `f` in the TUI
