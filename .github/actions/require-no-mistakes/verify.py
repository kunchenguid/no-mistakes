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

        self.body = os.environ.get("PR_BODY") or _payload_str(payload, "body")
        self.head_sha = env("PR_HEAD_SHA") or _payload_str(head, "sha").strip()
        self.head_ref = env("PR_HEAD_REF") or _payload_str(head, "ref").strip()
        self.author = env("PR_AUTHOR") or _payload_str(user, "login").strip()
        number = env("PR_NUMBER")
        if not number:
            raw_number = payload.get("number")
            number = str(raw_number) if isinstance(raw_number, int) else ""
        self.number = number


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
