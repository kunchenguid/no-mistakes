---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: omp

forgejo_axi_path: forgejo-axi

agent_path_override:
  omp: /Users/you/.bun/bin/omp

agent_config:
  omp:
    model: dashscope/qwen4-max
    effort: xhigh

agent_args_override:
  omp:
    - --service-tier
    - priority

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

provider_plugins:
  ssm:
    command: ~/bin/nm-ssm-plugin
    hosts: ["*.sourcemanager.dev"]

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
  review_bot_comments: on_failure # or: always
  rebase_behind: false
  carry_review_on_replay: false
  release_after_checks_pass: false
  required_reviews: []

rebase:
  strategy: rebase # or: merge

review:
  path_instructions: []

commit:
  fix_message: "chore(no-mistakes-{{.Step}}): {{.Summary}}"
  # branch_pattern: '^PROJ/([0-9]+)$'
  # branch_replacement: 'PROJ-${1}'
  # To use the captured identifier in the subject:
  # fix_message: "{{.Branch}}: {{.Summary}}"
  # trailers:
  #   - "Assisted-by: no-mistakes:{{.Agent}}:{{.Model}}"

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []
  # publish_intent: false # Keep the generated Intent section out of PR bodies by default
  # check:
  #   mode: warn # off (default) | warn | reject
  #   terms: ["internal project label"]
  #   patterns: ['(?i)run-[a-z0-9-]+']

test:
  evidence:
    store_in_repo: false
    attach_media: true
    compact: true
    dir: .no-mistakes/evidence
    branch: no-mistakes/evidence
    commit_message: "no-mistakes: evidence for {{.Branch}} (run {{.RunID}})"
    retention: 336h
    max_runs: 200

providers:
  github:
    draft_pull_requests: false
  gitlab:
    draft_pull_requests: false
  bitbucket:
    draft_pull_requests: false
  azuredevops:
    draft_pull_requests: false
  origin:
    draft_pull_requests: false
```

## Fields

### agent

|         |                 |
| ------- | --------------- |
| Type    | `string`        |
| Values  | `omp`           |
| Default | `omp`           |

This build supports exactly one pipeline agent: the native omp (Oh My Pi) CLI.
There is no `auto` probing, no ordered fallback list, and no ACP target. Every
pipeline step uses the same omp configuration.

A repository may set `agent: omp`; any other value fails the run at setup.
`no-mistakes doctor` checks the global configuration, while every run repeats
resolution after applying a trusted repository-level `agent` value.

### acpx_path

Accepted for configuration compatibility and ignored. ACP agents are not part
of this build.

### forgejo_axi_path

Executable used for Forgejo PR and CI operations.

|         |               |
| ------- | ------------- |
| Type    | `string`      |
| Default | `forgejo-axi` |

A bare name is resolved from the daemon's effective `PATH`; an explicit path is executed directly. See [Provider Integration](/no-mistakes/guides/provider-integration/#forgejo) for setup and the [environment reference](/no-mistakes/reference/environment/#forgejo_base_url) for host and token configuration.

### acp_registry_overrides

Accepted for configuration compatibility and ignored. This build has no ACP
targets.

### agent_path_override

Custom binary paths. Only the `omp` key is used.

|         |                                   |
| ------- | --------------------------------- |
| Type    | `map[string]string`               |
| Default | Empty (resolves `omp` on `PATH`)  |

```yaml
agent_path_override:
  omp: /Users/you/.bun/bin/omp
```

### agent_config

Model and reasoning effort for omp.

|         |                                             |
| ------- | ------------------------------------------- |
| Type    | `map[string]{model, effort, quota_provider}` |
| Keys    | `omp`                                       |
| Default | Empty (omp keeps its own defaults)          |

```yaml
agent_config:
  omp:
    model: dashscope/qwen4-max
    effort: xhigh
```

`effort` is one of `minimal`, `low`, `medium`, `high`, `xhigh`, `max` and is
passed to omp as `--thinking`. `model` is passed as `--model`. `agent_config` is
global-only; a repository block is ignored.

**Precedence.** A raw `agent_args_override.omp` flag that already pins
`--model` or `--thinking` wins, and `agent_config` does not emit that knob.

### Per-run reviewer profiles

Removed. This build has no reviewer/fixer role split: a per-run `--model` /
`--effort` pin, when supplied, applies to every pipeline step.

### Per-run run profiles

A `--model`/`--effort` pair on `axi run` (or `axi rerun`) pins one omp model and
effort for that run, immutable after creation. Omitted fields inherit
`agent_config.omp`. See [`axi run`](/no-mistakes/reference/cli/#no-mistakes-axi-run).

### review_agents

Accepted as a parse-only compatibility tombstone and ignored. There is no
reviewer/fixer role routing in this build; every step uses the one omp
configuration.

### session_reuse

Accepted for configuration compatibility and ignored. Every omp invocation runs
cold; no pipeline turn resumes conversational state.

### quota_routing

Accepted for configuration compatibility and ignored. One omp configuration
serves every step, so there is no agent ordering or mid-run provider switch.

### agent_args_override

Extra CLI flags for omp.

|         |                         |
| ------- | ----------------------- |
| Type    | `map[string][]string`   |
| Keys    | `omp`                   |
| Default | Empty (no extra flags)  |

```yaml
agent_args_override:
  omp:
    - --model
    - dashscope/qwen4-max
    - --thinking
    - xhigh
```

Flags are inserted before no-mistakes' managed flags. A raw `--model` or
`--thinking` pin wins over `agent_config`. Managed flags (`--mode`,
`--no-session`, and session-resume flags) are refused at config load so the
JSONL stream stays parseable.

### forge_profiles

Optional machine-local routing for repositories that use different GitHub or GitLab identities. Keys are the raw host tokens recorded in the repository remote, including SSH aliases such as `github-personal`. A `gh_user` profile key may also name one repository owner as `host/owner`. Each entry must set exactly one provider config directory, or select a signed-in GitHub login with `gh_user`:

```yaml
forge_profiles:
  github-personal:
    gh_config_dir: ~/.config/gh-personal
  github-work:
    gh_config_dir: /Users/you/.config/gh-work
  gitlab-work:
    glab_config_dir: ~/.config/glab-work
    expected_login: team-bot
  github.com/remicaesar:
    gh_user: remicaesar
```

Paths must be absolute or begin with `~/`; environment variables and other shell expansion are not supported. Host keys are case-insensitive.

`forge_profiles` is read from the global config only. Repository config, including the pushed branch's `.no-mistakes.yaml`, can never add, select, or change a profile, because a profile decides whose credentials the run executes with.

When a repository matches a profile, no-mistakes validates it before starting the pipeline and applies an immutable environment to every subprocess in that run: built-in provider commands, custom shell commands, agents, managed agent servers, and the run's Git commands together with any hooks or credential helpers they spawn. A GitHub profile sets `GH_CONFIG_DIR` (or, for a `gh_user` profile, the selected login's token, described below) and removes higher-precedence GitHub token, host, and repository variables; a GitLab profile does the equivalent for `GLAB_CONFIG_DIR` and GitLab variables. The daemon process environment is never changed.

Each selected GitHub config directory (`gh_config_dir`) must contain the target host and exactly one account for it, with that account active. A selected GitLab config must contain the target host. An optional `expected_login` pins the account name the profile must be signed in as; resolution fails closed when the config's active login differs or is missing, so a swapped or re-authenticated config directory can never route a run through the wrong account. It carries an account name only, never credentials. `no-mistakes doctor` validates every configured profile and its online authentication.

#### Choosing one of several gh accounts with `gh_user`

When one gh configuration holds several accounts for the same host, `gh_user` selects one of them by login without a separate config directory or SSH alias. Key the profile by `host/owner` so it applies only to repositories that owner holds:

```yaml
forge_profiles:
  github.com/remicaesar:
    gh_user: remicaesar
