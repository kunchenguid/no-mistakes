# Task-First `/no-mistakes` Skill Evidence

This evidence captures the end-user skill prompt surface generated from `internal/skill/skill.go` into `skills/no-mistakes/SKILL.md`.

## Generator Check

Command: `go run ./cmd/genskill --check`

Output:

```text
genskill: skills/no-mistakes/SKILL.md is up to date
```

## Generated Skill Excerpt

Source: `skills/no-mistakes/SKILL.md`

```markdown
description: Validate your code changes through the no-mistakes pipeline - automated code review, tests, lint, docs, push, PR, and CI - before they reach upstream. Use when the user asks to run no-mistakes, gate or ship or validate their changes, push safely, asks you to do a task and then validate it, or invokes /no-mistakes.

## Two ways to invoke

`/no-mistakes` works in two modes, depending on whether the user hands you a
task along with the command:

- **Validate-only** - bare `/no-mistakes` (optionally with flag-style requests
  like "skip the lint step"). The user's code changes are already committed;
  validate them and report the outcome.
- **Task-first** - `/no-mistakes <task>`, e.g.
  `/no-mistakes add a --json flag to the status command`. First carry out the
  task yourself, then validate the result through the pipeline:
  1. **Check scope.** Inspect `git status` before you change or commit anything.
     Preserve unrelated pre-existing uncommitted changes, and when you commit,
     commit only the changes that belong to the user's task.
  2. **Do the work.** Make the changes the task describes, then **commit them on
     a feature branch**. If the user is on the repository's default branch,
     create a feature branch first - the gate validates committed history on a
     non-default branch, so the work must land there before you run.
  3. **Then validate**, passing the user's task as your `--intent`. The task
     text is exactly what the user set out to accomplish, in their own words, so
     it *is* the intent - pass it through, enriched with the decisions and
     tradeoffs you made while doing the work.
```
