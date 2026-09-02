---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

acpx_path: acpx

forgejo_axi_path: forgejo-axi

acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  grok: /Users/you/.grok/bin/grok
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi
  copilot: /usr/local/bin/copilot

agent_config:
  codex:
    model: gpt-5.4
    effort: low

agent_args_override:
  codex:
    - -c
    - service_tier="priority"

ci_timeout: "168h"

step_quiet_warning: "10m"

agent_timeout: "30m"

review_agent_timeout: "30m"

test_agent_timeout: "30m"

daemon_connect_timeout: "3s"

branch_sync_remote_timeout: "60s"

gate_reconcile_interval: "2m"

gate_reconcile_timeout: "30s"

log_level: info

session_reuse: true

worktree_roots:
  /Users/you/src/my-repo: /Users/you/work/my-repo-runs

forge_profiles:
  github-personal:
    gh_config_dir: ~/.config/gh-personal
  github-work:
    gh_config_dir: ~/.config/gh-work
  gitlab-work:
    glab_config_dir: ~/.config/glab-work

auto_fix:
  rebase: 3
  review: 0
  test: 3
  document: 3
  lint: 3
  ci: 3

ci:
  rerun_transient: 0
  revalidate_repairs: false

commit:
  fix_message: "chore(no-mistakes-{{.Step}}): {{.Summary}}"

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    attach_media: true
    dir: .no-mistakes/evidence
    branch: no-mistakes/evidence
    retention: 336h
    max_runs: 200
```

## Fields

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

|         |                                                                                             |
| ------- | ------------------------------------------------------------------------------------------- |
| Type    | `string` or `string[]`                                                                      |
| Values  | `auto`, `claude`, `codex`, `grok`, `rovodev`, `opencode`, `pi`, `copilot`, `antigravity`, `cursor`, `acp:<target>` |
| Default | `auto`                                                                                      |

`auto` resolves to the first supported native agent or ACP alias in this order: `claude`, `codex`, `grok`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, `antigravity`, then `cursor`.
`cursor` is an ACP alias for the `cursor` target with default command `cursor-agent acp`.
With default paths, `auto` only selects it when both `cursor-agent` and `acpx` resolve; `acp_registry_overrides.cursor` and `acpx_path` replace those respective defaults during availability checks.
`acp:<target>` uses the user-installed `acpx` binary to run an ACP target, for example `acp:gemini`; `acp:cursor` uses the same default command as `cursor`.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If an explicit agent is unavailable, `auto` finds no native agent or ACP alias, or no fallback-list entry is available, the gate fails before its first pipeline step rather than reporting a partial command-only validation as passed.
`no-mistakes doctor` checks the global configuration, while every run repeats resolution after applying any trusted repository-level `agent` override.

You can also set an ordered fallback list:

```yaml
agent: [codex, grok]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
After resolving `auto`, entries that resolve to the same ACP target are deduplicated in list order, so `cursor` and `acp:cursor` provide one fallback and preserve whichever spelling appears first.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.

### acpx_path

Path to the user-installed `acpx` binary used for `agent: acp:<target>` and ACP aliases such as `agent: cursor`.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | `acpx`   |

### forgejo_axi_path

Executable used for Forgejo PR and CI operations.

|         |               |
| ------- | ------------- |
| Type    | `string`      |
| Default | `forgejo-axi` |

A bare name is resolved from the daemon's effective `PATH`; an explicit path is executed directly. See [Provider Integration](/no-mistakes/guides/provider-integration/#forgejo) for setup and the [environment reference](/no-mistakes/reference/environment/#forgejo_base_url) for host and token configuration.

### acp_registry_overrides

Map an ACP target name to a raw ACP agent command.
When `agent: acp:<target>` matches an override key, no-mistakes runs `acpx --agent <command>` instead of `acpx <target>`.
ACP aliases use the same target keys. For example, `agent: cursor` and `agent: acp:cursor` resolve to the `cursor` target, so set `cursor` to override the default `cursor-agent acp` command.
Values are trimmed; a blank or whitespace-only value behaves as no override, so an alias keeps its default command.
Availability checks always resolve `acpx_path`. They also probe the executable named first in the effective non-blank raw command when it is a bare command name or clean absolute path. Relative, quoted, or escaped raw commands are not pre-probed; `acpx` executes them from the worktree. These checks do not invoke the ACP target or test its credentials.

|         |                     |
| ------- | ------------------- |
| Type    | `map[string]string` |
| Default | Empty               |

Example:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

### agent_path_override

Custom binary paths for native agents.
When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.
ACP agents and aliases use `acpx_path` for the bridge; use `acp_registry_overrides` to replace a raw target command such as `cursor-agent acp`.

|         |                                   |
| ------- | --------------------------------- |
| Type    | `map[string]string`               |
| Default | Empty (uses default binary names) |

Default native binary names when no override is set:

| Agent      | Binary     |
| ---------- | ---------- |
| `claude`   | `claude`   |
| `codex`    | `codex`    |
| `grok`     | `grok`     |
| `rovodev`  | `acli`     |
| `opencode` | `opencode` |
| `pi`       | `pi`       |
| `copilot`  | `copilot`  |
| `antigravity` | `agy`      |

