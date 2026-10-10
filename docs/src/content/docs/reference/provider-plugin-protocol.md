---
title: Provider Plugin Protocol
description: The AXI-shaped CLI contract a provider plugin implements to give no-mistakes PR and CI support for a host it does not ship.
---

A provider plugin is an executable you configure under global
[`provider_plugins`](/no-mistakes/reference/global-config/#provider_plugins). It
lets the PR and CI steps work against any forge - a company-internal host,
Google Cloud Secure Source Manager, or anything else with an API - without that
forge's code living in no-mistakes. This page is the contract the executable
implements.

The plugin is an **AXI-shaped CLI**: subcommands such as `pr find` and
`pr checks`, each printing one JSON document when called with `--json`. It is
the same shape no-mistakes uses to drive
[`forgejo-axi`](/no-mistakes/reference/global-config/#forgejo_axi_path), so the same
binary can print a human- or agent-friendly format (for example TOON) when
`--json` is absent. no-mistakes always passes `--json` and only ever reads
JSON.

## Invocation model

Every operation is one process:

```text
<command> <configured args...> <subcommand words> [operation args] <context flags> --json
```

For example:

```text
no-mistakes-ssm --profile work pr view 7 --plugin=ssm --repo=my-project/my-repo --host=my-instance-123456-git.us-central1.sourcemanager.dev --raw-host=my-instance-123456-git.us-central1.sourcemanager.dev --remote-url=https://my-instance-123456-git.us-central1.sourcemanager.dev/my-project/my-repo.git --json
```

- **Argument form.** Positional arguments (the subcommand words, then a PR
  number where one is taken) come first. Every flag that carries a value is a
  single `--name=value` argument, never `--name value`, so a value that starts
  with `-` (a PR title such as `-fix: typo`) can never be read as an option.
  Boolean flags (`--draft`, `--failed`, `--json`) take no value. `--json` is
  always the last argument. Any argument parser that understands
  `--name=value` works (Python `argparse`, Go `flag` or `cobra`, `click`).
- **One process per operation.** The plugin keeps no connection open between
  operations; persist anything you need (caches, tokens) yourself.
- **Stdin is empty.** The plugin reads EOF immediately. Nothing is ever sent on
  stdin.
- **PR bodies travel in a file.** `pr create` and `pr update` pass
  `--body-file=<path>`: a temporary file no-mistakes creates with mode `0600`
  (readable only by the daemon's user), outside the worktree, and deletes as
  soon as the process exits. Read it during the call; do not keep the path. A
  body can therefore exceed argv limits, survive Windows `.cmd` shims (which
  cut arguments at the first newline), contain lines starting with `-`, and
  never appear in a process listing. No secrets are passed in argv.
- **Environment and working directory.** The plugin inherits the run's
  environment - the daemon's login-shell environment plus any forge-profile
  overlay - exactly like built-in provider CLIs, and runs with the run
  worktree as its working directory. Do not trust files in the worktree: they
  come from the pushed branch.
- **Timeout.** Each invocation is bounded by `timeout` (default `2m`). On
  expiry the process tree is killed and the operation fails closed.
- **Limits.** Stdout is capped at 1 MiB (4 MiB for `pr check-logs`); larger
  output fails the call. Only the first 64 KiB of stderr is kept.
  Plugin-supplied messages are credential-redacted and capped at 4 KiB before
  they reach logs.

### Context flags

Every invocation carries all five, mirroring `forgejo-axi`'s `--repo`:

| Flag | Meaning |
| --- | --- |
| `--plugin` | The `provider_plugins` key, so one executable can serve several entries. |
| `--repo` | The repository path in the remote, without `.git` (for example `my-project/my-repo`). |
| `--host` | The remote's host after SSH `HostName` resolution. |
| `--raw-host` | The literal host token in the remote (an SSH alias for alias remotes). |
| `--remote-url` | The run's upstream remote with any credentials redacted. |

## Output

**Success:** exit `0` and print exactly one JSON object on stdout. That object
*is* the result; there is no wrapper:

```json
{ "pr": { "number": 7, "url": "https://host/owner/repo/pulls/7", "head_branch": "feature", "base_branch": "main" } }
```

**Failure:** exit non-zero. Stdout may carry one failure document:

```json
{ "error": { "code": "unauthenticated", "message": "run `gcloud auth login`" } }
```

Without one, no-mistakes reports the (redacted, capped) stderr instead.

Rules:

- Unknown fields inside a result are ignored, so a plugin can add its own
  diagnostics.
- A top-level `error` key is reserved for failures. Exit `0` with an `error`
  object is a contract violation, never a result.
- Exit `0` with empty stdout, non-JSON stdout, a JSON value that is not an
  object, or a second JSON document after the first is a contract violation.

Error codes with defined meaning:

| Code | Meaning |
| --- | --- |
| `head_changed` | The PR head is no longer the expected commit (`pr merged`). |
| `unsupported` | Never a valid answer: no-mistakes invokes only the required subcommands and the ones you declared. It is reported as a contract violation (capability mismatch). |

Any other code, or none, is an ordinary failure whose message is shown to the
operator.

## Failing closed

no-mistakes treats two kinds of failure differently:

- **The plugin cannot serve the repository.** `status` exits non-zero (not
  authenticated, wrong account, repository unknown), or the configured command
  does not resolve. The PR and CI steps skip with the plugin's message, exactly
  like a built-in provider whose CLI is not logged in. The skip is not silent:
  it is recorded as the step's skip reason and `axi` reports the run as
  `passed-with-skips` with the reason under `run.automatic_skips`. Make the
  message actionable (for example "run `gcloud auth login`").
- **The plugin broke this contract.** Unreadable output, a `protocol_version`
  other than `1`, a timeout, oversized output, a missing required key, an
  unknown enum value, a PR identity that does not match the request, or an
  `unsupported` answer. The step fails (and the pre-push attestation of the
  push step or of a CI repair refuses to push) instead of skipping or
  parking, because a skip would let the run read as a pass off an answer
  nothing validated, and a park would present a broken integration as a
  decision for the operator.

A contract violation in any later subcommand fails the step the same way:
the CI step does not poll or repair again after a malformed `pr checks`,
`pr view`, `pr mergeability`, or `pr check-logs` answer. Timeouts have two
exceptions, both of which never count as a pass: one during the CI step's
repeated `pr view`, `pr checks`, or `pr mergeability` polls is retried like
any failed read, and a timed-out `pr check-logs` lets the repair proceed
with the logs marked unavailable. A timeout anywhere else, including the CI
step's one-time read of the PR's live base branch, fails closed. Any other
failure of a subcommand after `status` fails or parks the step the same way
a built-in provider's failure does; it is never read as an empty answer.

## Objects

### PR

```json
{ "number": 7, "url": "https://host/owner/repo/pulls/7", "head_branch": "feature", "base_branch": "main", "head_sha": "abc123" }
```

| Field | Rules |
| --- | --- |
| `number` | Positive base-10 integer, as a JSON number or string (`7` or `"7"`, never `"07"`). |
| `url` | `http` or `https`, no user info, and **must end with the PR number** (`.../pulls/7`, `.../merge_requests/7`). Runs persist only the URL, and later steps recover the number from it. Its path must contain the `--repo` path as whole segments (case-insensitive, so `https://web.example.com/my-project/my-repo/pulls/7` names `my-project/my-repo`); the host is not compared, because a forge's web host may differ from its git host. Once a run holds a PR's URL, every `pr view`, `pr update`, `pr retarget`, and `pr merged` answer must return exactly that URL. |
| `head_branch` | The PR's source branch. Required where identity is checked (`pr find`, `pr create`). |
| `base_branch` | The PR's target branch. Required where identity is checked and for base reads. |
| `head_sha` | Optional current head commit. |

### Check

```json
{ "name": "build", "id": "run-42", "bucket": "fail", "state": "FAILURE", "link": "https://ci/run/42", "started_at": "2026-01-02T03:04:05Z", "completed_at": "2026-01-02T03:09:00Z", "execution_id": "42" }
```

| Field | Rules |
| --- | --- |
| `name` | Required, non-empty. Shown in findings and passed back to `pr check-logs`. |
| `bucket` | Required: `pass`, `fail`, `pending`, `cancel`, or `skipping`. Map your forge's states onto these; anything else fails the call. |
| `id`, `state`, `link`, `execution_id` | Optional provider identifiers and raw state, surfaced for diagnostics. |
| `started_at`, `completed_at` | Optional RFC 3339 timestamps. |

## Subcommands

Required subcommands are invoked whenever the PR or CI step needs them.
Capability-gated subcommands are invoked only when `status` declared the
capability `true`.

### status (required)

The handshake, run before every step that uses the plugin. Exit non-zero when
the plugin is not authenticated for the repository or cannot serve it.

- **args:** none
- **result:**

```json
{
  "protocol_version": 1,
  "capabilities": { "mergeable_state": true, "failed_check_logs": true, "merged_proof": true, "set_pr_base_branch": false },
  "max_pr_body_chars": 0
}
```

`protocol_version` must be `1`. Every capability defaults to `false`; a
subcommand whose capability is `false` is never invoked. `max_pr_body_chars`
is required: the forge's description limit counted in UTF-16 code units, or
an explicit `0` for unlimited (a missing key fails the handshake rather than
reading as unlimited). The PR step fits its body under it, and a body over it
is refused before any write.

### pr find (required)

Print the **open** PR whose head is `--head` (and whose base is `--base`, when
given), or `null`.

- **args:** `--head=<branch> [--base=<branch>]` (no `--base` means any base)
- **result:** `{ "pr": PR | null }`

The `pr` key is required: only an explicit `null` means there is no open PR,
and a response without the key fails the call. The returned `head_branch`
(and `base_branch`, when `--base` was given) must match, or the call fails.
This is what prevents duplicate PRs.

### pr create (required)

- **args:** `--head=<branch> --base=<branch> --title=<title> [--draft] --body-file=<path>`
- **result:** `{ "pr": PR }` whose `head_branch` and `base_branch` equal the request.

`--draft` is passed when the plugin entry sets `draft_pull_requests: true`.

### pr update (required)

Replace the description with the content of `--body-file`; set the title too
when `--title` is passed (no `--title` means "leave it unchanged").

- **args:** `<number> [--title=<title>] --body-file=<path>`
- **result:** `{ "pr": PR }` with the same number.

### pr view (required)

- **args:** `<number>`
- **result:** `{ "pr": PR, "state": "open" | "merged" | "closed", "title": "...", "body": "..." }`

`title` and `body` must both be present (an empty string is fine, a missing
or `null` value is not): template-preserving publication and pre-push
attestation never overwrite text they did not read. `pr.base_branch` is
required, because an existing PR's live base outranks configuration.

### pr checks (required)

Print the checks for the PR's current head.

- **args:** `<number> [--head-sha=<sha>]`
- **result:** `{ "checks": [Check, ...] }`

The `checks` key is required. An empty list means "nothing registered yet";
a missing key is an unreadable response and is never treated as empty. If the
repository has no CI at all, declare
[`no_ci: true`](/no-mistakes/reference/repo-config/#no_ci) instead of returning an
empty list forever.

### pr mergeability (capability `mergeable_state`)

- **args:** `<number>`
- **result:** `{ "state": "mergeable" | "conflicting" | "pending" | "unknown" }`

Enables merge-conflict detection and agent-driven conflict repair in the CI
step.

### pr check-logs (capability `failed_check_logs`)

Print the logs of the named failed checks.

- **args:** `<number> --failed [--branch=<branch>] [--head-sha=<sha>] --check=<name> [--check=<name> ...]`
- **result:** `{ "logs": "..." }` (use `""` when there are none; a missing key fails)

`--failed` is always passed in protocol version 1. The CI auto-fix prompt
keeps the last 1 MiB, where build and test failures are reported.

### pr merged (capability `merged_proof`)

Prove that the PR merged at the commit no-mistakes validated.

- **args:** `<number> --expected-head=<sha>`
- **result:**

```json
{ "merged": true, "number": 7, "url": "https://.../pulls/7", "head_sha": "abc123", "merge_commit_sha": "def456", "merged_at": "2026-01-02T03:04:05Z", "merged_by": "alice" }
```

`number` and `url` must identify the requested PR (the same URL you returned
for it), `head_sha` must equal `--expected-head` (or fail with code
`head_changed`), and a merged proof must include `merge_commit_sha` and
`merged_at`.

### pr retarget (capability `set_pr_base_branch`)

Retarget an existing PR when a per-run `--base-branch` override disagrees
with its live base.

- **args:** `<number> --base=<branch>`
- **result:** `{ "pr": PR }` whose `base_branch` is the new base.

Without this capability, such a run fails closed instead of opening a second
PR.

## What plugins do not cover

These are built-in-provider features with no plugin subcommand in protocol
version 1; with a plugin they behave as they do for providers that lack them:

- Fork PR routing: a repository with a fork URL skips the PR and CI steps
  with the reason `fork PR routing for provider plugin "<name>" is not
  implemented`, reported under `run.automatic_skips` like every non-GitHub
  provider. It never opens a PR from the upstream repository to itself.
- Review-bot findings and transient-check reruns at the CI gate.
- `axi run --closes` closing references (refused, as on every provider that does
  not declare the closing-reference capability) and evidence media uploads
  and evidence-branch links.

PR bodies use the same GitHub-flavored Markdown (with `<details>` blocks) as
GitHub, GitLab, and Gitea.

## Minimal plugin skeleton

A plugin can be written in any language. This Python skeleton shows the
argument parsing and output rules; the `forge_*` calls are where your forge's
API goes.

```python
#!/usr/bin/env python3
import argparse
import json
import sys


def ok(result):
    json.dump(result, sys.stdout)
    return 0


def fail(message, code=""):
    json.dump({"error": {"code": code, "message": message}}, sys.stdout)
    return 1


def parser():
    root = argparse.ArgumentParser(prog="no-mistakes-ssm")
    # Configured `args` come before the subcommand words, so the root parser
    # must declare them (here the `--profile work` from the example above).
    root.add_argument("--profile")
    ctx = argparse.ArgumentParser(add_help=False)
    for name in ("--plugin", "--repo", "--host", "--raw-host", "--remote-url"):
        ctx.add_argument(name, required=True)
    ctx.add_argument("--json", action="store_true", required=True)

    sub = root.add_subparsers(dest="cmd", required=True)
    sub.add_parser("status", parents=[ctx])
    pr = sub.add_parser("pr").add_subparsers(dest="op", required=True)

    find = pr.add_parser("find", parents=[ctx])
    find.add_argument("--head", required=True)
    find.add_argument("--base")

    create = pr.add_parser("create", parents=[ctx])
    for name in ("--head", "--base", "--title", "--body-file"):
        create.add_argument(name, required=True)
    create.add_argument("--draft", action="store_true")

    update = pr.add_parser("update", parents=[ctx])
    update.add_argument("number")
    update.add_argument("--title")
    update.add_argument("--body-file", required=True)

    pr.add_parser("view", parents=[ctx]).add_argument("number")
    checks = pr.add_parser("checks", parents=[ctx])
    checks.add_argument("number")
    checks.add_argument("--head-sha")
    return root


def main():
    args = parser().parse_args()
    repo = {"path": args.repo, "host": args.host, "remote_url": args.remote_url}
    try:
        if args.cmd == "status":
            forge_check_auth(repo)  # raise with an actionable message
            return ok({"protocol_version": 1, "capabilities": {}, "max_pr_body_chars": 0})
        if args.op == "find":
            return ok({"pr": forge_find_open_pr(repo, args.head, args.base)})
        if args.op == "create":
            with open(args.body_file, encoding="utf-8") as f:
                body = f.read()
            return ok({"pr": forge_create_pr(repo, args.head, args.base, args.title, body, args.draft)})
        if args.op == "update":
            with open(args.body_file, encoding="utf-8") as f:
                body = f.read()
            return ok({"pr": forge_update_pr(repo, args.number, args.title, body)})
        if args.op == "view":
            pr, state, title, body = forge_get_pr(repo, args.number)
            return ok({"pr": pr, "state": state, "title": title, "body": body})
        if args.op == "checks":
            return ok({"checks": forge_list_checks(repo, args.number, args.head_sha)})
        return fail(f"{args.cmd} {args.op} is not implemented")
    except Exception as exc:  # every failure exits non-zero
        return fail(str(exc))


if __name__ == "__main__":
    sys.exit(main())
```

This skeleton declares no optional capabilities, so the optional subcommands
are never invoked. Declare every option your configured `args` use on the
root parser, as `--profile` is above. argparse exits with status `2` and a
usage message on stderr for arguments it does not understand, which
no-mistakes reports as an ordinary failure.

The repository's own reference implementation, used by its tests, lives in
`internal/scm/plugin/fakeplugin`.
