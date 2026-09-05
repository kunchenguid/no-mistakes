#!/usr/bin/env python3
"""Enforce that a pull request was raised through the no-mistakes pipeline.

This is the single shared implementation of the `PR must be raised via
no-mistakes` gate. Enforcing repositories can call the `require-no-mistakes`
composite action instead of copying this logic into their own workflows; the
inline copies drifted (several fleet copies never gained the head_sha bind),
which is exactly what this file exists to prevent.

The verdict is a pure function of the pull request body plus the PR head SHA:

  1. the body carries the no-mistakes signature line;
  2. the body carries a parseable v1 pipeline-step attestation comment;
  3. the attestation's head_sha equals the current PR head SHA, so a later push
     cannot pass on an older attestation;
  4. review, test, and document each recorded status == "completed". Skips
     (quota or agent) and failures are not compliant.

Nothing here reads the repository contents, so a fork's code is never executed.

The PR body and head SHA compared in step 3 are read live from the GitHub API
whenever possible (see live_pr_facts()), not from the workflow's own cached
event payload. A GitHub Actions job RERUN replays the event payload archived
at the run's ORIGINAL trigger rather than delivering a fresh one; re-running
an old, already-superseded failed run therefore reproduces its stale verdict
with a brand-new timestamp, which both GitHub's own required-check view and
this repository's check-collapsing logic (internal/scm/github collapseLatestByName)
treat as the current state - permanently resurrecting a stale failure on an
otherwise-green commit with no clean recovery short of a new SHA. Live facts
close that hole: a rerun of any age re-derives its verdict from the PR's
actual current state instead of a frozen snapshot.

When a live lookup is REQUIRED (no explicit pr-body/pr-head-sha input was
forwarded) and it fails, main() fails the whole gate closed rather than
falling back to the cached event payload: evaluating compliance against that
payload is precisely the staleness hole described above, so a lookup failure
must never itself become a route to a passing verdict on stale data. Grant
this workflow `pull-requests: read`, or forward explicit pr-body/pr-head-sha
inputs, to avoid depending on the live lookup at all.

NON-GOAL: this gate is a CONTRIBUTOR GUARDRAIL, not a forgery-proof security
boundary. The signature line and the attestation are author-editable assertions
published in the PR body, so a hand-written body that reproduces the documented
format passes this check and exits 0. That is a known and accepted limitation,
and a pre-existing one: it is inherited verbatim from the inline gate this file
consolidates, not introduced by consolidating it. What the gate does reliably
catch is the case it exists for - a contributor who bypassed the pipeline by
accident, a malformed or incomplete declaration, and an attestation left stale
by a later push. It authorizes nothing against an author who forges the format
on purpose. Authenticated (signed) attestations are the robust fix and are
tracked separately as backlog item nm-signed-attestations-r1; do not build them
into this file.
"""

from __future__ import annotations

import fnmatch
import json
import os
import sys
import urllib.error
import urllib.request

SIGNATURE_MARKER = (
    "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
)
ATTESTATION_PREFIX = "<!-- no-mistakes-pipeline-attestation:v1 "
ATTESTATION_CLOSING = " -->"

# Fixed on purpose: these are the steps whose completion the gate certifies. A
# caller configures WHO is exempt, never WHICH steps are required, so a repo
# cannot quietly weaken the gate while still reporting the same check name.
REQUIRED_STEPS = ("review", "test", "document")

VERSION_FLOOR = "1.46.0"
VERSION_FLOOR_PR = "https://github.com/kunchenguid/no-mistakes/pull/670"


def env(name: str) -> str:
    return (os.environ.get(name) or "").strip()


