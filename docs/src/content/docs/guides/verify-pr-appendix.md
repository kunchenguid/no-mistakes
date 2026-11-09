---
title: Verify a PR appendix
description: Check the live PR appendix before starting another validation run.
---

Run this explicit read-only check to see whether publication accepts a PR's owned appendix:

```sh
no-mistakes axi pr-verify --repo https://github.com/owner/repo.git --pr 123
```

Supply the destination repository's full remote URL and the PR number. An scp-style remote such as `git@github.com:owner/repo.git` is also accepted. The command uses the configured provider adapter and machine-local forge profile. It requires authentication and a numbered PR read that reports the destination repository's identity, but does not require an initialized gate or a run. Missing or mismatched repository identity returns an error without a verdict.

The TOON result contains `pr`, `verdict`, and `reason`. Inspect `verdict` to decide whether to start another run:

| Verdict | Meaning |
| --- | --- |
| `match` | Publication accepts the appendix, including its digest and attestation shape. |
| `mismatch` | The appendix digest does not match, including after existing legacy caret recovery. |
| `malformed` | Ownership markers, Markdown fences, or attestation structure prevent publication from accepting the appendix. |
| `absent` | No owned appendix exists. An unmarked legacy attestation also returns `absent`, with publication's refusal reason. |

All four verdicts exit 0 because the read and verification completed. Invalid arguments exit 2. Configuration, authentication, unsupported-provider, and read failures exit 1 without a verdict. A `match` is an accidental-edit check, not authentication or a check that the attestation head matches the current PR head. An `absent` result does not establish that a future publication will succeed.

The command reads the live body once and runs the same parser used by publication. It never re-signs or edits the PR, starts a daemon, opens the local run database, or changes local refs. A later edit can change the result, so publication still verifies the body when it writes.

Legacy appendices stamped over control bytes remain accepted when the parser can recover the recorded digest from `gh`'s caret rewriting. It tries every subset of up to 12 caret pairs. Above that limit it tries restoring every pair. No re-sign operation is needed for the accepted legacy cases.
