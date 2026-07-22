# macOS release signing verification

The release workflow contract tests passed for both macOS architectures, secret scoping, fail-closed verification, sign-before-archive ordering, cleanup, artifact compatibility, and the deliberately limited Phase 1 scope.

```text
$ go test . -run '^TestReleaseWorkflow' -count=1 -v
=== RUN   TestReleaseWorkflowSignsDarwinArtifactsWithDeveloperID
--- PASS: TestReleaseWorkflowSignsDarwinArtifactsWithDeveloperID
=== RUN   TestReleaseWorkflowSignsBothDarwinArches
--- PASS: TestReleaseWorkflowSignsBothDarwinArches
=== RUN   TestReleaseWorkflowScopesSigningSecretsToDarwin
--- PASS: TestReleaseWorkflowScopesSigningSecretsToDarwin
=== RUN   TestReleaseWorkflowSignsBeforeArchiveAndChecksum
--- PASS: TestReleaseWorkflowSignsBeforeArchiveAndChecksum
=== RUN   TestReleaseWorkflowFailsClosedOnBadSignature
--- PASS: TestReleaseWorkflowFailsClosedOnBadSignature
=== RUN   TestReleaseWorkflowCleansUpKeychainAlways
--- PASS: TestReleaseWorkflowCleansUpKeychainAlways
=== RUN   TestReleaseWorkflowPreservesArtifactContract
--- PASS: TestReleaseWorkflowPreservesArtifactContract
=== RUN   TestReleaseWorkflowStaysPhase1NoNotarization
--- PASS: TestReleaseWorkflowStaysPhase1NoNotarization
PASS
```

A release-shaped local build and archive exercise preserved the updater-facing filenames and confirmed that each tarball contains a thin binary for the intended architecture.

```text
artifact=no-mistakes-vTEST-darwin-amd64.tar.gz
no-mistakes
no-mistakes: Mach-O 64-bit executable x86_64

artifact=no-mistakes-vTEST-darwin-arm64.tar.gz
no-mistakes
no-mistakes: Mach-O 64-bit executable arm64
```

The same exercise reproduced the unsafe inputs that motivated the change.
The amd64 binary had no signature metadata, while the Go-produced arm64 binary was ad-hoc with no stable identifier or Team ID:

```text
Identifier=a.out
Signature=adhoc
TeamIdentifier=not set
```

The changed workflow places Developer ID signing and strict verification before tarball creation, so neither reproduced input can reach the archive/upload step.
An actual Developer ID signature was not produced locally because `CSC_LINK` is deliberately not configured for this PR and publishing a release is explicitly out of scope.
