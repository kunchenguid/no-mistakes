---
title: Automatic repair
description: How routed repair resolves findings through escalation, deterministic checks, and independent verification.
---

When a pipeline step finds issues, `no-mistakes` can repair them automatically before pausing for your approval.
Repair is governed by the routing contract, not by user-configurable attempt limits.
There is no `auto_fix` configuration key: a finding escalates through a fixed quality cascade and fails closed when the cascade is exhausted.
Hosted-CI repair has an internal finite budget, but users cannot configure a numeric attempt count.

Two roles never mix.
A fixer produces a patch.
A separate, fresh verifier judges that patch.
No model's own claim of success can resolve a blocking finding, and no unverified patch can reach the push target.

The exact profile and route tables live in the [routing reference](/no-mistakes/reference/routing/#default-routes).

## Findings: severity and action

Every finding carries a severity and an action.

Severity says how much a finding matters:

- `error` and `warning` are blocking - they gate the pipeline until resolved
- `info` is informational and never blocks

Action says who may act on it:

- `auto-fix` - an objective issue that can be repaired without questioning the author's intent
- `ask-user` - an intent-sensitive or ambiguous issue that waits for explicit consent before any fixer runs
- `no-op` - a note that needs no action; a `no-op` finding never enters a repair cascade, even when its ID is selected for a fix

## Root finding lineages

Every finding that enters repair gets a root lineage: a durable, run-wide identity that survives re-reviews, restarts, and model rewording.

- an initial review finding's lineage is recorded against the exact routed model attempt that produced it, before any repair or approval
- a lineage is independent of the model's display ID and of the finding prose, so a finding cannot disappear through rewording or fuzzy ID matching
- findings from other steps get run-local root lineages, because a deterministic command failure has no producing model attempt
- each repair round persists the lineage's immutable finding content, action, severity, current tier, remaining budget, and links to the fixer attempt, the deterministic checks, and the verifier attempt

This history is what `no-mistakes axi status` and the TUI reconstruct after a restart, without parsing logs.

## The blocking cascade

Blocking `auto-fix` findings escalate through the structured repair route: `fix_fast` → `fix_balanced` → `authority_strong`.
Each tier runs the same shape:

1. One fresh fixer receives the whole batch of unresolved same-tier lineages, with the diff, the lineage details, and the remaining budget.
2. The fixer's changes are committed with a one-line summary.
3. The step's applicable deterministic checks run, and their command, exit code, and output are recorded on every lineage in the batch.
4. If an applicable check fails, the whole batch advances one tier immediately - no verifier invocation is spent on a patch a command already rejected.
5. Otherwise one separate, fresh strong verifier adjudicates every lineage in the batch by ID.

The verifier's adjudication is strict:

- only an explicit `resolved` verdict with a rationale resolves a lineage
- a missing, duplicate, or unknown lineage ID makes the entire verdict inconclusive, because a partial answer could silently approve a blocking finding
- malformed output, an `unresolved` verdict, an `inconclusive` verdict, or silence advances the lineage instead of resolving it

At the final tier the verifier is `authority_strong`.
A `fix_fast` or `fix_balanced` tier is verified by a fresh `review_strong` invocation.
An `authority_strong` fixer can succeed only after a different, fresh `authority_strong` (xhigh) invocation adjudicates its work.

## Batching and new findings

- all unresolved lineages at the lowest active tier are fixed together in one batch; resolved or differently tiered lineages do not rerun
- a shared deterministic check failure cannot be attributed to one lineage, so it conservatively advances every lineage in the batch
- a verifier finding caused by the patch inherits its root lineage's next tier and remaining ceiling, rather than receiving a fresh budget
- a verifier finding unrelated to the batch creates a separate new root lineage and is tracked independently
- a verifier finding that requires consent (`ask-user`) stops that lineage until a human or standing consent decides

## Fail-closed exhaustion

When a lineage exhausts its cascade without a clean verdict, it is persisted as unresolved and the gate fails closed.

- the step pauses for a human decision instead of completing
- approval is refused while any blocking lineage on the run remains unresolved
- under unattended consent, an exhausted or inconclusive blocking lineage aborts the run instead of being approved or retried

## Informational repair

Informational review findings - `info` severity with action `auto-fix` - take a cheap two-tier cascade: `fix_fast` → `tools_balanced`, with a `tools_balanced` verifier.

- it never invokes a `review_strong` or `authority_strong` profile
- it never blocks the gate
- an unresolved informational finding stays visible on the run instead of gating it

## Consent for intent-sensitive findings

An `ask-user` finding starts no fixer before consent.

- explicit consent is a fix action with the finding's ID selected, from the TUI or `no-mistakes axi respond --action fix`
- unattended consent is TUI yolo mode or AXI `--yes`, which the user grants as standing consent for the run
- a consented repair starts at `fix_balanced` and may escalate to `authority_strong`; it never uses the fast tier
- a consented repair must durably resolve every selected finding, or the run fails instead of quietly continuing

Unattended consent can never waive final authority.
Yolo and `--yes` fix each gate once with every finding selected, then approve the resulting fix review.
If a blocking lineage remains unresolved or inconclusive after its cascade, they abort the run rather than approve it.

## Provider failover is not escalation

Escalation changes the quality tier.
Failover changes the provider inside the same tier.

- within a profile, candidates are tried in provider-preference order, and a classified operational failure opens that provider's circuit for the rest of the run
- when every candidate in a required profile is unavailable, the invocation fails closed and the gate fails, rather than borrowing a weaker or stronger profile
- provider failover never advances the quality tier

See [provider circuits](/no-mistakes/reference/routing/#provider-circuits) for the full rules.

## Deterministic command failures

A configured `commands.test` or `commands.lint` failure pauses the step with the command output as a blocking finding.
When a fix is authorized, a fresh routed fixer repairs the failure and the step re-runs the exact configured command as the primary gate.
Test repair starts at `fix_balanced` because a failing test log is unstructured evidence; the fast tier is never used merely to infer scope.
The same principle covers rebase conflicts and CI failures: see the [pipeline steps reference](/no-mistakes/reference/pipeline-steps/) for each step's repair behavior.

## Combined housekeeping findings

When `commands.lint` is empty, the Document authoring pass also performs the agent-driven lint duty.
It applies safe lint and format fixes, then categorizes every unresolved finding as `documentation` or `lint`.

The two categories keep separate owners:

- a fresh documentation-only verifier produces the findings that gate Document
- the combined pass hands lint findings to Lint once, where errors and warnings pause for approval
- an uncategorized combined finding stays with Document, which is the stricter fail-safe gate

Combined lint findings do not enter the routed repair coordinator automatically because the authoring pass already attempted safe fixes.
If you authorize a lint fix, Lint runs a fresh standalone pass instead of trusting the consumed result.

Malformed combined author output fails Document and invalidates any earlier lint result.
A malformed result already handed to Lint fails Lint as inconclusive.
When there is no trustworthy result to hand over, including after a skipped Document pass or a process boundary, Lint runs its own agent pass so the lint duty is not lost.

## User-triggered fixes

When the pipeline pauses for approval, you can trigger a fix yourself from the TUI or the AXI interface:

1. The findings panel shows all findings with checkboxes.
2. Toggle individual findings with `space`, or use `A` (all) and `N` (none).
3. Optionally press `e` to attach a note to the current finding, or `+` to add your own finding to the fix request.
4. Press `f` to fix the selected findings.

The fixer receives the merged payload for that round: the selected findings, any per-finding user notes, any selected user-authored findings, and a sanitized history of previous rounds for that step.
That history includes which finding IDs were selected before, which findings you left unselected, and one-line summaries from earlier fix commits.
Follow-up review passes use that history to avoid re-reporting user-ignored findings unless the code now has a materially different problem.

After a user-triggered fix, the step re-runs and pauses again to show the results (`fix_review` status).
You can then approve, fix again, skip, or abort.

## Fix commits

Every repair commit carries a step-scoped message so the branch history explains itself:

| Source | Commit message |
|---|---|
| Rebase conflict resolution | the branch's own commits, completed through `git rebase --continue` |
| Review repair | `no-mistakes(review): <summary>` |
| Test repair | `no-mistakes(test): <summary>` |
| Document authoring | `no-mistakes(document): <summary>` |
| Lint and formatting | `no-mistakes(lint): <summary>` |
| Verify repair | `no-mistakes(verify): <summary>` |
| CI repair | `no-mistakes: apply CI fixes` |

Push commits nothing.
It transports the sealed candidate exactly as verified.

## Step rounds

Each execution of a step is recorded as a round in the database.
A round stores its findings, duration, any selected finding IDs and whether the selection came from the user or automatic filtering, the merged finding payload actually sent to the fixer, and the one-line fix summary.

Round trigger types:

- `initial` - the step's first execution
- `auto_fix` - a repair-cascade round, or a fix you trigger with `f` in the TUI or `no-mistakes axi respond --action fix`
- `repair_exhausted` - the terminal record written when a cascade fails closed with unresolved lineages

Legacy `user_fix` rounds from older versions are still rendered as `auto-fix` in PR summaries.
The PR body's risk assessment, testing, and pipeline sections are built from these rounds, so reviewers can see what was found, what was fixed, and how it was verified.
