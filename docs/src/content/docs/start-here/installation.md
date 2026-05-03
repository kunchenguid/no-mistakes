---
title: Installation
description: All install options, prerequisites, update, and uninstall.
---

## macOS / Linux

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

The installer keeps the real binary in `~/.no-mistakes/bin` and exposes `no-mistakes` through a symlink in `~/.local/bin` or `/usr/local/bin`. That keeps future `no-mistakes update` runs in a user-owned location instead of rewriting a system binary in place.

It also installs or refreshes the background daemon for you by running `no-mistakes daemon restart`, preferring a managed service (launchd on macOS, systemd user service on Linux) and falling back to a detached daemon if that path is unavailable. If the restart fails, the install command fails.

Official release binaries installed this way include the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

## Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.ps1 | iex
```

Installs the binary and restarts the background daemon automatically with `no-mistakes.exe daemon restart`, preferring a managed Task Scheduler task and falling back to a detached daemon if needed. If the restart fails, the install command fails.

Official release binaries installed this way include the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

## Go install

```sh
go install github.com/kunchenguid/no-mistakes/cmd/no-mistakes@latest
```

`go install` builds the CLI without an embedded telemetry website ID, so telemetry stays off by default unless you later set `NO_MISTAKES_UMAMI_WEBSITE_ID` at runtime.

## From source

```sh
git clone git@github.com:kunchenguid/no-mistakes.git
cd no-mistakes
make build
make install
```

`make build` embeds the telemetry host from `NO_MISTAKES_UMAMI_HOST` in a repo-local `.env` first, then `UMAMI_HOST` from the shell, then the default self-hosted host. It embeds the telemetry website ID from `NO_MISTAKES_UMAMI_WEBSITE_ID` in `.env` first, then `UMAMI_WEBSITE_ID` from the shell, then the default website ID.

## Prerequisites

- **git** - required
- **One supported agent binary** - `claude`, `codex`, `acli` (Rovo Dev), `opencode`, or `pi`, or a separately installed `acpx` binary for `agent: acp:<target>`
- **Optional, for PRs and CI:**
  - `gh` CLI (GitHub)
  - `glab` CLI (GitLab)
  - `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN` (Bitbucket Cloud)

Run `no-mistakes doctor` to check what's installed.

See [Provider Integration](/no-mistakes/guides/provider-integration/) for PR and CI setup per host.

## Update

```sh
no-mistakes update
no-mistakes update --beta
```

This downloads the latest release from GitHub, verifies the SHA-256 checksum, atomically replaces the binary, and resets the daemon so it picks up the new executable. It prefers the managed service path and falls back to a detached daemon if service startup is unavailable or fails.

`no-mistakes update` installs the latest stable release. Use `no-mistakes update --beta` to opt into prereleases and install the latest beta when one is newer than the current stable release.

Because `update` installs the latest official release binary, it installs a binary with the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

It only proceeds if the running daemon is already using the same executable path. If the daemon executable path cannot be determined or it was started from a different binary, the update aborts before replacing the binary. If the daemon does not come back cleanly after a successful replacement, the new binary stays installed but the command reports the daemon reset failure.

Background update checks run automatically on each CLI invocation (except `update` itself). Suppress with `NO_MISTAKES_NO_UPDATE_CHECK=1`.

## Remove from a repo

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo, cleans up worktrees, and removes the database record.

## Uninstall

Stop the daemon, delete the binary, and clear state:

```sh
no-mistakes daemon stop
rm -f ~/.local/bin/no-mistakes /usr/local/bin/no-mistakes
rm -rf ~/.no-mistakes
```

On macOS, also remove `~/Library/LaunchAgents/com.kunchenguid.no-mistakes.daemon.*.plist`. On Linux, also remove `~/.config/systemd/user/no-mistakes-daemon-*.service`. On Windows, remove the `no-mistakes-daemon-*` Task Scheduler task.
