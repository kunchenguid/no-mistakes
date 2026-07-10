//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCascadeDirectLunaSuccess proves a blocking finding fixed at the first
// (fix_fast / Luna) tier resolves without escalating: one repair cycle at tier
// 0, and the fixer launched the fix_fast Luna candidate.
func TestCascadeDirectLunaSuccess(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-luna-direct"
    text: "found a fixable bug"
    structured:
      findings:
        - id: "luna-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "direct luna bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    text: "fixed at fix_fast"
    edits:
      - path: "cascade.txt"
        new: "luna fix\n"
    structured:
      summary: "guarded the bug"
  - match: "Independently verify whether each of the following"
    text: "verified resolved at tier 0"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the bug is now guarded"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-luna-direct", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-luna-direct")
	run := waitForStepStatus(t, h, "cascade-luna-direct", types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

	repairs := h.FindingRepairs(t, run.ID)
	if len(repairs) != 1 {
		t.Fatalf("finding repairs = %d, want exactly 1 (resolved at fix_fast without escalation)", len(repairs))
	}
	if repairs[0].Tier != 0 || repairs[0].Status != db.RepairStatusResolved || repairs[0].Verdict != db.RepairVerdictResolved {
		t.Fatalf("repair = tier %d status %q verdict %q, want tier 0 resolved", repairs[0].Tier, repairs[0].Status, repairs[0].Verdict)
	}

	fixers := succeededAttemptsFor(h.InvocationAttempts(t, run.ID), types.PurposeStructuredFindingRepair)
	if len(fixers) != 1 {
		t.Fatalf("fixer attempts = %d, want 1 (no escalation)", len(fixers))
	}
	assertCandidate(t, fixers[0], "fix_fast", 0, "luna", types.EffortMedium)

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("cascade-luna-direct", 60*time.Second)
}

// TestCascadeLunaTerraSol proves a blocking finding the fix_fast and
// fix_balanced verifiers leave unresolved escalates the quality tier
// Luna->Terra->Sol, and only a separate authority_strong (xhigh) final reviewer
// — a distinct invocation from the Sol fixer — resolves it.
func TestCascadeLunaTerraSol(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-escalate"
    text: "found a stubborn bug"
    structured:
      findings:
        - id: "escalate-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "escalating bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    model: "luna"
    text: "fix_fast attempt"
    edits:
      - path: "cascade.txt"
        new: "luna fix\n"
    structured:
      summary: "fix_fast attempt"
  - match: "Fix the following"
    model: "terra"
    text: "fix_balanced attempt"
    edits:
      - path: "cascade.txt"
        new: "terra fix\n"
    structured:
      summary: "fix_balanced attempt"
  - match: "Fix the following"
    model: "sol"
    text: "authority attempt"
    edits:
      - path: "cascade.txt"
        new: "sol fix\n"
    structured:
      summary: "authority attempt"
  - match: "Independently verify whether each of the following"
    effort: "xhigh"
    text: "authority verified resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the authority reviewer confirms the fix"
      new_findings: []
  - match: "Independently verify whether each of the following"
    text: "still unresolved at this tier"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "unresolved"
          rationale: "not resolved at this tier"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-escalate", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-escalate")
	run := waitForStepStatus(t, h, "cascade-escalate", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	// The finding escalates through all three quality tiers under one lineage,
	// resolving only at the top.
	repairs := h.FindingRepairs(t, run.ID)
	byTier := map[int]*db.FindingRepair{}
	lineage := ""
	for _, r := range repairs {
		byTier[r.Tier] = r
		if lineage == "" {
			lineage = r.LineageID
		} else if r.LineageID != lineage {
			t.Fatalf("escalation split across lineages %q and %q; a cascade must stay one lineage", lineage, r.LineageID)
		}
	}
	for tier := 0; tier <= 2; tier++ {
		if byTier[tier] == nil {
			t.Fatalf("missing repair row for tier %d; repairs=%+v", tier, repairs)
		}
	}
	if byTier[0].Verdict != db.RepairVerdictUnresolved || byTier[1].Verdict != db.RepairVerdictUnresolved {
		t.Fatalf("tier 0/1 verdicts = %q/%q, want both unresolved (escalating)", byTier[0].Verdict, byTier[1].Verdict)
	}
	if byTier[2].Status != db.RepairStatusResolved || byTier[2].Verdict != db.RepairVerdictResolved {
		t.Fatalf("tier 2 = status %q verdict %q, want resolved", byTier[2].Status, byTier[2].Verdict)
	}

	attempts := h.InvocationAttempts(t, run.ID)
	fixers := succeededAttemptsFor(attempts, types.PurposeStructuredFindingRepair)
	if len(fixers) != 3 {
		t.Fatalf("fixer attempts = %d %v, want 3 (Luna->Terra->Sol)", len(fixers), candidateModels(fixers))
	}
	assertCandidate(t, fixers[0], "fix_fast", 0, "luna", types.EffortMedium)
	assertCandidate(t, fixers[1], "fix_balanced", 1, "terra", types.EffortMedium)
	assertCandidate(t, fixers[2], "authority_strong", 2, "sol", types.EffortXHigh)

	// Sub-max tiers use the review_strong verifier; the top tier is adjudicated
	// by a separate authority_strong xhigh final reviewer.
	normal := succeededAttemptsFor(attempts, types.PurposeNormalAggregateVerification)
	if len(normal) != 2 {
		t.Fatalf("review_strong verifier attempts = %d, want 2 (one per sub-max tier)", len(normal))
	}
	for _, v := range normal {
		assertCandidate(t, v, "review_strong", 0, "sol", types.EffortHigh)
	}
	final := succeededAttemptsFor(attempts, types.PurposeEscalatedAggregateVerification)
	if len(final) != 1 {
		t.Fatalf("authority_strong final reviewer attempts = %d, want exactly 1", len(final))
	}
	assertCandidate(t, final[0], "authority_strong", 0, "sol", types.EffortXHigh)
	if final[0].ID == fixers[2].ID {
		t.Fatalf("final reviewer must be a separate invocation from the Sol fixer, got same attempt %q", final[0].ID)
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("cascade-escalate", 60*time.Second)
}

// cascadeCleanCatchAll is the trailing clean action for journeys whose run
// proceeds past review; it satisfies every later step's schema.
const cascadeCleanCatchAll = `  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\ncanned"
`

// TestCascadeLunaTerra proves a finding the fix_fast verifier leaves unresolved
// escalates one tier to fix_balanced (Terra) and resolves there, without ever
// reaching authority_strong. The two review_strong verifiers are identical in
// model/effort, so the tier is distinguished by the diff the fixer produced.
func TestCascadeLunaTerra(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-luna-terra"
    text: "found a two-tier bug"
    structured:
      findings:
        - id: "twotier-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "two tier bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    model: "luna"
    text: "fix_fast attempt"
    edits:
      - path: "cascade.txt"
        new: "NEEDS_STRONGER_FIX\n"
    structured:
      summary: "fix_fast attempt"
  - match: "Fix the following"
    model: "terra"
    text: "fix_balanced attempt"
    edits:
      - path: "cascade.txt"
        new: "TERRA_RESOLVED_MARKER\n"
    structured:
      summary: "fix_balanced attempt"
  - match: "TERRA_RESOLVED_MARKER"
    text: "resolved after the stronger fix"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the stronger fix guards the bug"
      new_findings: []
  - match: "Independently verify whether each of the following"
    text: "still unresolved at fix_fast"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "unresolved"
          rationale: "the fix_fast attempt is insufficient"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-luna-terra", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-luna-terra")
	run := waitForStepStatus(t, h, "cascade-luna-terra", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	repairs := h.FindingRepairs(t, run.ID)
	byTier := map[int]*db.FindingRepair{}
	for _, r := range repairs {
		byTier[r.Tier] = r
	}
	if byTier[0] == nil || byTier[1] == nil {
		t.Fatalf("want repair rows at tiers 0 and 1; got %d rows", len(repairs))
	}
	if byTier[2] != nil {
		t.Fatalf("did not expect escalation to tier 2 (authority_strong); got %+v", byTier[2])
	}
	if byTier[0].Verdict != db.RepairVerdictUnresolved {
		t.Fatalf("tier 0 verdict = %q, want unresolved", byTier[0].Verdict)
	}
	if byTier[1].Status != db.RepairStatusResolved || byTier[1].Verdict != db.RepairVerdictResolved {
		t.Fatalf("tier 1 = status %q verdict %q, want resolved", byTier[1].Status, byTier[1].Verdict)
	}

	attempts := h.InvocationAttempts(t, run.ID)
	fixers := succeededAttemptsFor(attempts, types.PurposeStructuredFindingRepair)
	if len(fixers) != 2 {
		t.Fatalf("fixer attempts = %d %v, want 2 (Luna then Terra)", len(fixers), candidateModels(fixers))
	}
	assertCandidate(t, fixers[0], "fix_fast", 0, "luna", types.EffortMedium)
	assertCandidate(t, fixers[1], "fix_balanced", 1, "terra", types.EffortMedium)
	if final := succeededAttemptsFor(attempts, types.PurposeEscalatedAggregateVerification); len(final) != 0 {
		t.Fatalf("authority_strong final reviewer attempts = %d, want 0 (resolved before the top tier)", len(final))
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("cascade-luna-terra", 60*time.Second)
}

// TestCascadeSameTierBatching proves two blocking findings at the same tier are
// repaired in ONE batch: a single fixer invocation and a single verifier
// invocation at tier 0 cover both lineages, which resolve together.
func TestCascadeSameTierBatching(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-batch"
    text: "found two bugs"
    structured:
      findings:
        - id: "batch-alpha"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "batch bug alpha"
          action: auto-fix
        - id: "batch-beta"
          severity: error
          file: "cascade.txt"
          line: 2
          description: "batch bug beta"
          action: auto-fix
      risk_level: high
      risk_rationale: "two blocking bugs must be fixed"
  - match: "Fix the following"
    text: "fixed both in one batch"
    edits:
      - path: "cascade.txt"
        new: "batch fixed\n"
    structured:
      summary: "fixed the batch"
  - match: "Independently verify whether each of the following"
    text: "both resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID_0"
          status: "resolved"
          rationale: "alpha is guarded"
        - lineage_id: "PROMPT_LINEAGE_ID_1"
          status: "resolved"
          rationale: "beta is guarded"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-batch", "cascade.txt", "bug one\nbug two\n", "add batch target")
	h.PushToGate("cascade-batch")
	run := waitForStepStatus(t, h, "cascade-batch", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	repairs := h.FindingRepairs(t, run.ID)
	if len(repairs) != 2 {
		t.Fatalf("finding repairs = %d, want 2 (one per lineage, both at tier 0)", len(repairs))
	}
	lineages := map[string]bool{}
	for _, r := range repairs {
		lineages[r.LineageID] = true
		if r.Tier != 0 || r.Status != db.RepairStatusResolved {
			t.Fatalf("repair %+v: want tier 0 resolved", r)
		}
	}
	if len(lineages) != 2 {
		t.Fatalf("distinct lineages = %d, want 2", len(lineages))
	}
	// Same-tier batching: both lineages share one fixer attempt and one
	// verifier attempt (one batch), not one fixer each.
	if repairs[0].FixerAttemptID == "" || repairs[0].FixerAttemptID != repairs[1].FixerAttemptID {
		t.Fatalf("fixer attempts = %q / %q, want one shared batch fixer", repairs[0].FixerAttemptID, repairs[1].FixerAttemptID)
	}
	if repairs[0].VerifierAttemptID == "" || repairs[0].VerifierAttemptID != repairs[1].VerifierAttemptID {
		t.Fatalf("verifier attempts = %q / %q, want one shared batch verifier", repairs[0].VerifierAttemptID, repairs[1].VerifierAttemptID)
	}
	if fixers := succeededAttemptsFor(h.InvocationAttempts(t, run.ID), types.PurposeStructuredFindingRepair); len(fixers) != 1 {
		t.Fatalf("fixer invocations = %d, want 1 (the batch was fixed in a single invocation)", len(fixers))
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("cascade-batch", 60*time.Second)
}

// TestCascadePatchCausedInheritance proves a new blocking finding the verifier
// attributes to a fix patch reattaches to that root lineage and keeps escalating
// with the new content — inheriting the next tier and remaining budget rather
// than starting a fresh lineage with a full budget.
func TestCascadePatchCausedInheritance(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-patch-caused"
    text: "found a root bug"
    structured:
      findings:
        - id: "root-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "root bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    model: "luna"
    text: "fix_fast attempt introduces a regression"
    edits:
      - path: "cascade.txt"
        new: "ROUND0_FIX\n"
    structured:
      summary: "fix_fast attempt"
  - match: "Fix the following"
    model: "terra"
    text: "fix_balanced attempt fixes the regression"
    edits:
      - path: "cascade.txt"
        new: "ROUND1_FIX\n"
    structured:
      summary: "fix_balanced attempt"
  - match: "severity error: root bug"
    model: "sol"
    text: "root fixed but the patch caused a regression"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the root symptom is gone"
      new_findings:
        - description: "patch regression"
          severity: error
          action: auto-fix
          caused_by_lineage_id: "PROMPT_LINEAGE_ID"
  - match: "severity error: patch regression"
    model: "sol"
    text: "regression resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the regression is now fixed"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-patch-caused", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-patch-caused")
	run := waitForStepStatus(t, h, "cascade-patch-caused", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	repairs := h.FindingRepairs(t, run.ID)
	// One lineage only: the patch-caused finding reattached to the root rather
	// than spawning an unrelated root.
	lineages := map[string]bool{}
	byTier := map[int]*db.FindingRepair{}
	for _, r := range repairs {
		lineages[r.LineageID] = true
		byTier[r.Tier] = r
	}
	if len(lineages) != 1 {
		t.Fatalf("distinct lineages = %d, want 1 (patch-caused finding must inherit the root lineage)", len(lineages))
	}
	if byTier[0] == nil || byTier[1] == nil {
		t.Fatalf("want repair rows at tiers 0 and 1; got %d rows", len(repairs))
	}
	if byTier[0].Description != "root bug" {
		t.Fatalf("tier 0 description = %q, want the original root finding", byTier[0].Description)
	}
	if byTier[1].Description != "patch regression" {
		t.Fatalf("tier 1 description = %q, want the inherited patch-caused content", byTier[1].Description)
	}
	// Inherited the NEXT tier and the reduced remaining budget, not a fresh one.
	if byTier[1].RemainingBudget != 1 {
		t.Fatalf("tier 1 remaining budget = %d, want 1 (inherited, not a fresh full budget)", byTier[1].RemainingBudget)
	}
	if byTier[1].Status != db.RepairStatusResolved {
		t.Fatalf("tier 1 status = %q, want resolved", byTier[1].Status)
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("cascade-patch-caused", 60*time.Second)
}

// TestCascadeInformationalTermination proves an informational finding is
// repaired through the cheap fix_fast -> tools_balanced cascade, never reaches a
// Sol/Fable (authority_strong) profile, and never blocks the gate.
func TestCascadeInformationalTermination(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-info"
    text: "found an informational nit"
    structured:
      findings:
        - id: "info-1"
          severity: info
          file: "cascade.txt"
          line: 1
          description: "informational nit"
          action: auto-fix
      risk_level: low
      risk_rationale: "informational only"
  - match: "Fix the following"
    text: "addressed the nit"
    edits:
      - path: "cascade.txt"
        new: "nit addressed\n"
    structured:
      summary: "addressed the nit"
  - match: "Independently verify whether each of the following"
    text: "informational nit resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the nit is addressed"
      new_findings: []
`+cascadeCleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-info", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-info")
	run := h.WaitForRun("cascade-info", 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (an informational finding never blocks the gate)", run.Status)
	}

	attempts := h.InvocationAttempts(t, run.ID)
	infoFixers := succeededAttemptsFor(attempts, types.PurposeInformationalRepair)
	if len(infoFixers) == 0 {
		t.Fatalf("expected an informational repair fixer; models=%v", candidateModels(attempts))
	}
	assertCandidate(t, infoFixers[0], "fix_fast", 0, "luna", types.EffortMedium)
	for _, a := range attempts {
		if a.Start.Candidate.Profile == "authority_strong" {
			t.Fatalf("informational repair reached authority_strong (Sol/Fable): %+v", a.Start.Candidate)
		}
	}
	if esc := attemptsForPurpose(attempts, types.PurposeEscalatedAggregateVerification); len(esc) != 0 {
		t.Fatalf("informational path invoked the escalated authority verifier %d times, want 0", len(esc))
	}
}

// TestCascadeConsentedTerraSol proves an ask-user finding starts no fixer until
// consent, then the consented intent-sensitive cascade starts at fix_balanced
// (Terra) and escalates to authority_strong (Sol) — never fix_fast.
func TestCascadeConsentedTerraSol(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-consent"
    text: "found an intent-sensitive issue"
    structured:
      findings:
        - id: "consent-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "intent sensitive issue"
          action: ask-user
      risk_level: high
      risk_rationale: "challenges author intent"
  - match: "Fix the following"
    model: "terra"
    text: "fix_balanced attempt"
    edits:
      - path: "cascade.txt"
        new: "terra consent fix\n"
    structured:
      summary: "fix_balanced attempt"
  - match: "Fix the following"
    model: "sol"
    text: "authority attempt"
    edits:
      - path: "cascade.txt"
        new: "sol consent fix\n"
    structured:
      summary: "authority attempt"
  - match: "Independently verify whether each of the following"
    effort: "xhigh"
    text: "authority resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the authority reviewer confirms the fix"
      new_findings: []
  - match: "Independently verify whether each of the following"
    text: "unresolved at fix_balanced"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "unresolved"
          rationale: "not resolved at fix_balanced"
      new_findings: []
`+cascadeCleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-consent", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-consent")
	run := waitForStepStatus(t, h, "cascade-consent", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	// No fixer may run before consent.
	before := succeededAttemptsFor(h.InvocationAttempts(t, run.ID), types.PurposeIntentSensitiveRepair)
	if len(before) != 0 {
		t.Fatalf("intent-sensitive fixer ran %d times before consent, want 0", len(before))
	}

	// Consent to fixing the ask-user finding.
	h.RespondFix(t, run.ID, types.StepReview, "consent-1")
	h.WaitForRun("cascade-consent", 120*time.Second)

	attempts := h.InvocationAttempts(t, run.ID)
	fixers := succeededAttemptsFor(attempts, types.PurposeIntentSensitiveRepair)
	if len(fixers) != 2 {
		t.Fatalf("consented fixer attempts = %d %v, want 2 (Terra then Sol)", len(fixers), candidateModels(fixers))
	}
	assertCandidate(t, fixers[0], "fix_balanced", 0, "terra", types.EffortMedium)
	assertCandidate(t, fixers[1], "authority_strong", 1, "sol", types.EffortXHigh)
	for _, f := range fixers {
		if f.Start.Candidate.Profile == "fix_fast" {
			t.Fatalf("consented repair used fix_fast (Luna); an ask-user fix must start at fix_balanced")
		}
	}
}

// TestCascadeTerminalFailClosed proves a blocking finding no tier resolves —
// including the authority_strong final reviewer — exhausts the cascade and fails
// closed as unresolved at the top tier rather than passing.
func TestCascadeTerminalFailClosed(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: cascade-failclosed"
    text: "found an unfixable bug"
    structured:
      findings:
        - id: "failclosed-1"
          severity: error
          file: "cascade.txt"
          line: 1
          description: "unfixable bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    model: "luna"
    edits:
      - path: "cascade.txt"
        new: "luna attempt\n"
    structured:
      summary: "luna attempt"
  - match: "Fix the following"
    model: "terra"
    edits:
      - path: "cascade.txt"
        new: "terra attempt\n"
    structured:
      summary: "terra attempt"
  - match: "Fix the following"
    model: "sol"
    edits:
      - path: "cascade.txt"
        new: "sol attempt\n"
    structured:
      summary: "sol attempt"
  - match: "Independently verify whether each of the following"
    text: "never resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "unresolved"
          rationale: "the bug persists"
      new_findings: []
`)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("cascade-failclosed", "cascade.txt", "buggy line\n", "add cascade target")
	h.PushToGate("cascade-failclosed")
	run := waitForStepStatus(t, h, "cascade-failclosed", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	repairs := h.FindingRepairs(t, run.ID)
	byTier := map[int]*db.FindingRepair{}
	for _, r := range repairs {
		byTier[r.Tier] = r
	}
	for tier := 0; tier <= 2; tier++ {
		if byTier[tier] == nil {
			t.Fatalf("missing repair row for tier %d; the cascade must exhaust every tier before failing closed", tier)
		}
	}
	if byTier[2].Status != db.RepairStatusUnresolved {
		t.Fatalf("tier 2 status = %q, want unresolved (fail closed at the top tier)", byTier[2].Status)
	}
	for _, r := range repairs {
		if r.Status == db.RepairStatusResolved {
			t.Fatalf("tier %d resolved, but no tier should resolve an unfixable finding", r.Tier)
		}
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("cascade-failclosed", 60*time.Second)
}
