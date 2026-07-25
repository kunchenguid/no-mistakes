# Run-start repository URL refresh evidence

Focused end-to-end exercise through `RunManager.startRun`:

```text
successful refresh
status=completed
persisted_upstream="https://example.com/owner/project.git"
clone_origin="https://example.com/owner/project.git"
gate_origin="git@example.com:owner/project.git"
clone_and_gate_remotes_unchanged=true

missing origin
status=completed
prior_registration_preserved=true
warning="repository URL refresh skipped; continuing with existing registration"
reason=remote_unreadable

malformed or credential-bearing origin
status=completed
prior_registration_preserved=true
warning="repository URL refresh skipped; continuing with existing registration"
reason=invalid_remote

ambiguous fork remotes
status=completed
prior_registration_preserved=true
warning="repository URL refresh skipped; continuing with existing registration"
reason=ambiguous_remote

database write failure
status=completed
prior_registration_preserved=true
warning="repository URL refresh skipped; continuing with existing registration"
reason=database_write
```

The assertions also reject warnings containing credentials, remote URLs, Git URL
fragments, or the working-copy path.

Command:

```sh
go test ./internal/daemon -run 'TestRunStart(RefreshesCloneURLWithoutMutatingRemotes|URLRefreshFailuresWarnSafelyAndContinueWithOldRegistration)$' -count=1 -v
```