### agent_config

Model and reasoning effort per agent, in one common spelling. no-mistakes maps each field down to whatever mechanism that harness actually uses, so you no longer have to know each CLI's own flag.

|         |                                                                                     |
| ------- | ----------------------------------------------------------------------------------- |
| Type    | `map[string]{model, effort}`                                                        |
| Keys    | `claude`, `codex`, `grok`, `rovodev`, `opencode`, `pi`, `copilot`, `antigravity`, `cursor`, `acp:<target>` |
| Default | Empty (every harness keeps its own defaults)                                        |

```yaml
agent_config:
  codex:
    model: gpt-5.4
    effort: low
  claude:
    model: sonnet
    effort: high
  opencode:
    model: openai/gpt-5
  cursor:
    model: gpt-5
```

`effort` is one of `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. The value is passed to the harness as written, so a level that harness does not implement is rejected by the harness itself rather than silently downgraded.

How each field maps:

| Agent             | `model`                                       | `effort`                          | Accepted effort levels                              |
| ----------------- | --------------------------------------------- | --------------------------------- | --------------------------------------------------- |
| `claude`          | `--model`                                     | `--effort`                        | `low`, `medium`, `high`, `xhigh`, `max`             |
| `codex`           | `-m`                                          | `-c model_reasoning_effort="…"`   | `minimal`, `low`, `medium`, `high`                  |
| `grok`            | `--model`                                     | `--reasoning-effort`              | whatever the selected reasoning model accepts       |
| `copilot`         | `--model`                                     | `--effort`                        | `minimal`, `low`, `medium`, `high`, `xhigh`, `max`  |
| `pi`              | `--model`                                     | `--thinking`                      | `minimal`, `low`, `medium`, `high`, `xhigh`, `max`  |
| `opencode`        | session-message `model` (needs `provider/model`) | session-message `variant`      | provider-specific                                   |
| `cursor`, `acp:*` | `acpx --model`                                | not expressible                   | -                                                   |
| `rovodev`         | not expressible                               | not expressible                   | -                                                   |
| `antigravity`     | not expressible                               | not expressible                   | -                                                   |

`opencode` needs the `provider/model` form (for example `openai/gpt-5`) because its session API takes the provider and the model as separate fields; a bare model name is refused at config load rather than dropped. Both of its knobs travel in the session message, not in the launch command, because `opencode serve` exits with usage on an unknown flag.

`rovodev` and `antigravity` have no mechanism no-mistakes can set - `acli rovodev serve` plus its REST session API take no model parameter, and the `agy` CLI parses flags strictly - so `agent_config` for them is a config error rather than a request that quietly does nothing. Reach for [`agent_args_override`](#agent_args_override) there if your build of the CLI accepts a flag. Reasoning effort is likewise unavailable for ACP targets: no-mistakes drives them through `acpx`, which exposes `--model` but no effort surface.

`agent_config` is global-only. Like `agent_args_override`, it decides which model runs with your credentials, so an `agent_config` block in a repository's `.no-mistakes.yaml` is ignored.

**Precedence.** `agent_args_override` always wins. If a raw flag already pins a knob natively - for example, `-m`, `--model`, or a `-c`/`--config` assignment whose exact key is `model` or `model_reasoning_effort` for Codex, plus the other harnesses' `--effort`, `--reasoning-effort`, or `--thinking` forms - then `agent_config` does not emit its value for that knob. Text such as `model=` nested inside an unrelated option's value is not a pin. Any knob the raw flags leave alone still comes from `agent_config`, so adding `agent_config` to an existing configuration never changes the arguments that configuration already supplied:

```yaml
agent_config:
  codex:
    model: gpt-5.4   # ignored: the raw -m below pins it
    effort: low      # applied: nothing raw pins reasoning depth
agent_args_override:
  codex:
    - -m
    - o3
