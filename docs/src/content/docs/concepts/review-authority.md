---
title: Review Authority Policy
description: When the gate may act on its own, when it must escalate, and how to measure the outcome.
---

This policy defines **how much authority a no-mistakes gate may exercise on
its own** — which findings it may auto-fix, which must pause for a human, and
how to measure whether the authority granted is earning its keep.

It is the policy layer over the mechanics documented in
[auto-fix](/no-mistakes/concepts/auto-fix/) and the
[gate model](/no-mistakes/concepts/gate-model/). The mechanics are
authority-agnostic: `auto_fix` limits, finding `action` classes, yolo consent.
This page decides what a given repository *should* configure, and what the
gate must never do regardless of configuration.

The model is adapted from Intercom's
[AI PR-approval operating system](https://www.intercom.com/blog/ai-is-approving-our-pull-requests-heres-how-we-made-it-safe/)
(19.2% of PRs auto-approved with zero reverts in pilot, ~10x lower revert
rate on AI-authored code), generalized to this gate's vocabulary.

## The authority bands

Every gate run operates in one of four bands. The band is a *repository
policy choice*, expressed through `.no-mistakes.yaml` and the repo's own
risk posture — never something a single run can escalate on its own.

| Band | What the gate may do | Mechanism |
|---|---|---|
| **L0 — Advisory** | Review, comment, report. Every finding parks for a decision; nothing is fixed or pushed without a human action. | Every `auto_fix` step set to zero. This is an opt-in posture, not the shipped default. |
| **L1 — Human-clicks** | The gate may fix findings, but the merge decision stays with a human; `ask-user` findings always pause; yolo is explicit per-run consent, not standing authority. | `auto_fix.review` > 0 only for findings classed `auto-fix`; `ask-user` always pauses; yolo documented as consent, never default. |
| **L2 — Conditional auto** | Within hard caps (change size, scope, finding class), the gate may fix, re-verify, and push unattended; anything above a cap or outside the class pauses. | Per-step `auto_fix` limits; review step auto-fixes only within class. Size and scope caps are not implemented yet — see [Known gaps](#known-gaps-deferred). |
| **L3 — Scoped full-auto** | Full autonomy inside a bounded, measured, reversible domain (e.g. generated docs, dependency bumps, lint normalization). | Explicit repo policy declaring the domain; still never for irreversible actions. |

**The shipped default sits between L0 and L1.** Out of the box `auto_fix.review`
is `0`, so every review finding parks for a decision, while the Test, CI,
Rebase, and — when `commands.lint` is configured — Lint steps each auto-fix
within their attempt limits. The Document step applies its fixes during its own
pass rather than through a follow-up loop, and unresolved documentation findings
pause for approval. A repository that wants strict L0 must set the looping steps
to zero explicitly; the
[`auto_fix` field reference](/no-mistakes/reference/global-config/#auto_fix)
owns the per-step defaults and which steps the limit applies to.

**Never in any band:** data deletion, credential changes, publishing
artifacts, or force-pushing over unincorporated upstream commits. The remote
data-loss guard is the floor; this policy extends the same logic to
non-reversible actions.

## Non-negotiables

1. **The merge decision stays human.** The gate's `checks-passed` outcome
   instructs agents to stop and ask for review and merge. Keep it that way.
   Automation may *prepare* a merge; it never *performs* one.
2. **Unclassified findings fail closed.** A finding without an `action` is
   treated as `ask-user`. Preserve this invariant in every step that emits
   findings, and treat any change that would make the default permissive as a
   security regression.
3. **`ask-user` is not auto-fixable.** Intent-sensitive findings exist
   precisely because judgment is required. No `auto_fix` limit may convert an
   `ask-user` finding into an unattended fix.
4. **Escalation is frictionless.** A human can pause, respond to, or abort
   any run at any point (`no-mistakes axi respond` / TUI / `axi abort`).
   Override must never require blaming the requester.
5. **Evidence parity.** A run's record — findings, fix commits, step logs,
   PR link, intent — must be reconstructable for an auditor without any
   human in the loop having been the AI. The gate's SQLite state plus the PR
   trail is the audit surface; keep it complete.
6. **Shrink before you investigate.** Any revert or incident attributable to
   a change the gate auto-approved or auto-fixed drops the repository's band
   by one immediately. Investigate after shrinking, never before.

## The measurement loop

Authority is granted on evidence and shrinks on evidence. For each
repository running above L0, track:

- **Revert rate of gated PRs** (the outcome currency — same class of metric
  as Intercom's
  [0.53% vs 5.39%](https://www.intercom.com/blog/the-safety-of-speed-shipping-code-at-intercom/))
- **Finding classes by step** (auto-fix / ask-user / no-op counts; rising
  ask-user rate in a step = the step's guidance is degrading or the work is
  getting harder)
- **Auto-fix success rate** (fixes that survive re-verification vs fixes
  that re-fail)
- **Parked duration** (how long runs wait on human decisions; growing
  duration = the gate is becoming the bottleneck again)
- **Incidents touching gated branches** (the hardening signal)

Expansion rules: raise a band only after a measured interval with the
current band's adverse signals at or below baseline. There is no time-based
promotion; there is only evidence-based promotion.

## Per-repo risk tiers

There is no dedicated band key today. A band is expressed through the per-step
`auto_fix` limits in `.no-mistakes.yaml` plus the repository's own written
policy. Suggested posture:

| Repo class | Band | Notes |
|---|---|---|
| Docs, generated files, dependency bumps | L2 | Small diffs, reversible, CI-verified |
| Application code, library code | L1 | Auto-fix within class; ask-user pauses |
| Security-sensitive, data-critical, public-facing | L0–L1 | Any change touching auth, payments, or data paths escalates regardless of band |

## Known gaps (deferred)

- No dedicated band key. Declaring `L2` in `.no-mistakes.yaml` is not
  possible today; a repository approximates its band with per-step `auto_fix`
  limits and records the intent outside the config.
- No size or scope caps. A diff beyond a declared threshold should pause for a
  human even inside L2, and that cap must be a gate rule enforced where the
  decision is made rather than a prompt preference — but no such threshold
  exists in the config schema or the review step yet.
- No built-in outcome telemetry (revert/incident tracking per gated PR).
  Until it exists, the measurement loop is a manual or external process; the
  policy should be adopted in repos that can run it.
- Review-lens decomposition (specialist sub-reviews: intent alignment,
  safety, logic, best practices) is prompt/agent territory today, not a gate
  mechanism. The policy treats it as a future review-step capability.
- A helpfulness flywheel (engineers flag whether review comments were
  useful; the feedback sharpens review guidance) needs a feedback capture
  surface that does not exist yet.
