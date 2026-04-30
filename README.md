<h1 align="center"><code>git push no-mistakes</code></h1>
<p align="center">
  <a href="https://github.com/kunchenguid/no-mistakes/actions/workflows/release.yml"
    ><img
      alt="Release"
      src="https://img.shields.io/github/actions/workflow/status/kunchenguid/no-mistakes/release.yml?style=flat-square&label=release"
  /></a>
  <a href="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square"
    ><img
      alt="Platform"
      src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square"
  /></a>
  <a href="https://x.com/kunchenguid"
    ><img
      alt="X"
      src="https://img.shields.io/badge/X-@kunchenguid-black?style=flat-square"
  /></a>
  <a href="https://discord.gg/Wsy2NpnZDu"
    ><img
      alt="Discord"
      src="https://img.shields.io/discord/1439901831038763092?style=flat-square&label=discord"
  /></a>
</p>

<h3 align="center">Kill all the slop. Raise clean PR.</h3>

<p align="center">
  <img src="https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/demo.gif" alt="no-mistakes demo" width="800" />
</p>

`no-mistakes` puts a local git proxy in front of your real remote. Push to `no-mistakes` instead of `origin`, and it spins up a disposable worktree, runs an AI-driven validation pipeline, forwards upstream only after every check passes, and opens a clean PR automatically.

- **Non-blocking** - the pipeline runs in an isolated worktree without disrupting your work.
- **Agent-agnostic** - `claude`, `codex`, `rovodev`, `opencode`, or `pi`.
- **Human stays in charge** - auto-fix or review findings, your call.
- **Clean PRs by default** - push, open PR, watch CI, and auto-fix failures in one shot.

Full documentation: <https://kunchenguid.github.io/no-mistakes/>

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

Windows, Go install, and build-from-source instructions are in the [installation guide](https://kunchenguid.github.io/no-mistakes/start-here/installation/).

## Quick Start

```sh
$ no-mistakes init
  ✓ Gate initialized

    repo  /Users/you/src/my-repo
    gate  no-mistakes → /Users/you/.no-mistakes/repos/abc123def456.git
  remote  git@github.com:you/my-repo.git

  Push through the gate with:
  git push no-mistakes <branch>

$ git checkout my-branch

# do some work in the branch...

$ git push no-mistakes
  * Pipeline started

  Run no-mistakes to review.

$ no-mistakes
# opens the TUI for the active run
```

You can also skip `git push` and run `no-mistakes` directly after making changes. The setup wizard walks you through creating a branch, committing, and pushing through the gate, then attaches if the daemon registers the new run.

See the [quick start](https://kunchenguid.github.io/no-mistakes/start-here/quick-start/) for the full first-run walkthrough.

## Development

```sh
make build   # Build bin/no-mistakes with version info
make test    # Run go test -race ./... (excludes the e2e suite)
make e2e     # Run the tagged end-to-end agent journey suite
make e2e-record # Re-record e2e fixtures when agent wire formats change
make lint    # Run go vet ./...
make fmt     # Run gofmt -w .
make demo    # Regenerate demo.gif and demo.mp4 (needs vhs and ffmpeg)
make docs    # Build the Astro docs site in docs/dist
```

See `Makefile` for the full target list.

`make e2e-record` overwrites `internal/e2e/fixtures/` from the real `claude`, `codex`, and `opencode` CLIs, spends real API quota, and should be reviewed before committing.