```

### agent_args_override

Extra CLI flags to pass to each native agent.
Use this for anything [`agent_config`](#agent_config) does not cover - service tier, permission mode, profiles, or any other flag the underlying agent supports - and as the escape hatch for a harness whose model or effort flag no-mistakes cannot map. For model and reasoning effort on a mapped harness, prefer `agent_config`: one spelling instead of seven.

|         |                                                           |
| ------- | --------------------------------------------------------- |
| Type    | `map[string][]string`                                     |
| Keys    | `claude`, `codex`, `grok`, `rovodev`, `opencode`, `pi`, `copilot`, `antigravity` |
| Default | Empty (no extra flags)                                    |

User-supplied flags are normally inserted ahead of no-mistakes' managed flags, so your choices usually take precedence. Security suppression selected by trusted [`disable_project_settings`](/no-mistakes/reference/repo-config/#disable_project_settings) may be placed first while preserving a compatible operator pin. A few flags are reserved because no-mistakes depends on them to communicate with the agent - setting any of these returns a config error on load:

| Agent      | Reserved flags                                                                                              |
| ---------- | ----------------------------------------------------------------------------------------------------------- |
| `claude`   | `-p`, `--print`, `--verbose`, `--output-format`, `--json-schema`, `-r`, `--resume`, `--session-id`, `-c`, `--continue`, `--fork-session` |
| `codex`    | `exec`, `resume`, `--resume`, `--session`, `--session-id`, `--thread`, `--thread-id`, `--last`, `--json`, `--color` |
| `grok`     | `-p`, `--single`, `--prompt-file`, `--prompt-json`, `--output-format`, `--json-schema`, `-r`, `--resume`, `-c`, `--continue`, `--fork-session`, `--session-id`, `--system-prompt-override`, `--system-prompt`, `--rules`, `--append-system-prompt`, `--agent`, `--agents`, `--verbatim`, `--no-subagents`, `--no-auto-update`, `--cwd`, `--restore-code`, `--worktree`, `--worktree-ref` |
| `rovodev`  | `rovodev`, `serve`, `--disable-session-token`                                                               |
| `opencode` | `serve`, `--hostname`, `--port`, `--print-logs`                                                             |
| `pi`       | `--mode`, `--no-session`, `-c`, `--continue`, `-r`, `--resume`, `--session`, `--session-id`, `--fork`     |
| `copilot`  | `-p`, `--prompt`, `--output-format`, `--no-color`                                                          |
| `antigravity` | `--dangerously-skip-permissions`, `--print`, `--json-schema`, `--output-format`, `--conversation`, `-c`, `--continue` |

For structured `codex` runs, no-mistakes also appends its own `--output-schema <tempfile>` after your overrides. Treat that flag as managed even though config validation does not currently reject it.
The Claude, Codex, Grok, Pi, and Antigravity session-control forms are reserved so no-mistakes can keep review-loop conversations deterministic: review turns stay session-free while the fixer keeps its own isolated durable session.

Smart defaults:

- For `claude`, supplying `--permission-mode` (or `--dangerously-skip-permissions`) suppresses the default `--dangerously-skip-permissions`.
- For `codex`, supplying `--ask-for-approval`, `--sandbox`, or `--dangerously-bypass-approvals-and-sandbox` suppresses the default `--dangerously-bypass-approvals-and-sandbox`.
- For `grok`, supplying `--permission-mode` or `--always-approve` suppresses the default `--permission-mode bypassPermissions`. No model flag is added: Grok uses its current configured default unless you explicitly set `-m` or `--model`.
- For `antigravity`, supplying `-t` or `--print-timeout` suppresses the default `--print-timeout 24h`.

Permission and sandbox flags affect the underlying agent, but they do not disable no-mistakes' pipeline prompt steering.
Pipeline agents are still told to keep intentional writes inside the worktree and avoid mutating system state outside it.

Example:

```yaml
agent_args_override:
  claude:
    - --model
    - sonnet
    - --permission-mode
    - acceptEdits
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"
  grok:
    - --reasoning-effort
    - high
  rovodev:
    - --profile
    - work
  pi:
    - --provider
    - google
```

Do not put a model flag under `opencode` here: these flags go to `opencode serve`, which exits with usage on an unknown option. Use `agent_config.opencode.model` instead.

For Codex, `service_tier` and reasoning effort tune different things: `service_tier` selects the speed or priority lane, while reasoning depth is what [`agent_config`](#agent_config)'s `effort` sets (as `-c model_reasoning_effort`). no-mistakes reloads global config while setting up each run, so edits made before `no-mistakes axi run` apply to that run. For repeatable profiles, use separately initialized `NM_HOME` directories; each has its own `config.yaml` and no-mistakes state.

### forge_profiles

Optional machine-local routing for repositories that use different GitHub or GitLab identities. Keys are the raw host tokens recorded in the repository remote, including SSH aliases such as `github-personal`. Each entry must set exactly one provider config directory:

```yaml
forge_profiles:
  github-personal:
    gh_config_dir: ~/.config/gh-personal
  github-work:
    gh_config_dir: /Users/you/.config/gh-work
  gitlab-work:
    glab_config_dir: ~/.config/glab-work
    expected_login: team-bot
