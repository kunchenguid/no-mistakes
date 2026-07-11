---
title: Migrating to routing
description: How to move a config that predates the routing cutover onto the routing contract.
---

Model selection moved from per-agent configuration keys to one global [routing contract](/no-mistakes/reference/routing/).
The cutover is clean: the old keys were removed, there are no aliases, and no-mistakes never rewrites your config for you.
A config that still sets a removed key fails to load with an error naming the key and what replaced it:

```
global config key "agent" is no longer supported: model selection is configured via `routing` (runners, profiles, routes); there is no single-agent selector
```

This page tells you what to do for each removed key.
Most people finish by deleting keys: the built-in routing contract applies whenever `routing` is omitted, and it needs no configuration.

## Migrate your global config

Work through `~/.no-mistakes/config.yaml` key by key.

### agent

Delete it.
There is no single-agent selector.
Every invocation is routed by its purpose through profiles of provider candidates; the defaults use `codex` first with a `claude` backup at every tier.
If you must pin different models, declare a complete custom `routing` block; copy the [full custom routing example](/no-mistakes/reference/routing/#custom-routing) and edit it.

### fallback_agents

Delete it.
Provider fail-over is now structural: each profile orders its candidates, and a candidate backup is tried after a classified operational failure opens the first candidate's provider circuit.
Non-operational failures, such as malformed output or a bad result, fail immediately instead of falling through to another provider.

### acpx_path and acp_registry_overrides

Delete both.
ACP and acpx agents were removed entirely.
The only runners are `codex` and `claude`, declared under `routing.runners` with their canonical failure domains.

### agent_path_override

Delete it.
A custom executable path belongs in `routing.runners.<name>.executable` inside a complete `routing` block:

```yaml
routing:
  runners:
    codex:
      executable: /opt/homebrew/bin/codex
      failure_domain: openai
    claude:
      executable: /Users/you/bin/claude
      failure_domain: anthropic
  # profiles and routes are required too - a partial block is rejected.
```

Remember the block is a complete replacement: it must route every registered purpose and declare every profile and runner those routes reference.
Copy the [full custom routing example](/no-mistakes/reference/routing/#custom-routing) and change only the executables.

### agent_args_override

Delete it.
Arbitrary native arguments cannot be configured.
The two things the old flags were mostly used for - model and reasoning effort - are now first-class candidate fields (`model`, `effort`) in a custom `routing` block.
Everything else about the native command line is managed by no-mistakes.

### auto_fix

Delete it, in both global and repo config.
Per-step numeric auto-fix limits no longer exist.
Repair escalates each blocking finding through its purpose's route, for example `fix_fast` → `fix_balanced` → `authority_strong`, with a fresh fixer and verifier at every tier.
When the cascade is exhausted, the finding is persisted unresolved and the gate fails closed; in unattended mode the run aborts instead of retrying.
There is no numeric knob to turn: choosing stronger or weaker profiles in a custom `routing` block is the supported way to tune repair.

### babysit_timeout

Rename it to `ci_timeout`.
The value semantics are unchanged, and the unlimited keywords (`unlimited`, `none`, `off`, `never`) still work.
The old spelling is not an accepted alias; it is a load error.

### daemon_connect_timeout and step_quiet_warning

Delete both.
Daemon connection readiness and step liveness reporting are managed internally and are no longer configurable.

## Migrate your repo config

`.no-mistakes.yaml` loses `agent` and `auto_fix`, and it can never define routing mechanics.
A repo config that sets `agent`, `auto_fix`, `fallback_agents`, an override key, `routing`, `runners`, `profiles`, or `candidates` fails to load with:

```
repo config may not define "agent": model selection is global-only through the routing contract; a repository cannot select an agent
```

If a repo genuinely needs different model behavior, use `routes`: map a purpose to one existing global profile, on the default branch.

```yaml
# .no-mistakes.yaml
routes:
  documentation_authoring: fix_balanced
```

See the [repo config routes field](/no-mistakes/reference/repo-config/#routes) for the trust rules.

## Keep only current configuration

Remove every retired key before you load the config.
Do not keep a commented legacy block as a template because it cannot be translated automatically.

If the only setting you still need is the CI monitoring timeout, the valid global config is:

```yaml
# ~/.no-mistakes/config.yaml
ci_timeout: "72h"
```

Use a custom `routing` block only when the built-in models, candidate order, or purpose routes do not meet your needs.
Start from the [full custom routing example](/no-mistakes/reference/routing/#custom-routing) because every custom block must be complete.

## Check your migration

1. Remove or replace every key listed above in `~/.no-mistakes/config.yaml` and `.no-mistakes.yaml`.
2. If you added a `routing` block, confirm it routes every registered purpose and declares every profile and runner it references.
3. Run `no-mistakes doctor` and confirm the config line reports no error.

`no-mistakes doctor` loads the global config with the same strict parser as the daemon, so a passing report means the daemon will accept it too.
