---
title: Shared Gate Profiles
description: Define one gate profile once and apply it across many repos.
---

A **shared gate profile** lets you define a pipeline once — steps, skills, and
instructions — and apply it to many repos with a single trusted line in each
repo's config, instead of copy-pasting `.no-mistakes.yaml` and skill files into
every repo and re-copying on every change.

A profile lives on the daemon host under `<NM_HOME>/profiles/<name>/` and is
selected by a repo's `profile: <name>` field. Because that field decides which
shell commands and which agent prompts run, it is a **trusted-only** selection:
it is read from the repo's default branch, never from a pushed branch (see
[Trust model](#trust-model)).

## Layout

```
<NM_HOME>/profiles/team-ios/
  profile.yaml                 # a version marker + a steps: list
  skills/ios-review.md         # skill bodies (resolved relative to the profile dir)
  instructions/swift.md        # instruction files (resolved relative to the profile dir)
```

`NM_HOME` defaults to `~/.no-mistakes`. A convenient way to keep the profile
fresh across a fleet is to make `<NM_HOME>/profiles/team-ios/` a git clone of a
team-owned repo: updating everyone's gate is then a `git pull`.

## profile.yaml

`profile.yaml` carries a `version` marker plus a `steps:` list in the exact same
schema a repo's own [`steps:`](/no-mistakes/reference/repo-config/#steps) uses —
built-in steps, [custom command steps](/no-mistakes/reference/repo-config/#custom-command-steps),
and [skill-driven steps](/no-mistakes/reference/repo-config/#skill-driven-steps):

```yaml
version: 3                     # informational; stamped into the run log
steps:
  - rebase
  - review
  - name: ios-review
    type: skill
    skill: skills/ios-review.md    # resolved against THIS profile dir
    mode: review
  - name: swiftlint
    type: command
    command: swiftlint --strict --reporter json > .nm-swiftlint.json || true
    findings_json: .nm-swiftlint.json
  - test
  - push
  - pr
  - ci
```

`skill:` and `instructions:` paths inside a profile step resolve **against the
profile directory**, and must not escape it. The bodies are read from
host-local disk, never from a repo worktree.

## Selecting a profile in a repo

Add one line to the repo's `.no-mistakes.yaml` on the **default branch**:

```yaml
profile: team-ios
```

With no `steps:` of its own, the repo's pipeline **is** the profile's step list.

## Composing profile + repo steps

If a repo also wants its own steps, it must say **where** the profile's steps
go, using the `- use: profile` splice sentinel:

```yaml
profile: team-ios
steps:
  - use: profile               # ← the team-ios steps splice in here
  - name: repo-special-check
    type: command
    command: ./scripts/check-generated.sh
```

Two rules (v1):

1. `profile:` and **no** repo `steps:` → the profile's list is the pipeline.
2. `profile:` **plus** repo `steps:` → the list must contain exactly one
   `- use: profile` sentinel, which expands in place to the profile's steps. A
   repo `steps:` list with a profile selected but **no** sentinel is an error —
   dropping the shared gate is too consequential to infer.

The merged list is validated exactly like any `steps:` list (unique names,
push-chain ordering).

## Fail-closed behavior

A profile is a team gate, so a **missing or unparsable profile fails the run at
start** rather than silently dropping to the default pipeline. A host that has
not provisioned the profile directory cannot gate that repo until it does. (A
missing skill *file* inside an otherwise-valid profile parks the individual
skill step with a misconfiguration finding, matching built-in skill steps.)

## Trust model

The `profile:` **reference** rides the same trusted channel as `commands`,
`agent`, and `steps`: it is read only from the repo's trusted default-branch
`.no-mistakes.yaml`, so a pushed branch can never set, switch, or drop a
profile. Unlike those fields, `profile:` stays trusted-only **even when**
[`allow_repo_commands`](/no-mistakes/reference/repo-config/#allow_repo_commands)
is set — the safer v1 default.

The profile's **content** is never read from a pushed worktree at all: it lives
under `<NM_HOME>/profiles/`, a path no pushed commit can address. Its trust
anchor is filesystem ownership on the daemon host — the same class as
`~/.no-mistakes/config.yaml`, which already selects the agent binary that runs
with the maintainer's credentials.

## Auditing which profile gated a run

Each run that used a profile is stamped with `<name>@<ref>` — the profile
checkout's `HEAD` when the profile directory is a git repo, else a content hash
of `profile.yaml`. The stamp is stored on the run record (visible via
`axi status` / the run info) and logged at run start, so a consumer can confirm
which profile revision enforced a given gate.