```

Paths must be absolute or begin with `~/`; environment variables and other shell expansion are not supported. Host keys are case-insensitive.

When a repository matches a profile, no-mistakes validates it before starting the pipeline and applies an immutable environment to every subprocess in that run: built-in provider commands, custom shell commands, agents, managed agent servers, and the run's Git commands together with any hooks or credential helpers they spawn. A GitHub profile sets `GH_CONFIG_DIR` and removes higher-precedence GitHub token, host, and repository variables; a GitLab profile does the equivalent for `GLAB_CONFIG_DIR` and GitLab variables. The daemon process environment is never changed.

Each selected GitHub config must contain the target host and exactly one account for it, with that account active. A selected GitLab config must contain the target host. An optional `expected_login` pins the account name the profile must be signed in as; resolution fails closed when the config's active login differs or is missing, so a swapped or re-authenticated config directory can never route a run through the wrong account. It carries an account name only, never credentials. `no-mistakes doctor` validates every configured profile and its online authentication.

Profile activation is provider-specific and fail-closed: after at least one GitHub profile is configured, a GitHub repository must match a GitHub profile; GitLab remains ambient unless a GitLab profile is also configured, and vice versa. With no `forge_profiles`, provider detection and ambient CLI authentication behave exactly as before.

For a GitHub fork, no-mistakes considers both the parent and fork host tokens. A match on either side is sufficient. If both match, they must select the same account: the same effective provider config directory *and* the same `expected_login` pin. Otherwise startup fails as ambiguous, so two host tokens sharing a config directory while pinning different logins can never silently resolve to one of them. Fork PR topology itself is unchanged.

Deliberate scope boundaries, so profiles never duplicate what other layers own:

- **Commit identity stays with Git.** Author and committer for pipeline fix commits come from the effective Git configuration (for example remote-keyed `includeIf` sections), which resolves naturally inside run worktrees. Profiles carry no name/email fields.
- **Two accounts on the same host are distinguished by remote host tokens.** Give each account its own SSH alias (`github-personal`, `github-work`) and key a profile per alias; a profile cannot disambiguate two accounts behind one identical remote URL.
- **Executable selection stays with the machine.** Which `gh`, `glab`, or `git` runs is owned by `PATH` and the existing command resolution, not by profile configuration.
- **Credential-helper context stays with Git configuration.** Profiles point at provider CLI config directories and never model or store credential material; credentials remain in the CLI's own store.

### ci_timeout

How long the CI step monitors an open PR, including provider CI status and on GitHub, GitLab, Forgejo, or Azure DevOps PR mergeability, before giving up.

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days)                                 |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor no matter how long it stays open.
If it later develops an actual GitHub, GitLab, Forgejo, or Azure DevOps merge conflict, the CI auto-fix path rebases it, revalidates from Review because rebasing cannot prove continuity with the reviewed head, and publishes it through Push, while a clean behind PR needs no command.
A genuinely idle/abandoned PR still parks at an approval gate after the timeout elapses.
While that CI gate is parked, the daemon continues bounded read-only PR-state checks.
If the PR is merged or closed externally, the stale gate completes automatically; an open, unknown, or temporarily unreachable PR remains parked for a user decision.

Set it to `unlimited` (`none`, `off`, and `never` are accepted aliases), `0`, or any non-positive duration to monitor until the PR is merged, closed, or the run is aborted with `no-mistakes axi abort --run <id>`.

Legacy alias: `babysit_timeout`.

### step_quiet_warning

How long a running or fixing step can go without recorded step-log or native-agent lifecycle activity before AXI status marks the step as quiet.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `10m`                  |

Accepts any positive Go `time.ParseDuration` string: `30s`, `5m`, `1h`, etc.
Non-positive values are ignored and keep the default.

This is observability only.
It does not cancel the step, change auto-fix behavior, or mark the run failed.
AXI renders the quiet signal in the `active_steps` table as part of `last_activity`, for example `quiet 12m3s ago: codex started pid=4242`.
For older active runs that do not yet have activity rows, AXI falls back to the step log file's modification time.

### agent_timeout

Maximum wall-clock time for one pipeline agent invocation that does not already have a more specific deadline.
This is the default-by-construction budget: Document, Lint, Rebase conflict repair, PR drafting, CI auto-fix, and any future agent-spawning step are bounded even if they forget to install their own timer.
Review still uses [`review_agent_timeout`](#review_agent_timeout) as a per-round budget, Test still uses [`test_agent_timeout`](#test_agent_timeout) per invocation, and Intent keeps its five-minute extraction cap; any existing deadline is honored rather than capped.
When this deadline expires, the agent is cancelled and the invocation returns a timeout diagnostic instead of remaining active indefinitely. Most agent-driven mutation steps fail the run, CI auto-fix parks for a user decision, and PR drafting follows its existing agent-error fallback and continues with deterministic content. The [CI step reference](/no-mistakes/reference/pipeline-steps/#ci) owns the approval behavior.
A late successful return after the deadline is rejected, so post-agent commits and PR content cannot use work from a timed-out turn.

The diagnostic reports what was actually measured, not the budget restated. Evidence resets whenever a retry or fallback starts a replacement attempt, including provider fallback, failed session resume, and OpenCode's prompt-only structured-output fallback, so the diagnostic describes only the attempt that reached the deadline:

- `agent produced no output at all in 30m0s after its subprocess started (pid=1234)` - the current attempt launched and then emitted nothing. Check that the agent CLI is authenticated and responsive.
- `agent last produced output 4s ago (312 observed)` - the current attempt was working right up to the deadline. The turn needs a larger budget, or the request is too large for one turn.
- `agent produced no output at all in 30m0s and never reported a subprocess start` - the current attempt never reached a running agent process.

Output means anything observable: streamed assistant text, or raw bytes on the agent subprocess's stdout or stderr. Subprocess bytes matter because an agent spends most of a long turn running tools rather than writing prose, so prose alone cannot tell a working agent from a wedged one.
Any substantive report from the agent adapter - for a native agent, its exit status and captured stderr - is appended to the diagnostic as `agent reported: ...`; credential-bearing URLs are redacted and the report is length-bounded before it can reach logs or findings. A bare context cancellation is omitted because it adds no evidence.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30m`                  |

