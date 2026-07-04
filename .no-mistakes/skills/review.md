---
name: review
description: Example read-only skill review — analyze the diff for bugs, risks, and simplifications and emit findings without changing code.
mode: review
---

# Review skill

You are reviewing a code change. Analyze it and emit structured findings. This
is a **read-only** pass: do not edit, stage, or commit any files — anything you
change will be discarded and reported as a violation.

This body is layered between an engine-owned context header (branch, base and
target commits, review scope, default branch, ignore patterns) and an
engine-owned output contract (severity levels and the `ask-user` / `auto-fix` /
`no-op` action vocabulary). You only need to describe *what to look for* — the
engine handles *how findings are reported and gated*.

## What to look at

- Read the relevant history and diff yourself.
- Focus on risks introduced by the changed code, but inspect surrounding code,
  call sites, shared helpers, tests, and invariants when needed to understand
  root cause.
- Do NOT run tests here — the pipeline has a dedicated test step.

## What matters

- Bugs, correctness issues, and race conditions.
- Security issues, performance regressions, breaking changes, and insufficient
  error handling — treat all of these as risks.
- Simplification opportunities: reducing complexity through non-functional
  refactoring (deduplication, clearer control flow). This does **not** mean
  removing features, changing product behavior, or stripping intentional
  user-facing output.

## How to report

- Do a full review pass before returning. Do not stop after the first valid
  finding — enumerate every material issue you can substantiate.
- Anchor every finding to a specific file and one-indexed line number when
  possible.
- Be concise and actionable. No generic advice like "add more tests".
- Do NOT report styling, formatting, linting, compilation, or type-checking
  issues.
- If the change is clean, return an empty findings array.

## Customizing this skill

Replace the sections above with your repo's own review conventions — for
example an API-compatibility checklist, a design-system/UX-copy conformance
guide, security rules, or architecture constraints. Reference it from
`.no-mistakes.yaml` on your **default branch** (the skill body is loaded from
the trusted default-branch commit, never a pushed branch):

```yaml
steps:
  - intent
  - rebase
  - review
  - name: security-review
    type: skill
    skill: .no-mistakes/skills/review.md
    mode: review
  - test
  - lint
  - push
  - pr
  - ci
```
