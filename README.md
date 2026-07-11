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

<p align="center"><strong>English</strong> · <a href="README.zh-CN.md">简体中文</a></p>

<p align="center">
  <img src="https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/demo.gif" alt="no-mistakes demo" width="800" />
</p>

`no-mistakes` puts a local git proxy in front of your real remote.
Push to `no-mistakes` instead of `origin` and it runs an AI-driven validation pipeline in a disposable worktree.
The branch reaches the configured push target only after every check passes, and a clean PR opens automatically.

What you get:

- an isolated pipeline that never blocks your working copy
- purpose-routed model calls with provider failover between OpenAI and Anthropic
- a `/no-mistakes` skill so your coding agent can do a task and gate it, or gate existing committed work
- repairs that are applied, checked deterministically, and independently verified before anything ships
- a clean PR raised and CI watched for you, with judgment calls left to you

Full documentation: <https://kunchenguid.github.io/no-mistakes/>

## How it works

```
        your branch
            │  git push no-mistakes
            ▼
   ┌────────────────────────────────────────────────┐
   │  disposable worktree — your work stays put     │
   │  intent → rebase → review → test → document    │
   │  → lint → verify → push → PR → CI              │
   └────────────────────────────────────────────────┘
            │  every check green
            ▼
        clean PR, opened for you
```

The pipeline always runs the same ten steps: intent, rebase, review, test, document, lint, verify, push, PR, CI.
Each step passes on its own or stops with a finding.
Safe findings are repaired for you; anything that touches your intent waits for your decision.
The push step transports only the exact commit the verify step certified.
Nothing reaches the configured push target until every check is green.

## How model calls are chosen

Every model call is routed by its purpose, with ordered provider failover between OpenAI and Anthropic.
An operational failure, such as quota or an outage, moves the rest of the run to the backup provider; when no candidate is available, the call fails closed instead of downgrading.
Repairs escalate the same way, and every fix is independently verified, until the finding resolves or fails closed.
See the [routing reference](https://kunchenguid.github.io/no-mistakes/reference/routing/) for the exact profiles, routes, and circuit rules.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

You need `git` and at least one routed runner CLI, `codex` or `claude`, on your `PATH`.
Windows, Go install, and build-from-source instructions are in the [installation guide](https://kunchenguid.github.io/no-mistakes/start-here/installation/).

## Quick start

```sh
$ no-mistakes init
  ✓ Gate initialized

    repo  /Users/you/src/my-repo
    gate  no-mistakes → /Users/you/.no-mistakes/repos/abc123def456.git
  remote  git@github.com:you/my-repo.git
   skill  /no-mistakes installed for agents at user level

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

For GitHub fork contributions, keep `origin` pointed at the parent repository and initialize with `no-mistakes init --fork-url <your-fork-url>`.

From the TUI you act on each finding.
The pipeline repairs safe findings itself and verifies each repair independently; judgment calls stop the run for you to approve, fix, or skip.
Once every check is green, the gate forwards the verified commit to the configured push target and opens the PR for you, so there is no manual `git push origin` and no hand-written PR body.
Prefer to let your coding agent drive the same flow headlessly?
Use `/no-mistakes` (see below).

## Three ways to trigger the gate

Every change runs through the same pipeline.
Pick the entry point that fits how you are working when the change is ready:

- `git push no-mistakes` - the explicit Git path: push a committed branch to the gate remote instead of `origin`
- `no-mistakes` - the TUI: run it after making changes (no commit needed) and a wizard walks you through branch, commit, and push, then attaches to the run; `no-mistakes -y` does all of that automatically
- `/no-mistakes` - the agent skill: tell the coding agent to do a task and gate it with `/no-mistakes <task>`, or use bare `/no-mistakes` to gate existing committed work; it lets the pipeline repair safe findings and stops to ask you about anything that needs a human call

`no-mistakes init` installs the `/no-mistakes` skill for Claude Code and other agents.
Under the hood the skill drives `no-mistakes axi`, a non-interactive TOON interface to the same approval flow.

See the [quick start](https://kunchenguid.github.io/no-mistakes/start-here/quick-start/) for the full first-run walkthrough.

## Development

```sh
make build   # Build bin/no-mistakes with version info
make test    # Run go test -race ./... (excludes the e2e suite)
make e2e     # Run the tagged end-to-end agent journey suite
make e2e-record # Re-record e2e fixtures when agent wire formats change
make lint    # Check generated skill drift and run go vet ./...
make skill   # Regenerate committed no-mistakes skill files
make fmt     # Run gofmt -w .
make demo    # Regenerate demo.gif and demo.mp4 (needs vhs and ffmpeg)
make docs    # Build the Astro docs site in docs/dist
```

See `Makefile` for the full target list.

`make e2e-record` overwrites `internal/e2e/fixtures/` from the real `claude`, `codex`, and `opencode` CLIs, spends real API quota, and should be reviewed before committing.

## Star History

<a href="https://www.star-history.com/?repos=kunchenguid%2Fno-mistakes&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=kunchenguid/no-mistakes&type=date&theme=dark&legend=top-left" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=kunchenguid/no-mistakes&type=date&legend=top-left" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=kunchenguid/no-mistakes&type=date&legend=top-left" />
 </picture>
</a>