Accepts any positive Go `time.ParseDuration` string: `5m`, `30m`, `1h`, etc.
Non-positive values are rejected when loading the global config.
Raise it for repositories whose document, lint, rebase, PR, or CI-fix agent turns legitimately run long.
It is global-only: repository config and environment variables cannot override it.

### review_agent_timeout

Maximum wall-clock time for the Review step's agent turns in one review round.
The budget starts at that round's first agent turn and covers its optional review-fix turn plus the rereview turn together; every later auto-fix round starts a fresh budget.
When the deadline expires, the review agent is cancelled and the run fails with a diagnostic naming the timeout instead of remaining active indefinitely.
That diagnostic carries the same measured evidence and adapter report described under [`agent_timeout`](#agent_timeout).

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30m`                  |

Accepts any positive Go `time.ParseDuration` string: `5m`, `30m`, `1h`, etc.
Non-positive values are rejected when loading the global config.
Raise it for repositories whose reviews legitimately run long; it bounds only the Review step, and no other step or environment variable overrides it.

### test_agent_timeout

Maximum wall-clock time for one Test-step agent invocation.
The budget covers the post-test evidence-gathering turn, and a Test-repair turn gets its own budget of the same length.
When the deadline expires, the test agent is cancelled and the run fails with a diagnostic naming the timeout instead of remaining active indefinitely.
That diagnostic carries the same measured evidence and adapter report described under [`agent_timeout`](#agent_timeout).

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30m`                  |

Accepts any positive Go `time.ParseDuration` string: `5m`, `30m`, `1h`, etc.
Non-positive values are rejected when loading the global config.
Raise it for repositories whose targeted tests or evidence gathering legitimately run long; it bounds only the Test step, and no other step or environment variable overrides it.

### daemon_connect_timeout

Maximum time a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging. Guards against a daemon process that is alive but stuck or unresponsive.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `3s`                   |

Accepts any positive Go `time.ParseDuration` string. Overridable per-invocation with the `NM_DAEMON_CONNECT_TIMEOUT` environment variable; see [Environment Variables](/no-mistakes/reference/environment/#nm_daemon_connect_timeout).

### branch_sync_remote_timeout

Maximum time guarded branch synchronization (`sync`, `axi sync`, and the TUI's sync action) waits for each remote Git operation - `ls-remote` or `fetch` - before remote verification fails closed and synchronization is refused.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `60s`                  |

Accepts any positive Go `time.ParseDuration` string.

Raise this if your environment's Git credential helper (for example `gh auth git-credential`, invoked by Git as a child process against a private remote) legitimately takes longer than the default - this is a real, non-outage latency characteristic that has been observed taking 19-22s in some environments, not a hang. It is a machine/environment setting, not a per-repository one: it is read only from global config and has no matching field in a repository's `.no-mistakes.yaml`, so a pushed branch cannot widen or narrow how long the local service waits before failing closed. It never changes the fail-closed guarantee itself - a timeout or unknown remote state still always refuses synchronization without changing files or refs, whatever this value is set to.

### gate_reconcile_interval

How often the daemon rechecks a parked approval gate while waiting for user approval. Today this applies to the CI step's parked gate, which re-probes provider availability (including `gh auth status`) and clears the gate when the PR was merged or closed.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `2m`                   |

Accepts any positive Go `time.ParseDuration` string. Global-only: there is no matching field in a repository's `.no-mistakes.yaml`.

### gate_reconcile_timeout

Maximum wall time one parked approval-gate reconcile attempt may spend before the attempt stops, the gate stays parked, and the next interval wait begins. Covers host probes such as `gh auth status` that can hang without returning.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30s`                  |

Accepts any positive Go `time.ParseDuration` string. Global-only: there is no matching field in a repository's `.no-mistakes.yaml`. Raise this if a legitimate credential helper or network path routinely needs longer than the default for auth probes during reconcile. Timeout and interruption are reported distinctly from authentication failure; that distinction does not require raising this value.

### log_level

Daemon log verbosity.

|         |                                  |
| ------- | -------------------------------- |
| Type    | `string`                         |
| Values  | `debug`, `info`, `warn`, `error` |
| Default | `info`                           |

### session_reuse

Per-run agent session reuse for the review loop's fixer role.

|         |        |
| ------- | ------ |
| Type    | `bool` |
| Default | `true` |

When enabled and the pipeline agent supports native session resume (Claude or Grok via `--resume`, Codex via `exec resume`, Pi via `--session <UUID>`, Antigravity via `--conversation <id>`), each run keeps one durable fixer session across its review-fix turns.
Review turns - the initial full review and every full rereview - always run as fresh, session-free invocations regardless of this setting: a rereview certifies fixes that implement the previous review turn's findings, so it must never resume the session that prescribed them; cross-round review context travels only in the explicit sanitized round history.
The fixer session is never lent to review turns, other pipeline steps stay session-isolated in their own cold invocations, and different runs never reuse identities.
When resume is unavailable or fails, the fix turn falls back to a cold run or a fresh fixer session and the fallback is recorded in the local `agent_invocations` performance record. Pi emits per-invocation usage after a resume, unlike Codex's cumulative session counters.
Session identities are persisted only as minimum local resume metadata, never as prompts or transcripts; Pi's own session directory retains its native transcript. Keep Pi's session directory private, and keep any `--session-dir` or `PI_CODING_AGENT_SESSION_DIR` setting stable while a run is active so a daemon restart can find the fixer session.
The [daemon crash-recovery reference](/no-mistakes/concepts/daemon/#crash-recovery) owns which parked gates can resume or reconcile after a restart.
Set `false` to force every agent invocation cold.

### worktree_roots

Where a repository's pipeline run worktrees are created.

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `map[string]string`                             |
| Keys    | Absolute registered checkout paths (what you ran `no-mistakes init` in) |
| Values  | Absolute directory paths                        |
| Default | Empty (`<NM_HOME>/worktrees/<repo id>/<run id>`) |

By default a run worktree is created under `NM_HOME`, outside every checkout, so directory-scoped toolchain configuration (mise, direnv) never reaches it: those tools resolve their settings by path ancestry.
Point a checkout at a directory of your own and its runs are created at `<value>/<run id>` instead, inheriting whatever that directory configures.
A relative value is rejected at load time, because the daemon that reads it has an unrelated working directory.

The directory stays yours. no-mistakes never enumerates it: the only directories it touches there are the exact ones its own run records name, which is what startup cleanup, orphan-process reaping, and `no-mistakes eject` all go by. Anything else in it - your files, your scratch checkouts, and a directory that merely looks like a run worktree but no run created - is never read, never swept, never signalled, and never removed.

Each checkout needs its own root: two entries pointing at the same directory, two spellings of one checkout, or a root equal to its checkout are rejected at load time, and `init --worktree-root` refuses a directory another checkout already claims.

Two more values are refused at daemon startup, because they cannot work:

- **Inside `NM_HOME`.** It collides with no-mistakes' own state - under `worktrees` a run worktree is indistinguishable from the per-repository directories the default placement owns, and under `logs` a run's worktree *is* its log directory, so removing the worktree at run end would take the run's logs with it.
- **Inside any checkout.** The run worktree is then an untracked directory in that checkout while the run executes, so the checkout is dirty and [branch synchronization](/no-mistakes/reference/cli/#no-mistakes-sync) refuses to move it until the run finishes. That holds whether the victim is the checkout whose own runs land there or an unrelated gated one, so the daemon refuses a root inside any repository it has registered. Registering a repository *around* an already configured root is refused by `no-mistakes init` itself, so you can still place that checkout elsewhere or repoint the entry; anything that reaches the configuration another way is caught at the next daemon start.

Changing an entry affects new runs only.
Each run records the directory it was created in, so editing, adding, or removing an entry never retargets a run that already exists - resuming it after a restart, reading its diff, cleaning it up, reaping processes left standing in it, and ejecting its repository all keep using the directory that run actually has, including after you point the checkout somewhere else.

The key is matched against the checkout path recorded at `init`. After moving a checkout, re-run `no-mistakes init` from the new path and update the key; a key that matches no registered repository is reported in the daemon log at startup and otherwise does nothing.

`no-mistakes init --worktree-root <dir>` prints the exact entry to add for the checkout you are initializing. The global config is hand-maintained, so init never rewrites it for you.

### auto_fix

Maximum follow-up auto-fix attempts per step. Set a step to `0` to disable the follow-up auto-fix loop, so findings require manual approval.
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings then pause for approval instead of starting another automatic fix loop.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field               | Type  | Default | Description                                                                                 |
| ------------------- | ----- | ------- | ------------------------------------------------------------------------------------------- |
| `auto_fix.rebase`   | `int` | `3`     | Rebase conflict auto-fix attempts                                                           |
| `auto_fix.review`   | `int` | `0`     | Review finding auto-fix attempts                                                            |
| `auto_fix.test`     | `int` | `3`     | Test failure auto-fix attempts                                                              |
| `auto_fix.document` | `int` | `3`     | Not used by the automatic document pass                                                     |
| `auto_fix.lint`     | `int` | `3`     | Lint issue auto-fix attempts                                                                |
| `auto_fix.ci`       | `int` | `3`     | CI auto-fix attempts for CI failures, plus GitHub, GitLab, Forgejo, and Azure DevOps merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

### ci.rerun_transient

How many times the CI step may re-run a single provider-attributed check before that check reaches an approval gate.
This covers cancellations on supported providers and, when the value is positive, opts GitHub into detecting jobs that failed before any repository step ran.

| | |
|---|---|
| Type | `int` |
| Default | `0` |
| Range | `0` to `5`; values outside it are clamped |

```yaml
ci:
  rerun_transient: 0
```

Each rerun is another provider-side workflow run billed to the repository being contributed to.
Set `0` here to never spend someone else's CI minutes; this is the only place to make that choice for a repository whose default branch you do not control.

The per-repo [`ci.rerun_transient`](/no-mistakes/reference/repo-config/#cirerun_transient) overrides this value and owns the classification, the trust boundary, and every case that skips the rerun.

### ci.revalidate_repairs

The operator-level fallback for [`ci.revalidate_repairs`](/no-mistakes/reference/repo-config/#cirevalidate_repairs), whose per-repository reference owns the repair-delivery semantics, safety rationale, and trust boundary.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

```yaml
ci:
  revalidate_repairs: false
```

A value in the trusted repository config overrides this global value in both directions: an explicit repository `true` enables revalidation when this is `false`, and an explicit repository `false` disables opt-in revalidation when this is `true`. When the trusted repository config omits the key, this global value applies.

### commit.fix_message

Template for the subject of commits created by the Review, Test, Document, Lint, and CI repair paths.

| | |
| --- | --- |
| Type | `string` |
| Default | `no-mistakes({{.Step}}): {{.Summary}}` |

The template supports literal text and two Go-style placeholders:

| Variable | Value |
| --- | --- |
| `{{.Step}}` | Pipeline step name, such as `review`, `test`, `document`, `lint`, or `ci` |
| `{{.Summary}}` | Sanitized one-line summary returned by the fix agent, or the step's deterministic fallback summary |

The value must be a valid UTF-8 template that renders to a non-empty, single-line commit subject.
The template source is limited to 1,024 bytes and 16 placeholders.
The fix-agent summary and final rendered subject are each limited to 4,096 bytes.
Before rendering, no-mistakes predicts the subject size from the validated literal text and placeholders, then rejects oversized output without allocating the expanded message.
Template functions, control actions, named templates, unknown placeholders, malformed syntax, control characters, unsafe Unicode format characters, and Unicode line or paragraph separators cause configuration loading to fail.
The blocked format set includes every Unicode `Bidi_Control` code point plus `U+00AD`, `U+180E`, `U+200B`, `U+2060` through `U+2064`, the deprecated bidi controls `U+206A` through `U+206F`, `U+FEFF`, `U+FFF9` through `U+FFFB`, and Unicode tag characters in `U+E0000` through `U+E007F`.
Legitimate `U+200C` zero-width non-joiner and `U+200D` zero-width joiner text shaping remains allowed.
The final rendered subject is validated again, so unsafe characters in an agent-provided summary are also rejected.
The setting does not change commit subjects created by the Rebase or Push steps.
A per-repo [`commit.fix_message`](/no-mistakes/reference/repo-config/#commitfix_message) value overrides this global setting.

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, pass that summary to rebase, review, test, document, lint, CI auto-fix, and PR prompts, and include it in generated PR descriptions.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type       | Default | Description                                                |
| ------------------------- | ---------- | ------- | ---------------------------------------------------------- |
| `intent.enabled`          | `bool`     | `true`  | Enable transcript-based intent extraction                  |
| `intent.threshold`        | `float`    | `0.2`   | Minimum raw match score for selecting a transcript session |
| `intent.slack_days`       | `int`      | `3`     | Extra days to look back before the change window           |
| `intent.disabled_readers` | `string[]` | Empty   | Transcript readers to disable                              |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated by the configured pipeline agent.

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts are written to `<NM_HOME>/evidence/<run-id>`. On GitHub.com/GHEC, supported image and video artifacts are also uploaded when the PR is rendered; see `attach_media` below.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                         | Type     | Default                  | Description                                                                |
| ----------------------------- | -------- | ------------------------ | -------------------------------------------------------------------------- |
| `test.evidence.store_in_repo` | `bool`   | `false`                  | Publish test evidence artifacts to the repository's orphan evidence branch |
| `test.evidence.attach_media`  | `bool`   | `true`                   | Upload image and video evidence to GitHub user-attachments when the PR is rendered |
| `test.evidence.dir`           | `string` | `.no-mistakes/evidence`  | Directory prefix inside the evidence branch                                |
| `test.evidence.branch`        | `string` | `no-mistakes/evidence`   | Name of the orphan evidence branch                                         |
| `test.evidence.local_root`    | `string` | `<NM_HOME>/evidence`     | Absolute directory where run evidence is written on local disk             |
| `test.evidence.retention`     | `string` | `336h` (14 days)         | How long a run's evidence survives; `unlimited`/`none`/`off`/`never` or a non-positive duration disables the bound |
| `test.evidence.max_runs`      | `int`    | `200`                    | How many run directories survive regardless of age; `0` disables the bound |

The test step always collects evidence outside the worktree, so artifacts never enter the branch under validation.
On GitHub.com and GitHub Enterprise Cloud, image and video artifacts that pass GitHub's attach rules (png, jpg, jpeg, gif, webp, svg, mp4, mov, webm; images at most 10 MiB and videos at most 100 MiB) are uploaded to GitHub user-attachments when the PR body is rendered, unless `attach_media` is false and `store_in_repo` is also false.
The testing section embeds the returned image markdown or bare video URL so remote reviewers can open the media. Upload is fail-closed: any error, unsupported type, oversize file, GitHub Enterprise Server, non-GitHub forge, or GitHub App/Actions token keeps today's rendering (a commit-pinned evidence-branch link if `store_in_repo` published, otherwise a local path) rather than a dead attachment URL. Text artifacts stay inlined as they are today.
When `store_in_repo` is true for a GitHub repository, the PR step copies that directory onto `branch` under `<dir>/<branch-slug>` in the code branch's push-target repository (the fork when fork routing is configured), pushes it, and links the artifacts from the pull request body.
When both `attach_media` and `store_in_repo` apply, the testing section carries the user-attachments embed in addition to the commit-pinned git link.
The branch is an orphan: it shares no history with your code branches, so evidence never reaches the default branch. Links use the evidence commit rather than the branch, so they keep resolving after later runs.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
`branch` must be a valid Git branch name; an invalid value fails the config with the offending key and value.
The publisher never force-pushes. It appends to the fetched evidence-branch tip with a fast-forward push, retries one lost race, and refuses to use the run branch, default branch, or an existing branch whose tip lacks the `.no-mistakes-evidence` marker.
Publication is also refused when the remote cannot be read or pushed, an artifact exceeds 64 MiB, a run exceeds 500 files or 256 MiB, or another writer wins the retry. The PR body then keeps its local rendering instead of adding links that would not resolve.
Evidence-branch publication currently supports GitHub links only. On other providers, no evidence branch is pushed and the PR body keeps its local rendering.
Enabling this pushes a branch to your remote, so pick a `branch` name your CI workflows do not build.

#### Local storage and cleanup

Evidence lives under the app root rather than the system temp directory. On Linux the daemon runs from a service unit that does not export `TMPDIR`, so the old temp-directory default resolved to the shared `/tmp`, which current Ubuntu mounts as a RAM-backed `tmpfs`. The app root is disk backed on macOS, Linux, and Windows alike.

no-mistakes reaps its recorded run directories itself rather than relying on an operating-system temp cleaner. Unrecognized directories under a custom `local_root` are left untouched.

- A finished run that produced no artifacts leaves nothing behind.
- Recorded run directories older than `retention` are removed.
- Whatever recorded run evidence survives is trimmed to `max_runs`, oldest first.
- A run that is still pending or running is never touched.

Reaping runs after each finished run and again at daemon startup. An upgraded daemon also drains the pre-relocation directory in the system temp directory under the same rules; nothing is migrated, because absolute paths recorded in older pull request bodies name the old location.

`local_root` must be an absolute path outside `<NM_HOME>/worktrees`; a relative or managed-worktree path fails daemon startup and prevents new or recovered runs from starting. Because `retention` bounds how long a PR body's local artifact links keep resolving, raise it rather than lowering it if your reviews run long.

The publication fields are global defaults. Repo config can override `store_in_repo`, `attach_media`, and `dir`; it can override `branch` only through the trusted default-branch copy. `local_root`, `retention`, and `max_runs` are global-only: a repository does not get to name a filesystem path this machine's daemon writes to, or set the retention budget for a directory every repository on the machine shares.

### eval

Local review-evaluation corpus settings for [`no-mistakes eval`](/no-mistakes/reference/eval/).

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                      | Type   | Default | Description                                                            |
| -------------------------- | ------ | ------- | ---------------------------------------------------------------------- |
| `eval.capture_provenance`  | `bool` | `true`  | Record the exact commit and configuration inputs a replay needs        |
| `eval.auto_capture`        | `bool` | `true`  | Freeze eligible finished runs' review passes into the local corpus     |
| `eval.max_cases`           | `int`  | `200`   | Retention target for automatic collection; `0` keeps every case        |
| `eval.diversified_size`    | `int`  | `32`    | Cap on the official gold-only `diversified` set; `0` is one gold case per stratum |

`capture_provenance` is what makes a review pass replayable at all. It is recorded when the round is written and cannot be added afterwards, because the pinned configuration is a point-in-time snapshot, so a run reviewed with it off can never be captured later.

`auto_capture` collects those passes without any command: when an eligible run finishes, its decided review rounds become cases. It does nothing while `capture_provenance` is off. Collection runs after the pipeline has already reported its outcome and can never change it; a failure is logged and nothing else.

`max_cases` sets the retention target enforced after automatic collection. When it is exceeded the oldest unprotected cases are dropped first. A case with a replay in progress or recorded candidate replays is protected, so the corpus can remain above the target rather than invalidate a comparison you have spent tokens on. Cases from the same repository share one local object pool, so a case costs its own records plus the objects its commits introduced rather than a copy of the repository.

`diversified_size` caps the official gold-only eval set used by `eval run --cases diversified`. Selection is stratified and pinned; unlabeled cases never fill it. `0` keeps one gold case per stratum with no Hamilton bound. Corpus retention (`max_cases`) and this official-set cap are different knobs.

These are operator settings for this machine's local disk, so they are global-only: an `eval` block in a repository's `.no-mistakes.yaml` is ignored. Corpus storage stays under `<NM_HOME>/eval` and no-mistakes never uploads it; replay still sends code to the selected agent's configured model provider as described in the [Evaluation toolkit](/no-mistakes/reference/eval/).

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, `NM_DAEMON_CONNECT_TIMEOUT`, Forgejo host and token settings, Bitbucket Cloud credentials, and update-check suppression.
