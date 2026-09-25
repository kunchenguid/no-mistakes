---
name: release-signing
description: Use when changing macOS release signing, release artifact verification, the release workflow, or the self-update channel manifest.
user-invocable: false
metadata:
  internal: true
---

**macOS Release Signing (permanent identity)**

- Every official macOS release artifact - both `darwin/arm64` and `darwin/amd64` - is Developer ID Application signed on a macOS runner with a fixed identifier, hardened runtime, secure timestamp, and no entitlements, then strictly verified before it is archived or checksummed; the Linux and Windows release paths are unchanged.
- The executable identifier `com.kunchenguid.no-mistakes` and Team ID `9T2J7MNUP9` are the permanent Developer ID identity and MUST NEVER change: they are the invariant of the identity-based designated requirement that lets macOS permission grants survive `no-mistakes update`, so changing either resets every grant once.
- Signing runs only in the darwin build job gated behind the `release-signing` GitHub environment; the certificate is the base64 `CSC_LINK` secret unlocked with `CSC_KEY_PASSWORD`, imported into an ephemeral keychain with a runtime-generated password that is deleted on success and failure, and no other job may reference those secrets.
- Signing happens before tarball creation and checksum generation, and the verify gate fails the release closed on any missing or ambiguous signature, wrong Team ID, non-permanent identifier, content-based (`cdhash`) requirement, missing hardened runtime or timestamp, or wrong architecture.
- Mechanics live in `.github/workflows/release.yml`; the contract is pinned by the root `TestReleaseWorkflow*` static tests in `workflow_release_signing_test.go`, and secret values are never recorded here or in any test fixture.
- Notarization, stapling, a PKG, Homebrew, and universal binaries are intentionally out of scope for this phase.

**Self-update channel manifest (`internal/update`)**

- `no-mistakes update` reads version metadata exclusively from `channels.json` on the GitHub release-asset CDN (`releases/download/channels/channels.json`), not `api.github.com`; a token is never required. Publisher: `cmd/publish-channels`, invoked from `.github/workflows/publish-channels.yml` (reusable `workflow_call`, plus `workflow_dispatch` / `on: release` backstops). `release.yml` calls it after `finalize` because GitHub does not cascade `GITHUB_TOKEN` `release` events, so release-please (pre)releases would otherwise leave the channel stale. Regressions: `internal/update/channels_test.go`, `workflow_publish_channels_test.go`, `TestReleaseWorkflowCallsPublishChannelsAfterFinalize`.
