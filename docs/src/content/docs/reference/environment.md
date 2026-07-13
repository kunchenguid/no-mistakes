---
title: Environment Variables
description: All environment variables recognized by no-mistakes.
---

## `NM_HOME`

Override the data directory.

|         |                  |
| ------- | ---------------- |
| Type    | `string`         |
| Default | `~/.no-mistakes` |

When set, everything else moves under this root:

- Global config: `$NM_HOME/config.yaml`
- Gate repos: `$NM_HOME/repos/<id>.git`
- Worktrees: `$NM_HOME/worktrees/<repoID>/<runID>/`
- Logs: `$NM_HOME/logs/`
- Database: `$NM_HOME/state.sqlite`
- Socket / PID / singleton lock: `$NM_HOME/socket`, `$NM_HOME/daemon.pid`, and `$NM_HOME/daemon.lock`
- Managed agent server PID records: `$NM_HOME/servers/`
- Managed service names get a short stable suffix derived from `$NM_HOME` so multiple installs don't collide.

## `NM_DAEMON_CONNECT_TIMEOUT`

Override how long a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging.

|         |                                                                                                   |
| ------- | ------------------------------------------------------------------------------------------------- |
| Type    | `string` (Go duration)                                                                            |
| Default | unset (falls back to the `daemon_connect_timeout` global config value, itself defaulting to `3s`) |

Takes precedence over `daemon_connect_timeout` in `config.yaml`. An empty, unparsable, or non-positive value is ignored and the config value (or its default) is used instead.

## `NO_MISTAKES_BITBUCKET_EMAIL`

Bitbucket Cloud account email used for PR creation and CI monitoring.

|         |                                               |
| ------- | --------------------------------------------- |
| Type    | `string`                                      |
| Default | (none; Bitbucket PR/CI steps skip when unset) |

