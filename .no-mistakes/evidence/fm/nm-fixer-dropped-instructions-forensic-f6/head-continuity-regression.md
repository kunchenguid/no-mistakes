# HEAD continuity incident reproduction

The focused race-enabled regression exercises the user-visible failure boundary at the pipeline commit operation.
It first commits the reviewed fix, moves the live worktree HEAD to a divergent sibling commit that omits that fix, and then attempts the document-step commit.
The operation refuses the divergent history, leaves the live HEAD at the clobber commit without layering a document commit on it, and preserves the recorded reviewed-head anchor.

Command:

```text
go test -race -v ./internal/pipeline/steps -run 'TestCommitAgentFixes_(RefusesToCommitOnOutOfBandResetHead|RefusesOnBackwardReset|RefusesResetDuringCommit|AllowsForwardAgentCommit)|TestAssertPipelineHeadContinuity_AnchorIsRecordedReviewedHead'
```

Observed output:

```text
=== RUN   TestCommitAgentFixes_RefusesToCommitOnOutOfBandResetHead
    headcontinuity_repro_test.go:91: guard refused divergent clobber: reviewed fix at 18ab4d2b protected
--- PASS: TestCommitAgentFixes_RefusesToCommitOnOutOfBandResetHead (0.31s)
=== RUN   TestCommitAgentFixes_RefusesOnBackwardReset
--- PASS: TestCommitAgentFixes_RefusesOnBackwardReset (0.16s)
=== RUN   TestCommitAgentFixes_RefusesResetDuringCommit
--- PASS: TestCommitAgentFixes_RefusesResetDuringCommit (0.41s)
=== RUN   TestCommitAgentFixes_AllowsForwardAgentCommit
--- PASS: TestCommitAgentFixes_AllowsForwardAgentCommit (0.19s)
=== RUN   TestAssertPipelineHeadContinuity_AnchorIsRecordedReviewedHead
--- PASS: TestAssertPipelineHeadContinuity_AnchorIsRecordedReviewedHead (0.12s)
PASS
ok  github.com/kunchenguid/no-mistakes/internal/pipeline/steps  2.823s
```

This also demonstrates that backward resets and resets racing the commit are refused, while a legitimate forward agent commit remains accepted.
The anchor-specific check confirms that restoring the live worktree to the recorded reviewed SHA makes the same guard pass.
