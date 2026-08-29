---
title: Launch Proof Fork Maintenance
description: Temporary release and retirement procedure for strict AXI launch receipts.
---

Strict AXI launch receipts are upstream-first. Use a fork binary only while no
upstream release exposes the `axi run --launch-nonce` receipt contract.

## Publish a temporary fork build

1. Rebase the isolated proof commit series onto the current upstream `main`.
   Do not carry unrelated custody, routing, or release changes.
2. Build from an immutable fork commit and publish a public GitHub release whose
   tag and release notes record that full commit SHA.
3. Configure the consuming automation with that exact release asset URL and
   commit SHA. Never point an updater or binary source at a mutable branch,
   `latest`, or a moving release tag.
4. Run the strict-mode smoke test against the installed fork binary: invoke
   `no-mistakes axi run --intent <exact-intent> --launch-nonce <fresh-nonce>`
   on a committed feature branch, and verify a pre-drive `launch_receipt` has
   `created`, the full branch/head bindings, and the SHA-256 digest of the exact
   persisted intent. Reinvoke the same request and verify the same run ID with
   `reused`.

The nonce and intent digest are safe correlation material; do not add raw intent
to fork release notes, telemetry, status output, or update configuration.

## Retire after upstream ships

Do not infer support from an upstream version number. Install the candidate
upstream release, inspect `no-mistakes axi run --help` for `--launch-nonce`, and
run the same smoke test above against the upstream binary. Only after that smoke
test passes:

1. Remove the fork binary source/update override and switch consumers to the
   verified upstream release.
2. Delete the temporary fork release and proof branch.
3. Remove this temporary maintenance path in the next upstream documentation
   update; no compatibility alias or permanent fork-only command remains.
