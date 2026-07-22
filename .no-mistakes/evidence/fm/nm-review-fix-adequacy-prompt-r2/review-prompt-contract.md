# Durable-fix review prompt evidence

This is the agent-facing `ReviewStep` task contract exercised at target commit
`df5e8171f364a02ee1d1211e43db3b2191a400c5`.

## Durable-fix evidence requirement

> Determine from the stated intent and relevant evidence whether a bug-fix change claims a durable fix or explicitly authorized short-term containment.
>
> For a claimed durable fix, reconstruct the concrete failing sequence and required invariant, inspect relevant sibling paths and shared state transitions, and ask whether the same authorized failure remains reachable.
>
> When source evidence proves the failure remains reachable, report the concrete path and recommend the earliest supported shared boundary that would make the invariant hold, rather than duplicating another symptom patch.

This directly requires the failing sequence, invariant, sibling paths, shared state
transitions, a reachable remaining failure, and the earliest supported shared
boundary before escalating a claimed durable fix.

## Scope and architecture guardrails

> Do not infer a systemic flaw from code shape, duplication, or architectural preference alone. Do not demand a shared abstraction or broad redesign without a concrete reachable path, violated invariant, or immediately competing semantic owner.
>
> Do not block explicitly authorized honest containment merely because a later durable fix is possible. Do not expand user scope or turn optional broader improvements into blockers.

The surrounding prompt still carries the pre-existing controls for relevant
history and diff inspection, surrounding code, severity, action classification,
intent conformance, review scope, and fresh execution and round history.

## Focused runtime checks

Command:

```text
go test ./internal/pipeline/steps -run '^(TestReviewStep_DurableFixAdequacyContract|TestReviewStep_FixMode|TestReviewStep_ConformanceObligationTracksIntentProvenance|TestReviewStep_RereviewFlagsIntentContradictionAsAskUser|TestReviewStep_RoundHistorySanitizesAgentInput|TestReviewStep_DropsDeferredPipelineOwnedPRFinding)$' -count=1 -v
```

Observed result:

```text
PASS
ok github.com/kunchenguid/no-mistakes/internal/pipeline/steps
```

The selected tests execute `ReviewStep` with its agent boundary mocked and
inspect the prompt actually handed to the reviewer. They cover the new durable
fix contract, a normal initial review, a fixer plus full rereview, authoritative
versus inferred intent, ask-user parking for intent contradiction, and sanitized
round history.

## Prohibited-scope check

The target diff changes only:

```text
internal/pipeline/steps/review.go
internal/pipeline/steps/review_test.go
```

It creates no revieweval command or package, evaluation corpus, candidates,
benchmark results, or benchmark documentation.
