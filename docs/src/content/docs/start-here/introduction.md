---
title: Introduction
description: What no-mistakes is and why it exists.
---

`no-mistakes` puts a local git proxy in front of your real remote. Push to `no-mistakes` instead of `origin`, and it spins up a disposable worktree, runs an AI-driven validation pipeline, forwards upstream only after every check passes, and opens a clean PR automatically.

## Why

Shipping code is too often "commit, push, wait for CI, watch it fail, push again, watch CI fail again." Pre-commit hooks help but block your workflow and don't run heavy checks. CI runs everything but you only see failures after the push is already public. Branch protection catches a few things but can't fix them for you.

`no-mistakes` sits in between. It's a local gate you push to on purpose:

- **Before** the code is public, it rebases, runs a structured AI code review, runs your tests, checks that docs are in sync, runs lint, and only then pushes upstream and opens the PR.
- **After** the push, it watches CI and auto-fixes failures. On GitHub and GitLab it also watches PR mergeability and fixes merge conflicts on the branch.
- **Throughout**, every step can pause for your approval. You see the findings, pick what to fix, and decide when to ship.

The whole thing runs in a disposable worktree. Your working directory is never touched, so you can keep coding while the pipeline runs.

## Mental model

```
  you                        no-mistakes                upstream
  ───                        ───────────                ────────
  git push no-mistakes  →    local gate repo            origin
                             ↓
                             worktree
                             ↓
                             rebase → review → test →
                             document → lint →
                                                 → push → origin
                                                 → pr
                                                 → ci watch + auto-fix
```

`origin` is never hijacked. Regular `git push` still works normally. You opt into the gate by pushing to the `no-mistakes` remote.

## What you get

- A fixed, opinionated pipeline: `rebase → review → test → document → lint → push → pr → ci`. Order is not configurable; what each step runs is.
- Choice of agent: `claude`, `codex`, `rovodev`, or `opencode`, with per-repo override.
- A TUI to watch, approve, fix, skip, or abort any step.
- A setup wizard when you run bare `no-mistakes` with no active run on the current branch - it walks you through creating a branch, committing, and pushing through the gate before attaching to the new run.

## Next

- [Quick Start](/no-mistakes/start-here/quick-start/) - first push in five minutes
- [Installation](/no-mistakes/start-here/installation/) - full install options
- [The Gate Model](/no-mistakes/concepts/gate-model/) - architecture and design decisions