Used alongside `NO_MISTAKES_BITBUCKET_API_TOKEN`. See [Provider Integration](/no-mistakes/guides/provider-integration/#bitbucket-cloud).

## `NO_MISTAKES_BITBUCKET_API_TOKEN`

Bitbucket Cloud API token.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

Get one from [Bitbucket account settings](https://bitbucket.org/account/settings/app-passwords/).

## `NO_MISTAKES_BITBUCKET_API_BASE_URL`

Override the Bitbucket Cloud API base URL.

|         |                                 |
| ------- | ------------------------------- |
| Type    | `string`                        |
| Default | `https://api.bitbucket.org/2.0` |

Useful for mocking in tests or pointing at a proxy.

## `AZURE_DEVOPS_EXT_PAT`

Azure DevOps Personal Access Token inherited by the daemon for non-interactive `az` CLI auth.
Alternatively, authenticate the Azure DevOps extension with `az devops login`.

|         |                                                    |
| ------- | -------------------------------------------------- |
| Type    | `string`                                           |
| Default | (none)                                             |

See [Provider Integration](/no-mistakes/guides/provider-integration/#azure-devops).

## `NO_MISTAKES_NO_UPDATE_CHECK`

Disable background update checks.

|         |                                                |
| ------- | ---------------------------------------------- |
| Type    | `1` to disable, anything else to leave enabled |
| Default | unset (checks enabled)                         |

Update checks run on every CLI invocation except `update` itself and version queries (`--version` / `-v`, which stay side-effect-free), hit GitHub releases, cache the result in `$NM_HOME/update-check.json`, and print a one-line notification to stderr when a newer version is available. Dev builds (non-semver versions) suppress the check automatically.

## `XDG_DATA_HOME`

Data directory used to discover OpenCode transcripts for intent extraction.

|         |                  |
| ------- | ---------------- |
| Type    | `string`         |
| Default | `~/.local/share` |

When set, no-mistakes looks for OpenCode's intent transcript database at `$XDG_DATA_HOME/opencode/opencode.db`.
When unset, it falls back to `~/.local/share/opencode/opencode.db`.

## `GLAB_CONFIG_DIR`

Directory holding glab's `config.yml`, consulted when detecting self-hosted GitLab.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

When the upstream hostname carries no `gitlab` marker, no-mistakes reads glab's configured hosts from `$GLAB_CONFIG_DIR/config.yml` to decide whether the host is a GitLab instance. It takes precedence over `XDG_CONFIG_HOME`. See [Provider Integration](/no-mistakes/guides/provider-integration/#self-hosted-githubgitlab).

## `GH_CONFIG_DIR`

Directory holding gh's `hosts.yml`, consulted when detecting self-hosted GitHub Enterprise.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

When the upstream hostname is not `github.com`, no-mistakes reads gh's configured hosts from `$GH_CONFIG_DIR/hosts.yml` to decide whether the host is a GitHub Enterprise instance. It takes precedence over `XDG_CONFIG_HOME`. See [Provider Integration](/no-mistakes/guides/provider-integration/#self-hosted-githubgitlab).

## `XDG_CONFIG_HOME`

Config directory used to locate glab's `config.yml` for self-hosted GitLab detection and gh's `hosts.yml` for self-hosted GitHub Enterprise detection.

|         |             |
| ------- | ----------- |
| Type    | `string`    |
| Default | `~/.config` |

When `GLAB_CONFIG_DIR` is unset, no-mistakes looks for glab's configured hosts at `$XDG_CONFIG_HOME/glab-cli/config.yml`, falling back to `~/.config/glab-cli/config.yml` when `XDG_CONFIG_HOME` is unset.
When `GH_CONFIG_DIR` is unset, no-mistakes looks for gh's configured hosts at `$XDG_CONFIG_HOME/gh/hosts.yml`, falling back to `~/.config/gh/hosts.yml` when `XDG_CONFIG_HOME` is unset.

## `NO_MISTAKES_UMAMI_HOST`

Override the telemetry collection host.

|         |                             |
| ------- | --------------------------- |
| Type    | `URL`                       |
| Default | `https://a.kunchenguid.com` |

When set, telemetry sends events to this host's `/api/send` endpoint. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_HOST`. If no runtime value is found, it falls back to any host embedded at build time and then the default self-hosted Umami instance.

## `NO_MISTAKES_UMAMI_WEBSITE_ID`

Override or enable the telemetry website ID.

|         |                                                                         |
| ------- | ----------------------------------------------------------------------- |
| Type    | `string`                                                                |
| Default | embedded in Makefile and release builds; unset in unembedded dev builds |

When set, telemetry uses this website ID at runtime. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_WEBSITE_ID`. If no runtime value is found, it falls back to any website ID embedded at build time.

When telemetry is enabled, `no-mistakes` sends command, run, approval, fix, and wizard events, completed step events with `awaiting_approval`, `fix_review`, or `failed` status, and pageviews for the human surfaces `/wizard` and `/tui` and the state-changing agent surfaces `/axi/run`, `/axi/respond`, and `/axi/abort` to Umami.
Mutation pageviews are sent alongside command events, so command status and duration remain available.
They include only flag-derived context: `/axi/run` records whether `--yes`, `--intent`, or `--skip` was present, and `/axi/respond` records the sanitized action and whether `--yes` was present.

Read-only surfaces (`axi` home, `axi status`, `axi logs`, `status`, `runs`) emit no pageview and rate-limit their command event: it is sent when the observed run state changed since the last emit, and otherwise at most once per 10 minutes, with the dedupe state persisted at `<NM_HOME>/telemetry-gate.json` so agent polling loops stay bounded across processes.
The `axi logs` command event records the sanitized step, whether `--full` was present, and whether `--run` was present; `axi status` records whether `--run` was present.

### What stays local and what leaves the machine

Everything sent remotely is low-cardinality: command names, statuses, durations, counts, flag booleans, agent and step names, and - on the single terminal `run finished` event - the bounded performance rollup `agent_invocations`, `resumed_invocations`, and `fallback_invocations` (small counts only).
Run IDs, repository paths, branch names, session identities, prompts, model outputs, diffs, and per-invocation performance records are never sent.

Detailed performance evidence stays on the machine in the local state database (`<NM_HOME>/state.sqlite`): one `agent_invocations` row per agent invocation, plus each run's accumulated parked-at-gate time.
Each row records run and step identity, purpose (such as review/review-fix/housekeeping), the reported model and its provider, the cold/started/resumed/fallback session mode, a truncated session-identity hash, timestamps, duration, exit status, and failure category, alongside the session-fidelity metrics below.
It never stores prompts, model outputs, diffs, raw command arguments, secret values, or credentials - only bounded counts, low-cardinality categories, and durations.

The additive session-fidelity fields are nullable and read back as unknown (rendered `-`) rather than a fabricated zero when the adapter did not report them, so rows written before a field existed, and adapters that do not surface a datum, stay honest.
The legacy raw input, output, and cache-read token counters render numerically; use the nullable per-round and derived fields to determine whether the adapter reported comparable usage:

- Token detail: `input_tokens`/`output_tokens`/`cache_read_tokens` (raw, cumulative across a resumed session for codex), `fresh_input_tokens` (input minus cache reads), `cache_creation_tokens` (unknown when the provider does not surface it), `reasoning_tokens`, and `delta_input_tokens`/`delta_output_tokens`/`delta_cache_read_tokens` (the correct per-round amounts, so a resumed session's cumulative counter is never mistaken for one round's usage).
- Activity: `model_roundtrips` (a proxy for productive model turns), `tool_calls`, and a bounded tool-category histogram (`tool_wait_calls`, `tool_test_lint_calls`, `tool_edit_calls`, `tool_read_calls`, `tool_git_calls`, `tool_other_calls`); a compound command counts once per sub-command, so the histogram can sum higher than `tool_calls`.
- Timing split: `subprocess_wait_ms` is the wall-clock spent inside tool subprocesses; model/reasoning time is the invocation duration minus it, clamped at zero.
- Context: `workload_files`/`workload_lines` (bounded change size), `finding_count` (findings in the structured output), and `fallback_reason` (why a failed resume forced a fresh session, one of transient/parse/exit/spawn/unsupported/other).

The count and timing definitions live in one authoritative place (`internal/agent/invocationmetrics.go`).
Inspect the evidence with `no-mistakes stats --agents` (per-purpose aggregates, including a `METRICS` coverage count so a real zero is distinguishable from missing instrumentation) or `no-mistakes stats --run <id>` (one run's invocations, the per-round-vs-cumulative token split, and parked time).

## `NO_MISTAKES_TELEMETRY`

Disable telemetry collection.

|         |                                                                   |
| ------- | ----------------------------------------------------------------- |
| Type    | `0`, `false`, or `off` to disable; anything else to leave enabled |
| Default | unset                                                             |

When set to a disabling value, telemetry stays off even if a runtime or embedded website ID is available.

## Environment the daemon sees

When the daemon runs through a managed service (launchd, systemd user service, Task Scheduler), the macOS and Linux service definitions include a default `PATH` with common user and system binary directories. They also bake in any proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY`) that were set when you installed or refreshed the service, so the daemon and the agents it spawns can reach the network through your proxy even when the login-shell probe is unavailable. Once baked in, the values are preserved across later service refreshes and restarts even when the proxy variables are not exported in that shell, so a routine `daemon restart` or a binary upgrade will not strip them; export the variables again only when you need to change or remove them. Both the upper- and lower-case spellings are forwarded exactly as you set them, because tooling is inconsistent about which it reads (curl, for example, honors only the lower-case `http_proxy` for plain-HTTP requests). Because a proxy URL can embed credentials (for example `http://user:pass@host`), the generated service file is restricted to owner-only `0600` permissions whenever proxy values are forwarded into it. When no proxy variables are set, the generated definition is unchanged and keeps the conventional `0644` mode. Windows Task Scheduler inherits your logon environment and needs no forwarding. At daemon startup, the daemon resolves environment from your login shell on macOS and Linux, preserves your shell `PATH` order, and appends any missing well-known directories such as `~/.local/bin`, `~/go/bin`, `~/.cargo/bin`, `~/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, and `/bin`. If login-shell resolution fails or returns no entries, the daemon logs a warning and uses an augmented process-environment fallback that may omit version-manager directories such as nvm, fnm, or volta. On Windows it reuses the current process environment.

If your env vars aren't set in your login shell's rc files (`.zprofile`, `.zshrc`, `.profile`, `.bash_profile`, `.bashrc`, PowerShell profile), the daemon won't see them. Put them somewhere a login shell will load, then restart the daemon to pick them up.