```

At run setup, no-mistakes runs `gh auth token --hostname <host> --user <gh_user>` once, against the ambient gh configuration, and gives that token to every subprocess in the run as `GH_TOKEN` (`GH_ENTERPRISE_TOKEN` on a GitHub Enterprise Server host). The other GitHub token, host, and repository variables are removed as for any GitHub profile. gh prefers that variable over its active account, and `gh auth git-credential` returns it, so HTTPS pushes through the gh credential helper authenticate as the same login. The token is held in memory for the run only: it never appears in arguments, logs, files, or the run record, and a recovered run reads it again. A profile never stores a token itself.

If gh has no token for that login on the host (the login is unknown or signed out), the run fails at setup with the gh error; it never falls back to the active account. `gh_user` cannot be combined with `gh_config_dir`, `glab_config_dir`, or `expected_login` (the login it names is already the pin).

An owner-scoped key is looked up before its plain host key, and a repository whose owner has no scoped key falls back to the plain host key, so existing host-keyed profiles keep working unchanged. An owner-scoped key never activates routing for other owners: with only `github.com/remicaesar` configured, repositories other owners hold on `github.com` keep ambient authentication. Owner-scoped keys are supported for `gh_user` profiles only.

Profile activation is provider-specific and fail-closed: after at least one plain host-keyed GitHub profile is configured, a GitHub repository must match a GitHub profile; GitLab remains ambient unless a GitLab profile is also configured, and vice versa. With no `forge_profiles`, provider detection and ambient CLI authentication behave exactly as before.

For a GitHub fork, no-mistakes considers both the parent and fork remotes. A match on either side is sufficient. If one side matches an owner-scoped key and the other only a plain host key, the owner-scoped profile wins, so a contributor's fork owner selects the account that pushes to the fork and opens the PR. Otherwise, if both match, they must select the same account: the same effective provider config directory, the same `expected_login` pin, *and* the same `gh_user`. Otherwise startup fails as ambiguous, so two host tokens sharing a config directory while pinning different logins can never silently resolve to one of them. Fork PR topology itself is unchanged.

Deliberate scope boundaries, so profiles never duplicate what other layers own:

- **Commit identity is independent of forge profiles.** Profiles carry no name/email fields. See the [pipeline commit identity policy](/no-mistakes/reference/pipeline-steps/) for submission binding and author preservation.
- **Two accounts on the same host are distinguished by remote host token or repository owner.** Either give each account its own SSH alias (`github-personal`, `github-work`) and key a profile per alias, or key GitHub profiles by `host/owner` with `gh_user`; a profile cannot disambiguate two accounts behind one identical remote URL.
- **Executable selection stays with the machine.** Which `gh`, `glab`, or `git` runs is owned by `PATH` and the existing command resolution, not by profile configuration.
- **Credential-helper context stays with Git configuration.** Profiles point at provider CLI config directories or name a login, and never model or store credential material; credentials remain in the CLI's own store, and a `gh_user` token is only read from it for the run.

### provider_plugins

Optional machine-local PR and CI support for hosts no-mistakes does not ship a provider for, such as a company-internal forge or Google Cloud Secure Source Manager. Each entry names an AXI-shaped CLI that implements the [provider plugin protocol](/no-mistakes/reference/provider-plugin-protocol/) and the hosts it serves:

```yaml
provider_plugins:
  ssm:
    command: ~/bin/nm-ssm-plugin
    args: ["--location", "us-central1"]
    hosts: ["*.sourcemanager.dev"]
    timeout: 2m
    draft_pull_requests: false
```

| Key | Required | Meaning |
| --- | --- | --- |
| `command` | yes | Executable to run. A bare name is resolved from the run's `PATH`; a path must be absolute or begin with `~/`. Relative paths are refused because plugins run with the run worktree as their working directory. |
| `args` | no | Arguments passed verbatim on every invocation, before the subcommand words (`<command> <args...> pr view 7 ... --json`). PR bodies travel in a private `--body-file`, never in argv, so they never appear in a process listing. |
| `hosts` | yes | Host patterns the plugin claims: an exact host name or SSH alias as it appears in the remote, or `*.<domain>` for any subdomain (not the apex). No scheme, port, path, or user. |
| `timeout` | no | Bound for one plugin invocation (Go duration). Defaults to `2m`. |
| `draft_pull_requests` | no | Passes `--draft` to `pr create`. Defaults to `false`. |

Names are 1-63 lowercase letters, digits, `-`, or `_` and must not reuse a built-in provider name. A run's provider shows up as `plugin:<name>`.

Selection happens per run, before built-in detection and forge profiles: the remote's literal host token is matched first, then its SSH `HostName` resolution. An exact pattern beats a wildcard, and a longer wildcard beats a shorter one. Two plugins (or one plugin twice) may not list the same pattern, and a host that a [`forge_profiles`](#forge_profiles) entry claims may not also be claimed by a plugin; both are configuration errors. Because a forge profile matches the remote's literal host while a plugin also matches its SSH `HostName`, an overlap that only appears after alias resolution (a profile keyed by an SSH alias whose `HostName` a plugin claims) is refused when the run starts, with an error naming both.

`provider_plugins` is global-only. A plugin runs with your credentials for every repository on its hosts, so a repository `.no-mistakes.yaml` can never add, change, or redirect one; a `provider_plugins` block there is ignored.

Plugins run through the same environment as built-in provider CLIs (the daemon's login-shell environment plus any forge-profile overlay) with the run worktree as the working directory. A plugin whose `status` exits non-zero (for example, not authenticated) makes the PR and CI steps skip with its message, reported under `run.automatic_skips`; a plugin that breaks the protocol (unreadable output, wrong protocol version, timeout) fails the step instead, in its `status` handshake or in any later call (a timeout during the CI step's repeated polls or log retrieval is handled like any failed read instead). `no-mistakes doctor` lists configured plugins and checks that each command resolves; it does not run the plugin, because the handshake is per repository.

### ci_timeout

How long the CI step waits for provider CI status to settle before giving up, and how long it keeps monitoring an open PR after every check is green.

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days)                                 |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor no matter how long it stays open; under [`ci.release_after_checks_pass`](#cirelease_after_checks_pass) that watch ends at the checks verdict instead.
If it later develops an actual merge conflict (on GitHub, GitLab, Forgejo, Azure DevOps, Cursor Origin, or a provider plugin declaring `mergeable_state`), the CI auto-fix path rebases it, revalidates from Review because rebasing cannot prove continuity with the reviewed head, and publishes it through Push.
A clean behind PR needs no command, except on GitHub when the base requires up-to-date branches (`mergeStateStatus: BEHIND`): that PR waits for a human to bring it up to date, unless [`ci.rebase_behind`](#cirebase_behind) is on.
A genuinely idle/abandoned PR still parks at an approval gate after the timeout elapses.
While that CI gate is parked, the daemon continues bounded read-only PR-state checks.
If the PR is merged or closed externally, the stale gate completes automatically; an open, unknown, or temporarily unreachable PR remains parked for a user decision.

Set it to `unlimited` (`none`, `off`, and `never` are accepted aliases), `0`, or any non-positive duration to disable this timeout. A run still ends when the PR is merged, closed, or declined, when green monitoring is explicitly handed off, or at readiness under `ci.release_after_checks_pass`. You can end it explicitly with `no-mistakes axi handoff` or cancel it with `no-mistakes axi abort --run <id>`.

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

### agent_crash_retries

Opt-in automatic retries of an agent-crashed Review, Test, Document or Lint step in the **same live run**. Completed steps stay completed. Set `agent_crash_retries: 1` to allow one additional attempt; the default `0` preserves the existing failure behavior.

|         |                  |
| ------- | ---------------- |
| Type    | integer          |
| Range   | `0`–`3`          |
| Default | `0` (disabled)   |

Agent process exits and agent invocation timeout cuts qualify, never a real finding, test verdict, configured-command failure, Git error, or cancellation. Typed terminal provider signals, including authentication/access-denial errors and quota exhaustion, stay terminal. Adapters that give no typed terminal signal (Pi, Grok) can have an auth or quota failure retried up to `agent_crash_retries` times before failing with the original diagnostic. Existing adapter and analyzer formatting retries run first; exhausted adapter-transient failures and Claude's native structured-output retry exhaustion stay terminal. Each additional attempt must still produce a valid result; a crash is never a pass.

Eligible validation steps also pin their freshly resolved diff base across the live run: a later fetch that changes that base fails before invoking another agent, even if the run's recorded previous-push SHA did not change. A full restart at Review establishes a new base pin.

Before retrying, the executor proves that the run and worktree head SHA, recorded base SHA, effective configuration, completed-step records/rounds and evidence-file hashes still match their pre-attempt snapshot. The worktree must be clean before and after the attempt, including untracked and ignored files. Missing or changed proof refuses the retry and fails normally: no in-run restart is attempted; start a fresh run for full validation from step 1. Partial edits, commits or evidence from a crashed attempt are never silently discarded or certified. The log records the `agent_invocation` retry or proof refusal; an exhausted retry still fails with its agent diagnostic.

A hang cut by the default invocation budget can qualify. A non-default agent timeout, or any still-working cap, disables timeout-based crash retries; the shipped timeout values count as unconfigured even when the generated config writes them explicitly. Process exits can still qualify. Test budget cuts that already park for an operator decision remain parked, not automatically retried.

Fix rounds, review-answer/coverage continuations, conflict resolution, custom gates, Intent and Push/PR/CI delivery are not replayed or retried. This budget is separate from `auto_fix`, is not replenished by approval rounds, and is disabled for the remainder of a run resumed after daemon recovery; a later agent crash fails that step normally. It does not continue a terminal run: [`axi retry`](/reference/cli/#axi-retry) is the separate manual continuation, using the shared `agent_invocation` cause and its own checkpoint proofs. Timeout cuts do not acquire that cause, and parser rejections remain terminal. It is global-only: repository config and environment variables cannot enable it.

### max_concurrent_runs

Opt-in cap on how many pipeline runs execute at once on this machine. When the cap is reached, newly submitted runs wait in the queue in submission order with their run row pending, and start as running runs finish, fail, or are cancelled, instead of all starting at once and competing for host memory and CPU.

|         |                        |
| ------- | ---------------------- |
| Type    | integer                |
| Range   | `0` or greater         |
| Default | `0` (unlimited)        |

`0` or unset means unlimited. An admitted run holds its slot until its executor finishes, fails, or is cancelled, including while parked at an approval gate. Recovered parked runs use the same cap and wait for a slot before resuming. Cancelling a queued run removes it from the queue without starting or resuming it.

Fresh queued runs do not survive a daemon restart and must be resubmitted. Recovered parked runs waiting for a slot remain recoverable across a clean shutdown. If the global configuration cannot be read, the daemon keeps the last successfully loaded cap. It is global-only: repository config cannot set it.

### agent_timeout

Stall budget for one pipeline agent invocation that does not already have a more specific deadline.
This is the default-by-construction budget: Document, Lint, Rebase conflict repair, PR drafting, CI auto-fix, and any future agent-spawning step are bounded even if they forget to install their own timer.
Review still uses [`review_agent_timeout`](#review_agent_timeout) for each review or fix invocation, Test still uses [`test_agent_timeout`](#test_agent_timeout) per invocation, and Intent keeps its five-minute extraction cap; any existing deadline is honored rather than capped.
A silent invocation is cancelled when this budget expires.
An invocation that is still producing output at expiry, or whose agent subprocess is still running a child process it started after its last output, such as a test suite a tool call launched, continues only when [`agent_working_timeout`](#agent_working_timeout) is set.
It may then continue until it goes quiet with no live child process for 10 minutes (the shipped [`step_quiet_warning`](#step_quiet_warning) default, fixed regardless of that setting; this budget itself when it is shorter) or until that cap, whichever comes first.
With the cap unset, the turn stops at this budget.
There is no hidden multiple of this budget.
When the invocation is cancelled, it returns a timeout diagnostic instead of remaining active indefinitely.
Most agent-driven mutation steps fail the run, CI auto-fix parks for a user decision, and PR drafting follows its existing agent-error fallback and continues with deterministic content.
The [CI step reference](/no-mistakes/reference/pipeline-steps/#ci) owns the approval behavior.
A late successful return after cancellation is rejected, so post-agent commits and PR content cannot use work from a timed-out turn.

The diagnostic names which bound cut the invocation and how long it ran, and separately reports what activity was actually measured.
With no still-working cap set it reads `after 30m0s (silent budget; no still-working cap is set; ran 30m0s)`, which claims nothing about output or child processes because nothing checked them.
With a cap set it reads `after 30m0s (stall budget, then no recent output or live child process; ran 42m0s)` for a turn that went idle, or `at its 1h0m0s still-working cap (silent budget 30m0s; ran 1h0m0s)` for a turn that reached the cap.
Evidence resets whenever a retry or fallback starts a replacement attempt, including provider fallback, failed session resume, and OpenCode's prompt-only structured-output fallback, so the diagnostic describes only the attempt that reached the deadline:

- `agent produced no output at all in 30m0s after its subprocess started (pid=1234)` - the current attempt launched and then emitted nothing. Check that the agent CLI is authenticated and responsive.
- `agent last produced output 4s ago (312 observed)` - the current attempt was working right up to cancellation. Set the still-working cap, raise it if it is already set, or split a request that is too large for one turn.
- `agent produced no output at all in 30m0s and never reported a subprocess start` - the current attempt never reached a running agent process.

Output means anything observable: streamed assistant text, or raw bytes on the agent subprocess's stdout or stderr.
Subprocess bytes matter because an agent spends most of a long turn running tools rather than writing prose, so prose alone cannot tell a working agent from a wedged one.
A live child process of the agent subprocess extends the budget too, because a long tool call writes nothing until it returns, but it is liveness rather than output and is never reported as the agent having produced anything.
The agent's child processes are sampled about once a second, and each output freezes a sample taken before it as the baseline; only a child missing from that baseline counts.
A tool the agent launches right after announcing it, even in its very first output, therefore still extends the budget.
Helpers it keeps alive for the whole turn, such as the ACP agent under `acpx` or stdio MCP servers, are in the baseline once a sample taken after they started precedes an output, so they cannot keep a hung turn alive past the stall budget.
A helper started less than about a second before the agent's first output, with no output after it, cannot be told apart from a tool that output announced, so it counts as work and can hold a hung turn open until the still-working cap.
Agents that report no subprocess, and hosts where the process table cannot be read, get no such extension.
[`step_quiet_warning`](#step_quiet_warning) remains a separate status-only signal; configuring it does not change the 10-minute quiet window that cancels a previously-working turn after the stall budget.
Any substantive report from the agent adapter - for a native agent, its exit status and captured stderr - is appended to the diagnostic as `agent reported: ...`; credential-bearing URLs are redacted and the report is length-bounded before it can reach logs or findings.
A bare context cancellation is omitted because it adds no evidence.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30m`                  |

