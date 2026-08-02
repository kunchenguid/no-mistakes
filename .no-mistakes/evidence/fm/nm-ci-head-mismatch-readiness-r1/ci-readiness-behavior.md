# CI readiness behavior evidence

Targeted on 2026-08-02 against commit `d1fabef6b1a79eb33e63732a14dd7152cfb652dd`.

## Incident journey

The executable CI-step harness replayed firstmate PR 1495: passing jobs alongside `Behavior portable serial`, first `IN_PROGRESS` and then terminal `CANCELLED`, using the shipped rerun budget of zero. The step returned an approval outcome before its bounded polling guard fired. Its observable contract verified:

- the finding names `Behavior portable serial` and has action `ask-user`;
- the run was not marked CI-ready;
- no rerun command was issued;
- no fix-agent round was invoked;
- no "all CI checks passed" message was emitted.

Focused test: `TestCIStep_CancelledCheckAmongPassingChecksEscalatesInsteadOfPollingForever`.

## Advanced-head journey

The harness created and pushed a new fix commit after the run record's `head_sha`, then supplied a live PR rollup whose build and test checks were green. The monitor emitted `all CI checks passed - still monitoring until merged or closed` and persisted `CIReadyAt`, proving that a green rollup at the current pushed head is accepted while the run row still names the older commit.

Focused test: `TestCIStep_GreenChecksAtAdvancedHeadAreRecognizedWhileRunTracksOlderHead`.

## Reviewer-visible monitor transcript

The adjacent rerun journey produced this actual step log and command trace:

```text
monitoring CI for PR #42 (timeout: 30m0s)...
re-running CI check test (1/1): provider reported cancelled, not a job failure
CI checks running, waiting for results...
all CI checks passed - still monitoring until merged or closed

gh auth status
gh pr view 42 --repo var/folders --json state --jq .state
gh pr view 42 --repo var/folders --json mergeable --jq .mergeable
gh pr checks 42 --repo var/folders --json name,state,bucket,completedAt,link
gh run rerun --job 901 --repo var/folders

fix-agent rounds consumed: 0
```

The terminal-cancellation-after-rerun journey produced:

```text
outcome: needs_approval=true
summary="CI checks were cancelled without reporting a verdict"
finding="CI check cancelled again after its rerun: test - the provider cancelled it rather than reporting a job failure, so it needs a decision rather than a code fix"
rerun requests: 1
ci ready: not set
```

The moved-published-head safeguard produced:

```text
published branch head moved (expected 18bb7ffc2628, observed 89cb3b01780e); not re-running checks
outcome: needs_approval=true
rerun requests: 0
```

## Commands

```text
go test ./internal/pipeline/steps -run 'TestCIStep_(CancelledCheckAmongPassingChecksEscalatesInsteadOfPollingForever|ZeroRerunBudgetEscalatesCancelledCheckWithoutMakingItReady|GreenChecksAtAdvancedHeadAreRecognizedWhileRunTracksOlderHead|BitbucketStoppedCheckParksForADecision)$' -count=1 -v

go test ./internal/pipeline/steps -run 'TestCIStep_(UncertainProviderStateClearsPersistedReadiness|DelayedCheckRegistrationStaysNotReadyUntilGreen|CancelledCheckIsRerunBeforeEscalating|LaggingRerunRollupKeepsWaitingForTheRepublishedCheck|CancelledCheckStaysUnresolvedAfterItsBudget|UnresolvedCancelledCheckNeverEntersTheAutoFixLoop|MovedPublishedHeadTerminatesInsteadOfRerunning)$' -count=1 -v
```

Both commands passed. The Bitbucket `STOPPED` journey also parked for an `ask-user` decision without a fix-agent round, confirming the behavior is based on the provider-generic cancel bucket.