def event_payload() -> dict:
    """Read the workflow event payload, so a caller need not forward PR facts."""
    path = os.environ.get("GITHUB_EVENT_PATH") or ""
    if not path or not os.path.exists(path):
        return {}
    try:
        with open(path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except (OSError, json.JSONDecodeError):
        return {}
    if not isinstance(payload, dict):
        return {}
    pull_request = payload.get("pull_request")
    return pull_request if isinstance(pull_request, dict) else {}


LIVE_LOOKUP_TIMEOUT_SECONDS = 10


def live_pr_facts(number: str) -> dict | None:
    """Fetch the PR's current body and head SHA directly from the GitHub API.

    This is the only source Facts uses for body/head_sha whenever a caller
    forwards no explicit pr-body/pr-head-sha input. See the module docstring
    for why the cached event_payload() is untrustworthy specifically on an
    Actions job rerun, and why main() fails the whole gate closed - rather
    than falling back to event_payload() - when this returns None in that
    situation.

    Returns None - never raises - whenever a live lookup cannot be attempted
    (no token, no repo, no PR number) or fails for any reason (network error,
    non-2xx response, unexpected body). A caller that has not granted the
    token `pull-requests: read` degrades the same way: the request 403s and
    this returns None.
    """
    token = env("GITHUB_TOKEN")
    repo = env("GITHUB_REPOSITORY")
    if not token or not repo or not number:
        return None
    api_url = env("GITHUB_API_URL") or "https://api.github.com"
    request = urllib.request.Request(
        f"{api_url}/repos/{repo}/pulls/{number}",
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=LIVE_LOOKUP_TIMEOUT_SECONDS) as response:
            if response.status != 200:
                return None
            payload = json.load(response)
    except (urllib.error.URLError, OSError, json.JSONDecodeError, ValueError):
        return None
    if not isinstance(payload, dict):
        return None
    body = payload.get("body")
    head = payload.get("head") if isinstance(payload.get("head"), dict) else {}
    head_sha = head.get("sha")
    if not isinstance(body, str) or not isinstance(head_sha, str) or not head_sha:
        return None
    return {"body": body, "head_sha": head_sha}


def parse_list(raw: str) -> list[str]:
    """Split a newline- or comma-separated input into trimmed, non-empty items."""
    items: list[str] = []
    for line in raw.replace(",", "\n").splitlines():
        value = line.strip()
        if value:
            items.append(value)
    return items


def parse_bool(raw: str) -> bool:
    return raw.strip().lower() in ("true", "1", "yes", "on")


def emit_output(name: str, value: str) -> None:
    path = os.environ.get("GITHUB_OUTPUT") or ""
    if not path:
        return
    try:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(f"{name}={value}\n")
    except OSError:
        pass


def fail(message: str) -> "NoReturn":  # type: ignore[name-defined]
    sys.stderr.write(message)
    emit_output("compliant", "false")
    emit_output("exempt", "false")
    raise SystemExit(1)


class Facts:
    def __init__(self) -> None:
        payload = event_payload()
        head = payload.get("head") if isinstance(payload.get("head"), dict) else {}
        user = payload.get("user") if isinstance(payload.get("user"), dict) else {}

        self.head_ref = env("PR_HEAD_REF") or _payload_str(head, "ref").strip()
        self.author = env("PR_AUTHOR") or _payload_str(user, "login").strip()
        number = env("PR_NUMBER")
        if not number:
            raw_number = payload.get("number")
            number = str(raw_number) if isinstance(raw_number, int) else ""
        self.number = number

        # Explicit pr-body/pr-head-sha inputs are a caller driving this from a
        # non-pull_request event (see README.md); they always win and are
        # never second-guessed against a live lookup or the event payload.
        explicit_body = os.environ.get("PR_BODY") or ""
        explicit_head_sha = env("PR_HEAD_SHA")
        self.live_lookup_attempted = False
        self.live_lookup_used = False
        live = None
        if not explicit_body and not explicit_head_sha:
            self.live_lookup_attempted = True
            live = live_pr_facts(self.number)
        if live is not None:
            self.live_lookup_used = True
            self.body = live["body"]
            self.head_sha = live["head_sha"]
        else:
            self.body = explicit_body or _payload_str(payload, "body")
            self.head_sha = explicit_head_sha or _payload_str(head, "sha").strip()


def _payload_str(payload: dict, key: str) -> str:
    value = payload.get(key)
    return value if isinstance(value, str) else ""


def exemption_reason(facts: Facts) -> str:
    """Return why this PR is exempt from the gate, or "" when it is not."""
    authors = parse_list(os.environ.get("NM_EXEMPT_AUTHORS") or "")
    if facts.author and facts.author in authors:
        return f"author {facts.author} is a configured exempt author"

    if parse_bool(os.environ.get("NM_EXEMPT_BOT_AUTHORS") or "") and facts.author.endswith("[bot]"):
        return f"author {facts.author} is a bot and bot authors are exempt"

    for pattern in parse_list(os.environ.get("NM_EXEMPT_HEAD_BRANCHES") or ""):
        if facts.head_ref and fnmatch.fnmatchcase(facts.head_ref, pattern):
            return f"head branch {facts.head_ref} matches exempt pattern {pattern}"

    return ""


def check_signature(facts: Facts) -> None:
    if SIGNATURE_MARKER in facts.body:
        return
    fail(
        "::error::This PR was not raised through no-mistakes.\n\n"
        "Contributions to this repository must be submitted via 'git push no-mistakes'.\n"
        "That pipeline runs the required review/test/lint/CI steps and writes a\n"
        "deterministic '## Pipeline' section into the PR body containing:\n\n"
        f"    {SIGNATURE_MARKER}\n\n"
        "See CONTRIBUTING.md for setup and the full workflow.\n\n"
        f"PR author: {facts.author}\n"
    )


def fail_missing_attestation(facts: Facts) -> "NoReturn":  # type: ignore[name-defined]
    fail(
        "::error::This PR is missing structured pipeline step attestation.\n\n"
        f"This repository requires no-mistakes >= {VERSION_FLOOR} "
        f"({VERSION_FLOOR_PR}). "
        "Older no-mistakes that only writes the signature line is not enough.\n\n"
        "The PR body must include a comment of the form:\n"
        '    <!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"...","steps":[...]} -->\n\n'
        "Contributions to this repository must be submitted via 'git push no-mistakes'.\n"
        "See CONTRIBUTING.md for setup and the full workflow.\n\n"
        f"PR author: {facts.author}\n"
    )


def parse_attestation(facts: Facts) -> dict:
    start = facts.body.find(ATTESTATION_PREFIX)
    if start < 0:
        fail_missing_attestation(facts)
    start += len(ATTESTATION_PREFIX)
    end = facts.body.find(ATTESTATION_CLOSING, start)
    if end < 0:
        fail_missing_attestation(facts)
    try:
        payload = json.loads(facts.body[start:end])
    except json.JSONDecodeError:
        fail_missing_attestation(facts)
    if not isinstance(payload, dict):
        fail_missing_attestation(facts)
    if not isinstance(payload.get("head_sha"), str) or not isinstance(payload.get("steps"), list):
        fail_missing_attestation(facts)
    return payload


def check_head_bind(facts: Facts, attested_head: str) -> None:
    """Bind the attestation to the commit the forge currently has for this PR.

    Without this the gate certifies a body, not a commit: a compliant PR can be
    pushed to afterwards and the stale attestation would still pass. This is the
    piece the drifted fleet copies were missing.
    """
    if attested_head and facts.head_sha and attested_head == facts.head_sha:
        return
    fail(
        "::error::Pipeline attestation head_sha does not match the current PR head.\n\n"
        f"attestation.head_sha: {attested_head or '(missing)'}\n"
        f"PR head: {facts.head_sha or '(missing)'}\n\n"
        "A later push must not pass on an older attestation. "
        "Re-run 'git push no-mistakes' so the PR body attestation binds to the current head.\n\n"
        "See CONTRIBUTING.md for setup and the full workflow.\n\n"
        f"PR author: {facts.author}\n"
    )


def check_required_steps(facts: Facts, steps: list) -> None:
    status_by_step: dict[str, str] = {}
    for item in steps:
        if not isinstance(item, dict):
            fail_missing_attestation(facts)
        name = item.get("step")
        status = item.get("status")
        if not isinstance(name, str) or name == "" or not isinstance(status, str):
            fail_missing_attestation(facts)
        status_by_step[name] = status

    incomplete = []
    for name in REQUIRED_STEPS:
        status = status_by_step.get(name)
        if status == "completed":
            continue
        if status is None:
            incomplete.append(f"{name} (missing)")
        else:
            incomplete.append(f"{name} (status={status})")

    if not incomplete:
        return
    listed = ", ".join(incomplete)
    fail(
        f"::error::Required no-mistakes pipeline steps are not completed: {listed}.\n\n"
        "This repository requires "
        f"{', '.join(REQUIRED_STEPS)} to have status=completed. "
        "Quota skips and agent skips are not compliant.\n\n"
        "Contributions to this repository must be submitted via 'git push no-mistakes'.\n"
        "See CONTRIBUTING.md for setup and the full workflow.\n\n"
        f"PR author: {facts.author}\n"
    )


def main() -> int:
    facts = Facts()

    # Exemption is evaluated even when the live lookup below failed: it never
    # certifies compliance (it always emits compliant=false, see the comment
    # below) and its inputs (author, head_ref) come from configured caller
    # policy plus author/branch facts that do not change across a rerun the
    # way body/head_sha can, so it carries none of the staleness risk the
    # live-lookup gate exists to close.
    reason = exemption_reason(facts)
    if reason:
        print(f"Skipping no-mistakes enforcement: {reason}.")
        emit_output("exempt", "true")
        emit_output("exempt-reason", reason)
        # Exemption is an explicit caller policy, not evidence that the PR ran
        # and satisfied the pipeline. Keep the successful bypass distinct from
        # a validated compliant verdict for downstream consumers.
        emit_output("compliant", "false")
        return 0
    emit_output("exempt", "false")

    if facts.live_lookup_attempted and not facts.live_lookup_used:
        # No explicit pr-body/pr-head-sha was forwarded (the documented
        # zero-input integration), so this run needed the live lookup to
        # know the PR's current body/head - and it failed. Falling back to
        # the workflow's own cached event payload here would be exactly the
        # hole this whole change exists to close: a GitHub Actions job RERUN
        # replays the payload archived at the run's ORIGINAL trigger, not a
        # live event, so evaluating compliance against it can certify a
        # stale, already-superseded verdict instead of the PR's current
        # state (see the module docstring). Fail closed instead of silently
        # downgrading to that stale source.
        fail(
            "::error::Could not verify this PR's live body/head via the GitHub API, so "
            "require-no-mistakes cannot safely evaluate compliance: falling back to the "
            "workflow's own cached event payload risks certifying a stale, "
            "already-superseded verdict if this job was rerun (see verify.py's module "
            "docstring).\n\n"
            "Add `pull-requests: read` to this workflow's `permissions:` so "
            "require-no-mistakes can verify against live PR state, or forward explicit "
            "`pr-body`/`pr-head-sha` inputs to bypass the live lookup entirely.\n\n"
            f"PR author: {facts.author}\n"
        )

    check_signature(facts)
    label = f"PR #{facts.number}" if facts.number else "PR"
    print(f"Found no-mistakes signature in {label} body.")

    payload = parse_attestation(facts)
    check_head_bind(facts, payload["head_sha"])
    check_required_steps(facts, payload["steps"])

    print("Found structurally compliant pipeline step attestation.")
    print(
        "::warning::PR-body attestation is author-editable and is not cryptographic proof "
        "that no-mistakes produced it."
    )
    emit_output("compliant", "true")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