Accepts any positive Go `time.ParseDuration` string: `5m`, `30m`, `1h`, etc.
Non-positive values are rejected when loading the global config.
Raise it when those turns routinely stay quiet longer than this budget.
With [`agent_working_timeout`](#agent_working_timeout) unset, this budget also stops a turn that is still working, so set that cap when busy turns are cut here.
It is global-only: repository config and environment variables cannot override it.

### agent_working_timeout

Optional cap for one invocation that is still producing output, or waiting on a live child, after [`agent_timeout`](#agent_timeout) expires.
Unset means no extension: the turn stops at `agent_timeout`, including when it is still working.
Set, it is the absolute deadline for that still-working turn, not a multiple of the silent budget.
The 10-minute quiet stop still cancels a turn that goes idle before the cap.
The value must be at least `agent_timeout`.
[`review_agent_working_timeout`](#review_agent_working_timeout) and [`test_agent_working_timeout`](#test_agent_working_timeout) are the same cap for Review and Test.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | unset                  |

Accepts any positive Go `time.ParseDuration` string.
Non-positive values are rejected when loading the global config.
It is global-only.

### review_agent_timeout

Stall budget for **one** Review-step agent invocation.
The optional fixer gets the full configured budget, and its fresh, session-free independent rereviewer gets a new full budget of its own.
Every later fixer and rereviewer does the same; no invocation inherits time spent by an earlier turn.
A silent invocation is cancelled when this budget expires.
A still-working invocation continues only when [`review_agent_working_timeout`](#review_agent_working_timeout) is set, under the same idle rule as [`agent_working_timeout`](#agent_working_timeout).
When the invocation is cancelled, the review agent fails the run with a diagnostic naming the bound that cut it instead of remaining active indefinitely.
That diagnostic carries the same measured evidence and adapter report described under [`agent_timeout`](#agent_timeout).
A repository can tighten this value for itself with the trusted [`review.agent_timeout`](/no-mistakes/reference/repo-config/#reviewagent_timeout) key; the smaller of the two applies, and a repository can never raise it.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30m`                  |

Accepts any positive Go `time.ParseDuration` string: `5m`, `30m`, `1h`, etc.
Non-positive values are rejected when loading the global config.
Raise it when reviews routinely stay quiet longer than this budget.
With [`review_agent_working_timeout`](#review_agent_working_timeout) unset, this budget also stops a review that is still working, so set that cap when busy reviews are cut here.
It bounds only the Review step, and no other step or environment variable overrides it.

### review_agent_working_timeout

Optional still-working cap for one Review invocation.
Unset means the turn stops at [`review_agent_timeout`](#review_agent_timeout).
Set, it is that turn's absolute deadline, and it must be at least `review_agent_timeout`.
The 10-minute quiet stop still applies.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | unset                  |

It is global-only.

### test_agent_timeout

Stall budget for one Test-step agent invocation.
The budget covers the post-test evidence-gathering turn, and a Test-repair turn gets its own budget of the same length.
A silent invocation is cancelled when this budget expires.
A still-working invocation continues only when [`test_agent_working_timeout`](#test_agent_working_timeout) is set, so a targeted run that already needs most of this budget is not failed solely because the provider was slow.
When the invocation is cancelled, the test agent parks for a decision with an ask-user finding rather than failing the run as a code defect.
That finding carries the same measured evidence and adapter report described under [`agent_timeout`](#agent_timeout).
A late structured result from the expired turn is still not used as a successful Test pass.
The park keeps the configured `commands.test` result from the same execution, so approving over a failing command is still recorded as a configured-command override.
A cut fix round also keeps the findings of the gate it was answering, selected or not, and the last completed evidence turn's verdict, so approving it is recorded against that verdict.
A commit the timed-out agent already made is recorded locally for custody and is not pushed, unless an unfinished rebase or merge leaves only a partial HEAD.
While the run worktree holds uncommitted changes or commits past the head the last completed evidence turn saw (before one completes, past the head the first cut measured from, which each later park carries forward and measures again), the park names them with the commands to inspect them and approval is refused, because the steps after Test would commit and publish them.
Otherwise approving the park is a Test exception (`passed-with-override`), not a silent green pass.
A fix response spends another budget: a repair turn runs only for selected findings other than the budget cut itself, then validation re-runs over whatever the cut left.
Guidance you attach to the budget-cut finding itself (`axi respond --instructions`, or `e` in the TUI) is given to that re-run validation.
You can also abort, raise this value, and retry.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `30m`                  |

Accepts any positive Go `time.ParseDuration` string: `5m`, `30m`, `1h`, etc.
Non-positive values are rejected when loading the global config.
Raise it when targeted tests or evidence gathering routinely stay quiet longer than this budget.
With [`test_agent_working_timeout`](#test_agent_working_timeout) unset, this budget also stops a Test turn that is still working, so set that cap when busy turns are cut here.
The shipped default stays a silent bound and is not raised automatically.
It bounds only the Test step, and no other step or environment variable overrides it.

### test_agent_working_timeout

Optional still-working cap for one Test invocation.
Unset means the turn stops at [`test_agent_timeout`](#test_agent_timeout).
Set, it is that turn's absolute deadline, and it must be at least `test_agent_timeout`.
The 10-minute quiet stop still applies.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | unset                  |

It is global-only.

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

How often the daemon rechecks a parked approval gate while waiting for user approval. Today this applies to the CI step's parked gate, which re-probes provider availability (including `gh auth status`) and clears the gate when the PR was merged or closed, and to a [review gate parked on its reviewer's own questions](/no-mistakes/concepts/review-conversation/), which resumes the reviewer once none are open.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `2m`                   |

Accepts any positive Go `time.ParseDuration` string. Global-only: there is no matching field in a repository's `.no-mistakes.yaml`.

### gate_reconcile_timeout

Maximum wall time one parked approval-gate reconcile attempt may spend before the attempt stops, the gate stays parked, and the next interval wait begins. Covers host probes such as `gh auth status` that can hang without returning. The same bound applies to the live verification an approve response runs at a parked CI gate; a verification that times out counts as unverifiable, so it is refused while a [`ci.required_reviews`](/no-mistakes/reference/repo-config/#cirequired_reviews) requirement applies and otherwise recorded as an explicit override.

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

### worktree

Retention for leftover run-worktree directories under the default `<NM_HOME>/worktrees/<repo id>/<run id>` tree.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                | Type     | Default          | Description                                                                    |
| -------------------- | -------- | ---------------- | ------------------------------------------------------------------------------ |
| `worktree.retention` | `string` | `24h`            | How long a leftover run worktree survives; `unlimited`/`none`/`off`/`never` or a non-positive duration disables the bound |
| `worktree.max_runs`  | `int`    | `20`             | How many leftover worktree directories survive regardless of age; `0` disables the bound |

A run's own worktree is already removed the instant its pipeline finishes, so this budget is a safety net rather than the normal path: it only ever governs the directory left behind by a `git worktree remove` failure (for example a vendored `.git` nested somewhere under a large `node_modules` tree) or a [protected-path](/no-mistakes/reference/repo-config/#protected_paths) refusal that later became removable. Without it, a leftover like that survived indefinitely on a long-running daemon that never restarts, since the crash-recovery sweep that also reclaims it (see the daemon's [worktree cleanup](/no-mistakes/concepts/daemon/#what-it-does)) runs only at startup.

This reap runs after every finished run and again at daemon startup, the same cadence `test.evidence.retention` uses. Only the default `<NM_HOME>/worktrees` tree is bounded; a checkout you placed with [`worktree_roots`](#worktree_roots) is your own directory, and only the directories no-mistakes' own run records name there are ever touched, per that section's rules.

Global-only, for the same reason `test.evidence`'s local storage fields are: it governs this machine's local disk, so a repository does not get to set the retention budget for a directory every repository on the machine shares.

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
| `auto_fix.ci`       | `int` | `3`     | CI auto-fix attempts for CI failures, plus merge conflicts on GitHub, GitLab, Forgejo, Azure DevOps, Cursor Origin, and provider plugins declaring `mergeable_state` |

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

### ci.required_reviews

The operator-level fallback for [`ci.required_reviews`](/no-mistakes/reference/repo-config/#cirequired_reviews).
That reference owns supported reviewer profiles, completion evidence, and the trust boundary.
Defaults to `[]`; the trusted repository list overrides it, including an explicit empty list. `ci.required_review_app_id` may pin the GHES CodeRabbit App ID; `ci.required_review_bot_id` may pin the GitLab CodeRabbit account ID. These are identifiers, not credentials, and must match the configured forge. `copilot` needs neither: it is read only on GitHub.com, from the review GitHub's own Copilot code review submits.

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

### ci.review_bot_comments

The operator-level fallback for [`ci.review_bot_comments`](/no-mistakes/reference/repo-config/#cireview_bot_comments), whose per-repository reference owns the semantics and the trust boundary.

| | |
|---|---|
| Type | `string` (`on_failure` or `always`) |
| Default | `on_failure` |

```yaml
ci:
  review_bot_comments: always
```

A value in the trusted repository config overrides this global value. An unrecognized value fails the config rather than falling back to the default.

### ci.rebase_behind

The operator-level fallback for [`ci.rebase_behind`](/no-mistakes/reference/repo-config/#cirebase_behind), whose per-repository reference owns the semantics, the cost, and the trust boundary.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

```yaml
ci:
  rebase_behind: true
```

A value in the trusted repository config overrides this global value in both directions. When the trusted repository config omits the key, this global value applies.

### ci.carry_review_on_replay

The operator-level fallback for [`ci.carry_review_on_replay`](/no-mistakes/reference/repo-config/#cicarry_review_on_replay), whose per-repository reference owns the pure-replay proof, its limits, and the trust boundary.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

```yaml
ci:
  carry_review_on_replay: true
```

A value in the trusted repository config overrides this global value in both directions. When the trusted repository config omits the key, this global value applies.

### ci.release_after_checks_pass

The operator-level fallback for [`ci.release_after_checks_pass`](/no-mistakes/reference/repo-config/#cirelease_after_checks_pass), whose per-repository reference owns the semantics, the cost, and the trust boundary.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

```yaml
ci:
  release_after_checks_pass: true
```

A value in the trusted repository config overrides this global value in both directions. When the trusted repository config omits the key, this global value applies.

### rebase.strategy

The operator-level default for [`rebase.strategy`](/no-mistakes/reference/repo-config/#rebasestrategy), whose per-repository reference owns the semantics, the trade-off, and the trust boundary.

| | |
|---|---|
| Type | `string` (`rebase` or `merge`) |
| Default | `rebase` |

```yaml
rebase:
  strategy: merge
```

A value in the trusted repository config overrides this global value in both directions. When the trusted repository config omits the key, this global value applies. An unrecognized value fails the config closed rather than falling back to the default, so a typo cannot quietly keep rewriting history a maintainer asked to stop rewriting.

### review.path_instructions

Machine-local review guidance that applies to every repository this machine gates.
Use it for house rules you hold in every repository, including ones you do not control and so cannot commit a [`review.path_instructions`](/no-mistakes/reference/repo-config/#reviewpath_instructions) to.
For a rule that holds in one repository only, use a matching [`repository_overrides`](#repository_overrides) entry instead.

| | |
|---|---|
| Type | `object[]` with `path` (`string`) and `instructions` (`string`, multiline) |
| Default | Empty |

```yaml
review:
  path_instructions:
    - path: "**/*.vue"
      instructions: |
        Repeated instances of a component are driven from a computed, not stacked v-ifs.
```

Entries match, render, and validate exactly like the repository field, whose reference owns those rules.
They only add guidance: the repository's own trusted rules always apply alongside them, and nothing here can remove or replace one.
The reviewer receives each source as its own section, in this order: this global list, then the matching `repository_overrides` entry's list, then the repository's trusted list.
The two machine-local headings say the rules come from the operator's global config and not from the repository, so a machine-local rule never reads as the repository's own.
The step log names the source of every rule it applied or skipped.

The entry and byte limits apply to the combined set from all three sources, because they share one review prompt.
This global list together with each `repository_overrides` list is checked when the config loads.
The repository's trusted list changes independently, so the combined set is checked again when each run starts and when a run is recovered after a daemon restart: a run whose combined rules exceed a limit fails before any step runs or resumes, with an error naming each source's entry count, rather than silently dropping a rule.
Shorten or remove your machine-local entries to fix it.

Only `path_instructions` is accepted under this block. `review.conversation` stays a repository decision, because an open question parks the gate.
Changes apply to the next run, and to a run recovered after a daemon restart, which re-reads this file.

### commit.fix_message

Template for the subject of commits created by the Review, Test, Document, Lint, and CI repair paths, plus operator-authorized repository gate repairs.
When set to anything other than the default, it also renders the Push step's catch-all commit for remaining uncommitted changes, with `{{.Step}}` as `push` and `{{.Summary}}` as `apply agent fixes`; at the default, that commit keeps its `no-mistakes: apply agent fixes` subject.

| | |
| --- | --- |
| Type | `string` |
| Default | `no-mistakes({{.Step}}): {{.Summary}}` |

The template supports literal text and three Go-style placeholders:

| Variable | Value |
| --- | --- |
| `{{.Step}}` | Pipeline step name, such as `review`, `test`, `document`, `lint`, `ci`, `push`, or `gate.test.mutation-budget` |
| `{{.Summary}}` | Sanitized one-line summary returned by the fix agent, or the step's deterministic fallback summary |
| `{{.Branch}}` | Normalized branch name, or the identifier captured and optionally transformed by [`commit.branch_pattern`](#commitbranch_pattern) and [`commit.branch_replacement`](#commitbranch_replacement) |

The value must be a valid UTF-8 template that renders to a non-empty, single-line commit subject.
The template source is limited to 1,024 bytes and 16 placeholders.
The fix-agent summary is limited to 4,096 bytes, and the final rendered subject is held to 4,096 bytes. When the summary would push the subject past that limit, no-mistakes truncates the summary without splitting a UTF-8 character. The summary keeps the largest whole-character prefix that fits alongside an ellipsis (`…`), removing any trailing space before the ellipsis. Repeated `{{.Summary}}` placeholders share the remaining byte budget equally and render the same truncated summary. If exactly three bytes remain per summary placeholder, only the ellipsis is emitted; if fewer than three bytes remain, the summary is dropped entirely without a marker. Literal template text, `{{.Step}}`, and `{{.Branch}}` are never truncated, so a template that overflows without any summary still fails.
Before rendering, no-mistakes predicts the subject size from the validated literal text and placeholders, then rejects a subject whose non-summary text alone overflows, without allocating the expanded message.
Template functions, control actions, named templates, unknown placeholders, malformed syntax, control characters, unsafe Unicode format characters, and Unicode line or paragraph separators cause configuration loading to fail.
The blocked format set includes every Unicode `Bidi_Control` code point plus `U+00AD`, `U+180E`, `U+200B`, `U+2060` through `U+2064`, the deprecated bidi controls `U+206A` through `U+206F`, `U+FEFF`, `U+FFF9` through `U+FFFB`, and Unicode tag characters in `U+E0000` through `U+E007F`.
Legitimate `U+200C` zero-width non-joiner and `U+200D` zero-width joiner text shaping remains allowed.
The final rendered subject is validated again, so unsafe characters in an agent-provided summary are also rejected.
Each fix prompt, including CI repair, tells the agent how many characters the summary may use so the subject stays within 72: 72 minus the rendered text around the summary, divided by the number of times the template uses `{{.Summary}}`, with a floor of 20. The 72-character target is guidance only; summaries exceeding that target remain unchanged when the rendered subject fits the 4,096-byte limit.
CI repair prompts direct the agent to keep detailed diagnosis and verification evidence in its explanatory output, recorded in the run log, rather than in the summary.
The setting does not change commit subjects created by the Rebase step or the evidence-branch commit; see [`test.evidence`](#testevidence) for the latter.
A per-repo [`commit.fix_message`](/no-mistakes/reference/repo-config/#commitfix_message) value overrides this global setting.

### commit.branch_pattern

Optional regular expression for extracting the value exposed as `{{.Branch}}` to commit and PR title templates.

| | |
| --- | --- |
| Type | `string` regular expression |
| Default | Unset, so `{{.Branch}}` is the normalized full branch name |

The expression is limited to 1,024 bytes, must be valid UTF-8, must exclude the same control and unsafe Unicode format characters as `commit.fix_message`, and must compile with exactly one capture group.
Without [`commit.branch_replacement`](#commitbranch_replacement), that capture group becomes `{{.Branch}}`, so `([A-Z]+-[0-9]+)` extracts `PROJ-123` from `feature/PROJ-123-add-widget`.
For example, this global configuration renders `PROJ-123: preserve legacy drafts` from branch `PROJ/123`:

```yaml
commit:
  branch_pattern: '^PROJ/([0-9]+)$'
  branch_replacement: 'PROJ-${1}'
  fix_message: "{{.Branch}}: {{.Summary}}"
```

When a template uses `{{.Branch}}` and the pattern does not find a non-empty identifier, rendering fails safely instead of producing an empty prefix.
A per-repo [`commit.branch_pattern`](/no-mistakes/reference/repo-config/#commitbranch_pattern) value overrides this global setting.

### commit.branch_replacement

Optional expression that adds literal text around the branch pattern's capture group before exposing it as `{{.Branch}}`. Set it under global `commit` for the machine-wide default, or under a matching [`repository_overrides`](#repository_overrides) entry for one remote. It is not available in a repository's `.no-mistakes.yaml`.

| | |
| --- | --- |
| Type | `string` replacement expression |
| Default | Unset, so the capture group is used unchanged |

Use exactly one `${1}` reference to insert the capture group; other dollar syntax is rejected.
Under global `commit`, the replacement must be configured with `commit.branch_pattern` in that block. Under `repository_overrides`, pair it with `commit.branch_pattern` in the same remote entry.
It is limited to 1,024 bytes, must be valid UTF-8, and must exclude the same control and unsafe Unicode format characters as `commit.fix_message`.
Malformed replacement syntax fails configuration loading with an actionable error.
The expanded identifier is subject to the existing UTF-8, control-character, unsafe-Unicode, and rendered-subject validation.
A `commit.branch_pattern` in `.no-mistakes.yaml` takes precedence and clears any inherited machine-wide replacement, including one from a matching repository override, so a replacement cannot be applied to a different pattern.

### commit.trailers

Git trailers appended to each commit made from a single agent invocation's changes, naming the agent and model that produced them.

| | |
| --- | --- |
| Type | `list` of `string` templates |
| Default | Unset, so commits carry no trailers |

Each entry renders to one trailer line. It supports literal text and two Go-style placeholders:

| Variable | Value |
| --- | --- |
| `{{.Agent}}` | The agent that actually ran the invocation (`omp` in this build) |
| `{{.Model}}` | The model the agent reported serving the invocation, or `unknown` when it reports none |

For example, this renders `Assisted-by: no-mistakes:omp:qwen4-max` on a fix made by omp:

```yaml
commit:
  trailers:
    - "Co-Authored-By: no-mistakes {{.Agent}} <noreply@example.com>"
    - "Assisted-by: no-mistakes:{{.Agent}}:{{.Model}}"
```

`{{.Agent}}` and `{{.Model}}` come from agent output, so each is cut to its first whitespace-separated token, reduced to letters, digits, and `-_.:/`, and limited to 64 bytes; a value with nothing left renders as `unknown`.
omp reports the model it served; if it reports none, the trailer renders `unknown`.
Every entry must render to a `Key: value` line that git recognizes as a trailer.
The key must be literal: an entry starts with its full `Key: ` prefix as plain text, and placeholders are allowed only in the value after it, so `{{.Agent}}-assisted: yes` is rejected.
Entries are limited to 1,024 bytes and 16 placeholders, the list to 16 entries, and the same template restrictions and unsafe-character rules as `commit.fix_message` apply.
The 1,024-byte limit also applies to the rendered line, checked when configuration loads with every placeholder at its 64-byte maximum, so an entry that loads cannot overflow at commit time.
An invalid entry fails configuration loading, and a render failure at commit time leaves the changes unstaged.

Trailers are added to Review, Test, Document, and Lint fix commits, operator-authorized repository gate repairs, and CI repair commits. Commits that no single invocation produced get none rather than a guessed agent: the Push step's catch-all commit, a CI repair retried after a protected-path refusal, and rebase or merge commits that an agent writes itself.
A per-repo [`commit.trailers`](/no-mistakes/reference/repo-config/#committrailers) list or a matching [`repository_overrides`](#repository_overrides) entry replaces this list rather than extending it; an empty list clears it.

### pr.appendix

Machine-wide default for the generated PR evidence appendix:

```yaml
pr:
  appendix: collapsed # full | collapsed | minimal
```

Precedence: a non-empty trusted repository `pr.appendix`, then a matching
[`repository_overrides`](#machine-local-pr-publication-defaults) value, then this
global value, then built-in `full`. An unset or empty value inherits the next
layer. The pushed branch cannot choose the appendix mode, even with
`allow_repo_commands: true`.

All modes retain the pipeline attestation. See the repository
[`pr.appendix`](/no-mistakes/reference/repo-config/#prappendix) reference for
rendering behavior and provider caveats. Invalid modes fail config loading.
`pr.template`, `pr.publish_intent`, and `pr.intent` are not machine-wide settings.

### repository_overrides

Machine-local settings scoped to one repository by remote host and full repository path.
This lets one machine add checks, lower command scheduling priority, add review, documentation, and live-validation guidance, choose review-loop agents, or apply ticket conventions without adding settings to that repository.
Remote hosts are matched case-insensitively.
HTTP, HTTPS, SSH, and Git-protocol URLs, plus scp-style remotes, are accepted; the transport scheme is not part of the match.
A URL's scheme-default port (80, 443, 22, or 9418 for HTTP, HTTPS, SSH, or Git) matches an omitted port; non-default ports remain distinct.
IPv6 addresses are canonicalized, with bracket boundaries preserved so a port cannot be confused with address text.
For `github.com`, `gitlab.com`, and `bitbucket.org`, repository paths are also matched case-insensitively and without a trailing `.git`, across equivalent HTTPS, SSH URL, and scp-style remotes.
On every other host, repository path case and a trailing `.git` are significant.
GitLab subgroup paths are preserved.
On other SSH hosts, rooted paths (`host:/...` or `ssh://host/...`) remain distinct from home-relative scp paths (`host:...`).

```yaml
repository_overrides:
  https://github.com/acme/widget.git:
    commit:
      branch_pattern: '([A-Z]+-[0-9]+)'
      fix_message: '{{.Branch}}: {{.Summary}}'
    pr:
      title_format: '{{.Branch}}: {{.Title}}'
```

Formatting fields are `commit.branch_pattern`, `commit.branch_replacement`, `commit.fix_message`, `commit.trailers`, and `pr.title_format`; each retains the same fail-closed validation as its global or repository-config equivalent.
A `commit.branch_replacement` must be paired with `commit.branch_pattern` in the same override.
Precedence for these formatting fields is explicit: `.no-mistakes.yaml` wins for every field it sets, then a matching machine-local override, then the plain global value, then the built-in default.
As with the global replacement, a repository `commit.branch_pattern` replaces the matching machine-local pattern and clears its replacement.
Repositories matching no block keep existing global and built-in behavior.

#### Machine-local PR publication defaults

A matching entry can select a repository's existing PR template without committing `.no-mistakes.yaml`:

```yaml
repository_overrides:
  https://github.com/acme/widget.git:
    pr:
      template: .github/pull_request_template.md
      appendix: collapsed
      update_text: keep # keep | regenerate (default)
```

`pr.template` selects a repository-relative file, never local template contents. It uses the same [path and file validation](/no-mistakes/reference/repo-config/#prtemplate) as the repository setting, and its bytes are read only from the pinned trusted default-branch tree, never from the pushed branch or worktree.
`pr.appendix` accepts `full`, `collapsed`, or `minimal` and can be set with or without `pr.template`; it uses the same [appendix and attestation behavior](/no-mistakes/reference/repo-config/#prappendix) as the repository setting. When template mode is active, author-edited narrative is preserved on later updates.
`pr.update_text` accepts `keep` or `regenerate`. The default is `regenerate`, also used when the key is absent. `regenerate` replaces the title and body on update, as today, but only while the live body carries no owned appendix markers. Once `keep` has created or updated a PR, that PR keeps its title and narrative even after the key changes back to `regenerate` or is removed, the same rule that stops removing `pr.template` from switching an owned body back. `keep` leaves the live title unchanged and keeps the narrative above the appendix byte for byte, the same preservation [`pr.template`](/no-mistakes/reference/repo-config/#prtemplate) already uses, including when that repository has no template file. The step still creates the pull request when none exists: the ordinary drafter supplies the title and narrative, and the evidence is stored as that owned appendix. Every later update refreshes the appendix, or the `minimal` risk line, and the attestation, so they match the current head. If that narrative and the evidence cannot fit the publication budget, the step fails instead of truncating them. A configured `pr.title_format` still redrafts the bare title. A description freshly supplied with [`axi run --pr-body-file`](/no-mistakes/reference/cli/#supplied-pr-title-and-description) is published as that same owned narrative whatever this key says, including when updating an existing PR. Inherited text on a plain rerun does not override this setting. Without a freshly supplied replacement description, an existing body that already carries an unowned attestation is refused until that generated evidence is separated from the narrative, the same adoption rule as template mode. To prepare an already open PR that an earlier `regenerate` run published, move its generated evidence out of the body by hand once before enabling `keep`. Any other value fails config loading. A repository `.no-mistakes.yaml` cannot set this key, and it is not a machine-wide `pr` setting.
For `pr.template`, a non-empty trusted repository value wins over the matching machine-local value. For appendix inheritance, see [`pr.appendix` precedence](#prappendix).
`pr.template` is available only under `repository_overrides`, not as a machine-wide PR setting. `pr.publish_intent` and `pr.intent` remain trusted-repository-only.

#### Machine-local review, documentation, and test guidance

A matching entry can add review rules, documentation policy, and a live-validation runbook for that one repository, without committing anything to it:

```yaml
repository_overrides:
  https://gitlab.example.com/group/app-one.git:
    review:
      path_instructions:
        - path: "**/*.cs"
          instructions: |
            Sync wording is always "sync from <upstream>": it is a one-way overwrite, never a merge.
    document:
      instructions: |
        Configuration keys are owned by docs/reference/config.md.
    test:
      instructions: |
        Never install, launch, or test on a physical device; drive the emulator only.
```

`review.path_instructions` behaves like the [global list](#reviewpath_instructions): its rules render in their own labeled section after the global rules and before the repository's trusted rules, and they count toward the same combined limits.
`document.instructions` is added to the document step's prompt in a labeled section before the repository's trusted [`document.instructions`](/no-mistakes/reference/repo-config/#documentinstructions), and like that field it augments the built-in placement policy and cannot weaken it.
`test.instructions` is added to the Test step's repair and evidence prompts in a labeled section before the repository's trusted [`test.instructions`](/no-mistakes/reference/repo-config/#testinstructions) runbook.
All three are additive only: the repository's trusted values still apply in full, and a pushed branch can supply none of them.
Under `review` only `path_instructions` is accepted, under `document` only `instructions`, and under `test` only `instructions`; settings that could weaken a gate, such as `no_ci`, `allow_repo_commands`, or `pr.base_branch`, are not accepted here.
Changes apply to the next run, and to a run recovered after a daemon restart, which re-reads this file.

Eval replay cases store no remote URL, so a replay does not apply a matching entry's review, documentation, or test guidance.

#### Machine-local commands

A matching `commands` block supplements the repository's trusted commands; it never changes their strings or removes them.
Repository command selection still comes from the trusted default branch, or the pushed branch only with trusted `allow_repo_commands: true`.
Only the operator's global config can supply these local settings, never a repository's `.no-mistakes.yaml`.
With no matching command override, execution remains unchanged.

```yaml
repository_overrides:
  https://github.com/acme/widget.git:
    commands:
      test:
        additional:
          - /opt/local-checks/widget-smoke
        nice: 10
      lint:
        additional:
          - /opt/local-checks/widget-policy
      prepare:
        nice: 10
```

| Field | Supported commands | Meaning |
| --- | --- | --- |
| `additional` | `test`, `lint` | Ordered list of separate shell checks added after the repository check; every check must succeed |
| `nice` | `prepare`, `test`, `lint`, `format` | POSIX niceness adjustment from `0` to `19`; `0` leaves scheduling unchanged |

Test runs the committed command first, then each added command in a separate shell, retaining a failure from either source even when another check succeeds.
Test still performs its unconditional agent-driven end-user scenarios afterwards.
Lint runs additional checks after the existing lint duty, including agent-driven lint when `commands.lint` is empty.
An added check's failure parks the step rather than silently passing; existing explicit approval rules still apply.
Its finding names that machine-local check and its exit code, and never attributes the failure to the committed command, including when the committed command is empty.
`additional` is refused for preparation and formatting, because those commands are not independent check gates.
Replacement command strings, `command`, `replace`, `skip`, per-command `env`, unknown command names, empty additional checks, and niceness outside `0` through `19` are configuration errors.

Overrides are always declared, never silent: whenever a command runs under any of these settings, its step log states `machine-local overrides applied to commands.<name>:` followed by the niceness and the added checks, once per step rather than per check, and Test passes the same declaration to its agent for the testing summary.
These settings are scoped to configured shell commands and their local checks, not agents, built-in Git operations, forge commands, or repository-declared extra gates.

There is no per-command environment override.
Toolchain paths (such as `PATH`) and parallelism settings (such as `GOMAXPROCS`) come from the operator's own environment, which the daemon captures at startup and passes to every configured command; [Environment the daemon sees](/no-mistakes/reference/environment/#environment-the-daemon-sees) owns where to set them.

`nice: 10` invokes the POSIX `nice -n 10` utility around the command shell, adding ten to its inherited niceness, not setting an absolute priority.
Positive niceness is refused on Windows; set resource limits in the operator's own environment there.
A missing `nice` utility fails the command rather than silently ignoring the request.

Before executing an opted-in run, no-mistakes records the full resolved configuration in `<NM_HOME>/logs/<run-id>/command-config.ndjson`, including the unchanged team command strings, added checks, niceness, trusted-config SHA, and tool build.
It also records the values configured commands inherit from the daemon's environment for `PATH`, `GOMAXPROCS`, `MAKEFLAGS`, `CARGO_BUILD_JOBS`, and `CMAKE_BUILD_PARALLEL_LEVEL`, omitting any that are unset; these values appear only in this file, never in step logs, agent prompts, findings, or the PR.
This record is independent of optional eval capture.
Recovery appends a new snapshot of the configuration it resolves, including removal of a previously active local override.
A snapshot write failure stops execution before checks run.
The file is private local evidence, restricted to owner-only permissions on POSIX on every write (including a file left from an earlier snapshot) and excluded from PR and test-evidence publication.

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, and pass that summary to rebase, review, test, document, lint, CI auto-fix, repository gate repair, and PR prompts. For publication of the generated Intent section, see [`pr.publish_intent`](/no-mistakes/reference/repo-config/#prpublish_intent).

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type       | Default | Description                                                |
| ------------------------- | ---------- | ------- | ---------------------------------------------------------- |
| `intent.enabled`          | `bool`     | `true`  | Enable transcript-based intent extraction                  |
| `intent.threshold`        | `float`    | `0.2`   | Minimum raw match score for selecting a transcript session |
| `intent.slack_days`       | `int`      | `3`     | Extra days to look back before the change window           |
| `intent.disabled_readers` | `string[]` | Empty   | Transcript readers to disable                              |
| `intent.publish_intent`   | `bool`     | `true`  | Publish the generated Intent section on PR bodies by default |
| `intent.check`            | `object`   | `off`    | Add deterministic term and pattern checks for run intent       |

Valid `disabled_readers` values are `omp`.

`intent.publish_intent: false` is a global, operator-side default that keeps the generated `## Intent` section out of the PR body for runs started without an explicit override. It is the caller-side counterpart of the repository's trusted [`pr.publish_intent`](/no-mistakes/reference/repo-config/#prpublish_intent): both are tighten-only, the repository's trusted policy remains the ceiling a caller can never exceed, and review, test, document, lint, and CI auto-fix prompts keep the full intent. Under the caller-side omission the PR-drafting turns receive no intent text at all and draft from the diff and commit messages only; the intent is withheld from them, never scanned out of their output. A run records the folded decision (the `axi run --no-publish-intent` flag OR this global default) at start; reruns inherit it, and a mid-run config change never re-publishes. This field is global-only: a pushed branch's `.no-mistakes.yaml` cannot express it.

#### intent.check

Configure deterministic rules that warn about or reject run intent before pipeline agents act on it. The same `intent.check` block is available in global config and the repository's trusted default-branch config. Global and trusted repository rules combine additively; the stronger mode wins. A pushed branch cannot add, remove, or weaken rules, even when `allow_repo_commands: true`.

```yaml
intent:
  check:
    mode: reject # off (default) | warn | reject
    terms:
      - "internal project label"
    patterns:
      - '(?i)run-[a-z0-9-]+'
```

`terms` match case-insensitive literal substrings. `patterns` use Go RE2 regular expressions and are case-sensitive unless the expression enables `(?i)`. Explicit, inherited, proof, push, rerun, retry, and other supplied intent is checked before it can supersede an active run or start a pipeline agent. Transcript-inferred intent is checked after extraction and before no-mistakes stores or forwards the summary. `--skip intent` does not bypass the policy.

`off` performs no checks. `warn` writes a private local warning and continues. `reject` stops the run. Warning, error, and log messages list matching rule indexes only; they never echo matched text. Warnings are not findings and are never published.

Each config layer accepts at most 32 terms and patterns combined, at most 256 bytes per rule, and at most 8 KiB of rule text. Invalid modes, empty or oversized rules, over-limit policies, and invalid RE2 expressions fail config loading. The default is `off`; leaving `intent.check` unset preserves existing behavior.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated by the configured pipeline agent.

### evidence_dir

Parent directory for new run evidence. Global-only. A repository cannot set it.

| | |
| ---- | -------- |
| Type | `string` |
| Default | `<NM_HOME>/evidence` |

Set `evidence_dir` to an absolute path, `~`, or a path starting with `~/` (or `~\` on Windows). New runs write `<evidence_dir>/<run-id>`. When the key is absent or empty, `test.evidence.local_root` still selects the parent, with `<NM_HOME>/evidence` as its default. A nonempty `evidence_dir` takes precedence over `local_root`.

If the directory is missing, not writable, not absolute after `~` expansion, or overlaps a default, configured, or recorded worktree placement, no-mistakes logs a warning and uses `<NM_HOME>/evidence`. It does not use `test.evidence.local_root` as that fallback. It does not create a missing directory, and it does not move a directory that already exists.

If the default evidence directory also overlaps a worktree placement, run creation fails rather than writing evidence into a checkout.

A run records its evidence directory at creation. A later change to `evidence_dir` does not retarget that run. A run recorded before this setting existed keeps resolving through `test.evidence.local_root`.

For GitLab image uploads from that run directory, see [Pull request](/no-mistakes/reference/pipeline-steps/#pull-request).

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts are written to `<NM_HOME>/evidence/<run-id>`. [`evidence_dir`](#evidence_dir) moves that parent for new runs. On GitHub.com/GHEC, supported image and video artifacts are also uploaded when the PR is rendered; see `attach_media` below.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                         | Type     | Default                  | Description                                                                |
| ----------------------------- | -------- | ------------------------ | -------------------------------------------------------------------------- |
| `test.evidence.store_in_repo` | `bool`   | `false`                  | Publish test evidence artifacts to the repository's orphan evidence branch |
| `test.evidence.attach_media`  | `bool`   | GitHub: `true`; GitLab: `false` | Upload evidence when the PR is rendered: images/videos to GitHub user-attachments, images to GitLab project uploads |
| `test.evidence.compact`       | `bool`   | `true`                   | Keep the PR Testing section compact; set `false` for inline previews and expanded details |
| `test.evidence.dir`           | `string` | `.no-mistakes/evidence`  | Directory prefix inside the evidence branch                                |
| `test.evidence.branch`        | `string` | `no-mistakes/evidence`   | Name of the orphan evidence branch                                         |
| `test.evidence.commit_message` | `string` | `no-mistakes: evidence for {{.Branch}} (run {{.RunID}})` | Subject template for evidence-branch commits |
| `test.evidence.local_root`    | `string` | `<NM_HOME>/evidence`     | Absolute parent for run evidence when `evidence_dir` is unset             |
| `test.evidence.link_base_url` | `string` | unset                    | Opt-in `http(s)` base for links to locally served evidence; see [Evidence links](#evidence-links) (global-only) |
| `test.evidence.retention`     | `string` | `336h` (14 days)         | How long a run's evidence survives; `unlimited`/`none`/`off`/`never` or a non-positive duration disables the bound |
| `test.evidence.max_runs`      | `int`    | `200`                    | How many run directories survive regardless of age; `0` disables the bound |

`compact: true` preserves the compact PR Testing section. Set `compact: false` to opt into inline screenshot/GIF previews, video embeds, visible log/text blocks, individual test-command bullets, and a summary bullet. Both modes retain inspectable artifact links alongside previews when published links are available. This presentation setting does not enable uploads or evidence publication; for commit-pinned inline images, also enable `store_in_repo`.

Non-compact sections are taller and can approach GitHub's PR body size limit sooner, especially with many screenshots. Repository weight is unchanged: with `store_in_repo` enabled, the same artifacts are committed to the evidence branch in either mode. Pipeline steps and their order are unchanged.

The test step always collects evidence outside the worktree, so artifacts never enter the branch under validation.
On GitHub.com and GitHub Enterprise Cloud, image and video artifacts that pass GitHub's attach rules (png, jpg, jpeg, gif, webp, svg, mp4, mov, webm; images at most 10 MiB and videos at most 100 MiB) are uploaded to GitHub user-attachments when the PR body is rendered, unless `attach_media` is false. Setting `attach_media: false` disables uploads even when `store_in_repo: true`; evidence-branch publication and its commit-pinned links remain enabled.
The same setting gates GitLab evidence uploads, which require explicit `attach_media: true` in global or repository config. For the GitLab Evidence section, including its behavior without uploads, see [Pull request](/no-mistakes/reference/pipeline-steps/#pull-request).
GitHub uploads need the raw GitHub token, which the PR step reads once per PR render with `gh auth token`; when that read fails, the step logs one line and skips every remaining upload in that render instead of asking again per file. If your `gh` sits behind a credential vault or disclosure gate that prompts before it releases the token, set `attach_media: false` so the PR step never asks for it.
For GitHub attachments, the testing section embeds the returned image markdown or bare video URL so remote reviewers can open the media. Upload is fail-closed: any error, unsupported type, oversize file, GitHub Enterprise Server, non-GitHub forge, or GitHub App/Actions token falls back to the configured evidence presentation rather than a dead attachment URL. Published evidence retains commit-pinned links, with previews when `compact: false`; unpublished local artifacts follow [Evidence links](#evidence-links). Text artifacts remain inline or collapsible according to `compact`.
When `store_in_repo` is true for a GitHub repository, the PR step copies that directory onto `branch` under `<dir>/<branch-slug>` in the code branch's push-target repository (the fork when fork routing is configured), pushes it, and links the artifacts from the pull request body.
Published text files get the same home-directory redaction as the PR title and body, so a path under the operator's home is committed with that part rewritten to `~`. Before that, the run's worktree root, and any path into another run worktree under `<NM_HOME>/worktrees/<repo-id>/<run-id>`, is rewritten to `{run-worktree}` (a bare `<NM_HOME>/worktrees` or `<NM_HOME>/worktrees/<repo-id>` to `{worktrees}`), because the repo id is a hash of the local checkout path and `NM_HOME` may sit outside the home directory. Binary and visual artifacts are published as written, and the local evidence files are never changed.
When both `attach_media` and `store_in_repo` apply, the testing section carries the user-attachments embed in addition to the commit-pinned git link.
The branch is an orphan: it shares no history with your code branches, so evidence never reaches the default branch. Links use the evidence commit rather than the branch, so they keep resolving after later runs.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
`branch` must be a valid Git branch name; an invalid value fails the config with the offending key and value.
`commit_message` supports literal text and two placeholders: `{{.Branch}}` (the code branch name) and `{{.RunID}}` (the run ID). It follows the same template restrictions, size limits, and unsafe-character rules as [`commit.fix_message`](#commitfix_message); an invalid template fails configuration loading, and a subject that fails validation at publish time leaves the evidence unpublished.
The publisher never force-pushes. It appends to the fetched evidence-branch tip with a fast-forward push, retries one lost race, and refuses to use the run branch, default branch, or an existing branch whose tip lacks the `.no-mistakes-evidence` marker.
Publication is also refused when the remote cannot be read or pushed, an artifact exceeds 64 MiB, a run exceeds 500 files or 256 MiB, or another writer wins the retry. The PR body then keeps its local rendering instead of adding links that would not resolve.
Evidence-branch publication currently supports GitHub links only. On other providers, no evidence branch is pushed. GitLab image uploads are independent of these publication settings; see [Pull request](/no-mistakes/reference/pipeline-steps/#pull-request).
Enabling this pushes a branch to your remote, so pick a `branch` name your CI workflows do not build.

#### Local storage and cleanup

The default evidence location is under the app root rather than the system temp directory; [`evidence_dir`](#evidence_dir) and `local_root` can select a different parent. On Linux the daemon runs from a service unit that does not export `TMPDIR`, so the old temp-directory default resolved to the shared `/tmp`, which current Ubuntu mounts as a RAM-backed `tmpfs`. The app root is disk backed on macOS, Linux, and Windows alike.

no-mistakes reaps its recorded run directories itself rather than relying on an operating-system temp cleaner. Unrecognized directories under an evidence root are left untouched.

- A finished run that produced no artifacts leaves nothing behind.
- Recorded run directories older than `retention` are removed.
- Whatever recorded run evidence survives across the current, historical, and legacy roots is trimmed to one shared `max_runs` budget, oldest first.
- A run that is still pending or running is never touched.

Reaping runs after each finished run and again at daemon startup. An upgraded daemon also drains the pre-relocation directory in the system temp directory under the same rules; nothing is migrated, because absolute paths recorded in older pull request bodies name the old location.

`local_root` must be an absolute path outside `<NM_HOME>/worktrees`; a relative or managed-worktree path fails daemon startup and prevents new or recovered runs from starting. Because `retention` bounds how long a PR body's local artifact links keep resolving, raise it rather than lowering it if your reviews run long.

#### Evidence links

`link_base_url` is opt-in and unset by default, and unset changes nothing: PR bodies render exactly as they do today. Set it to an absolute `http` or `https` URL when you serve the evidence parent (`evidence_dir`, or `local_root` when that is unset) over HTTP yourself, for example a synced folder behind a web server with its own access control. Local evidence references then render as markdown links to `<link_base_url>/<run-id>/<file>`, mirroring the served layout `<evidence parent>/<run-id>/<file>`, so a reviewer opens a screenshot in a new tab instead of copying a local path out of the description:

- In the Testing section, each local evidence file reference (an image, binary, or over-budget artifact that was neither published nor uploaded) becomes `- Evidence: [label](<link_base_url>/<run-id>/<file>)`, on every provider and in both `compact` presentations.
- In the scenario table, every complete reference to a file that exists under the run's evidence directory becomes a link: a name relative to that directory (nested paths and names with spaces included) or an absolute path inside it; unrelated words remain plain text, and a reference the cell already carries as a Markdown link, image, autolink, or bare URL is left exactly as written. A code span holding known evidence files renders each as a link without the backticks, preserving separators and other words; a code span without a known evidence file is left as written. Embedded transcripts retain their inline contents and gain a `Source` link in compact details or an `Evidence` link in expanded presentation.
- For GitLab's separate `## Evidence` section, see [Pull request](/no-mistakes/reference/pipeline-steps/#pull-request).
- In Testing artifact entries, `link_base_url` applies to local-file mentions under the evidence root where no remote link exists; it never overrides an explicit artifact URL, a successful upload, or a published evidence-branch link. Scenario-table references use the configured base independently of those artifact entries.

`link_base_url` links evidence files in the evidence list lines, the scenario table’s Evidence column, transcript sources, and the GitLab Evidence section; free-form prose such as the testing summary, tested details, and artifact descriptions is left as written.

Surrounding whitespace and trailing slashes are trimmed; an empty value disables the setting. A value that is not an absolute `http(s)` URL, contains whitespace after trimming, carries credentials, or has a query or fragment fails configuration loading, in the global file and in a pushed repository copy alike. Hosting, access control, and how long the served copy survives are yours; the pipeline never checks that a link resolves, and `retention` still bounds the local copy. The setting is global-only like `local_root`: a repository cannot choose where a PR's evidence links point, so a valid `test.evidence.link_base_url` in `.no-mistakes.yaml` is ignored.

The publication fields are global defaults. Repo config can override `store_in_repo`, `attach_media`, `compact`, `dir`, and `commit_message`; it can override `branch` only through the trusted default-branch copy. `local_root`, `retention`, `max_runs`, and `link_base_url` are global-only: a repository does not get to name a filesystem path this machine's daemon writes to, set the retention budget for a directory every repository on the machine shares, or choose where a PR's evidence links point.

`test.evidence.retention` and `test.evidence.max_runs` also bound `<NM_HOME>/logs/<run-id>` (per-run step logs), reaped on the same cadence rather than through a second config surface for the same kind of per-run diagnostic artifact.

### eval

Local review-evaluation corpus settings for [`no-mistakes eval`](/no-mistakes/reference/eval/).

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type   | Default | Description                                                            |
| ------------------------- | ------ | ------- | ---------------------------------------------------------------------- |
| `eval.capture_provenance` | `bool` | `true`  | Record the exact commit and configuration inputs a replay needs        |
| `eval.auto_capture`       | `bool` | `true`  | Collect eligible review cases and fixed CI false negatives automatically |
| `eval.max_cases`          | `int`  | `200`   | Retention target for automatic collection; `0` keeps every case        |
| `eval.diversified_size`   | `int`  | `32`    | Cap on the official gold-only `diversified` set; `0` is one gold case per stratum |

`capture_provenance` is what makes a review pass replayable at all. It is recorded when the round is written and cannot be added afterwards, because the pinned configuration is a point-in-time snapshot, so a run reviewed with it off can never be captured later.

`auto_capture` collects without any command: when an eligible run finishes, its decided review rounds become cases and fixed `ci-check` and `ci-review-bot` findings become Review false negatives. It does nothing while `capture_provenance` is off. Collection runs after the pipeline has already reported its outcome and can never change it; a failure is logged and nothing else. The [Evaluation toolkit](/no-mistakes/reference/eval/#how-cases-are-collected) owns eligibility and labeling details.

`max_cases` sets the retention target enforced after automatic collection. When it is exceeded the oldest unprotected cases are dropped first. A case with a replay in progress or recorded candidate replays is protected, so the corpus can remain above the target rather than invalidate a comparison you have spent tokens on. Cases from the same repository share one local object pool, so a case costs its own records plus the objects its commits introduced rather than a copy of the repository.

`diversified_size` caps the official gold-only eval set used by `eval run --cases diversified`. Selection is stratified and pinned; unlabeled cases never fill it. `0` keeps one gold case per stratum with no Hamilton bound. Corpus retention (`max_cases`) and this official-set cap are different knobs.

These are operator settings for this machine's local disk, so they are global-only: an `eval` block in a repository's `.no-mistakes.yaml` is ignored. Corpus storage stays under `<NM_HOME>/eval` and no-mistakes never uploads it; replay still sends code to the selected agent's configured model provider as described in the [Evaluation toolkit](/no-mistakes/reference/eval/).

### providers.github.draft_pull_requests

Open pull requests created on GitHub as drafts (`gh pr create --draft`).

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

Only affects PR creation; existing PRs are not toggled between draft and ready. GitHub only — ignored for other providers.
This is a global default. Per-repo config can override it via `providers.github.draft_pull_requests`.

### providers.gitlab.draft_pull_requests

Open merge requests created on GitLab as drafts (`glab mr create --draft`).

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

Only affects MR creation; existing MRs are not toggled between draft and ready. GitLab only — ignored for other providers.
This is a global default. Per-repo config can override it via `providers.gitlab.draft_pull_requests`.

### providers.bitbucket.draft_pull_requests

Open pull requests created on Bitbucket Cloud as drafts (`"draft": true` in the create-PR API request).

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

Only affects PR creation; existing PRs are not toggled between draft and ready. Bitbucket only — ignored for other providers.
This is a global default. Per-repo config can override it via `providers.bitbucket.draft_pull_requests`.

### providers.azuredevops.draft_pull_requests

Open pull requests created on Azure DevOps as drafts (`az repos pr create --draft true`).

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

Only affects PR creation; existing PRs are not toggled between draft and ready. Azure DevOps only — ignored for other providers.
This is a global default. Per-repo config can override it via `providers.azuredevops.draft_pull_requests`.

### providers.origin.draft_pull_requests

Open pull requests created on Cursor Origin as drafts (`origin pr create --status draft`).
When false (the default), no-mistakes passes `--status open` because the Origin CLI otherwise defaults to draft.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

Only affects PR creation; existing PRs are not toggled between draft and ready. Cursor Origin only — ignored for other providers.
This is a global default. Per-repo config can override it via `providers.origin.draft_pull_requests`.

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, `NM_DAEMON_CONNECT_TIMEOUT`, Forgejo host and token settings, Bitbucket Cloud credentials, and update-check suppression.
