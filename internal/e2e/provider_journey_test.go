//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// countReviewExecs returns how many fake-agent execs carried the initial-review
// prompt, so a transient-retry journey can prove the adapter re-execs the CLI
// within one Invoke.
func countReviewExecs(invs []Invocation) int {
	n := 0
	for _, inv := range invs {
		if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
			n++
		}
	}
	return n
}

// distinctSkipPurposes returns the set of Purposes an open circuit skipped, so a
// run-wide circuit journey can prove the skip spanned more than one step.
func distinctSkipPurposes(skips []*db.InvocationAttempt) map[types.Purpose]bool {
	out := make(map[types.Purpose]bool)
	for _, a := range skips {
		out[a.Start.Purpose] = true
	}
	return out
}

// TestProviderAdapterRetryThenSuccess proves an adapter that hits a transient
// provider failure re-execs the CLI within one Invoke and then succeeds: the
// Review route is the single review_strong profile, so the codex (OpenAI)
// candidate's retry-then-success is unambiguous. Exactly one SUCCEEDED review
// attempt is recorded (retries live inside one Invoke, not as separate
// attempts), while the fake-agent log shows three execs for the same review
// prompt (two transient failures, then the success).
func TestProviderAdapterRetryThenSuccess(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: provider-retry-review"
    fail: transient
    fail_times: 2
    text: "recovered after transient provider errors"
    structured:
      findings:
        - id: "retry-1"
          severity: error
          file: "provider.txt"
          line: 1
          description: "needs a human decision"
          action: ask-user
      risk_level: high
      risk_rationale: "a blocking finding parks the review"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("provider-retry-review", "provider.txt", "retry target\n", "add provider retry target")
	h.PushToGate("provider-retry-review")
	run := waitForStepStatus(t, h, "provider-retry-review", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	attempts := h.InvocationAttempts(t, run.ID)
	reviewAttempts := attemptsForPurpose(attempts, types.PurposeInitialReview)
	if len(reviewAttempts) != 1 {
		t.Fatalf("review attempts = %d %v, want exactly 1 (retries are within one Invoke, not new attempts)", len(reviewAttempts), candidateModels(reviewAttempts))
	}
	succeeded := succeededAttemptsFor(attempts, types.PurposeInitialReview)
	if len(succeeded) != 1 {
		t.Fatalf("succeeded review attempts = %d, want exactly 1", len(succeeded))
	}
	assertCandidate(t, succeeded[0], "review_strong", 0, "sol", types.EffortHigh)
	if succeeded[0].Start.Candidate.Runner != types.RunnerCodex || succeeded[0].Start.Candidate.CandidateIndex != 0 {
		t.Fatalf("succeeded review candidate = {runner:%q index:%d}, want the primary codex candidate", succeeded[0].Start.Candidate.Runner, succeeded[0].Start.Candidate.CandidateIndex)
	}

	// The single SUCCEEDED attempt hides three CLI execs: fail_times=2 transient
	// failures then the fall-through success, all under one prompt.
	if execs := countReviewExecs(h.AgentInvocations()); execs != 3 {
		t.Fatalf("review prompt execs = %d, want 3 (two transient failures + one success within one Invoke)", execs)
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("provider-retry-review", 60*time.Second)
}

// TestProviderCircuitOpensAnthropicBackup proves a classified operational
// failure on the routed codex (OpenAI) candidate opens that provider's circuit
// and fails over to the same-Profile claude (Anthropic) backup. For the single
// review_strong Review route, the codex attempt terminal is failed with the
// openai failure domain, and the candidate_index 1 claude candidate launches
// and succeeds for the same purpose.
func TestProviderCircuitOpensAnthropicBackup(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: provider-circuit-open"
    model: "gpt"
    fail: operational
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: provider-circuit-open"
    model: "claude"
    text: "anthropic backup review"
    structured:
      findings:
        - id: "backup-1"
          severity: error
          file: "provider.txt"
          line: 1
          description: "needs a human decision"
          action: ask-user
      risk_level: high
      risk_rationale: "a blocking finding parks the review"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("provider-circuit-open", "provider.txt", "circuit target\n", "add provider circuit target")
	h.PushToGate("provider-circuit-open")
	run := waitForStepStatus(t, h, "provider-circuit-open", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	attempts := h.InvocationAttempts(t, run.ID)
	reviewAttempts := attemptsForPurpose(attempts, types.PurposeInitialReview)
	if len(reviewAttempts) != 2 {
		t.Fatalf("review attempts = %d %v, want 2 (codex operational failure -> anthropic backup)", len(reviewAttempts), candidateModels(reviewAttempts))
	}

	codex := reviewAttempts[0]
	if codex.Start.Candidate.Runner != types.RunnerCodex || codex.Start.Candidate.CandidateIndex != 0 {
		t.Fatalf("first review candidate = {runner:%q index:%d}, want primary codex", codex.Start.Candidate.Runner, codex.Start.Candidate.CandidateIndex)
	}
	if codex.Terminal == nil || codex.Terminal.Outcome != types.InvocationOutcomeFailed || codex.Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("codex review terminal = %+v, want failed with openai failure domain (circuit opened)", codex.Terminal)
	}

	claude := reviewAttempts[1]
	if claude.Start.Candidate.Runner != types.RunnerClaude || claude.Start.Candidate.CandidateIndex != 1 {
		t.Fatalf("second review candidate = {runner:%q index:%d}, want same-Profile anthropic backup at index 1", claude.Start.Candidate.Runner, claude.Start.Candidate.CandidateIndex)
	}
	assertCandidate(t, claude, "review_strong", 0, "fable", types.EffortHigh)
	if claude.Terminal == nil || claude.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("claude review terminal = %+v, want succeeded (backup served the purpose)", claude.Terminal)
	}

	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	h.WaitForRun("provider-circuit-open", 60*time.Second)
}

// TestProviderCircuitRunWideSkipsLaterSteps proves the provider circuit is
// run-wide: once the OpenAI circuit opens on the Review step, every later routed
// step in the same run skips its codex candidate without launching (an
// InvocationOutcomeSkipped attempt carrying the openai domain) and serves the
// purpose on the Anthropic backup. The skip spans more than one step, and no
// codex candidate ever succeeds after the circuit opens.
func TestProviderCircuitRunWideSkipsLaterSteps(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: provider-circuit-runwide"
    model: "gpt"
    fail: operational
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("provider-circuit-runwide", "provider.txt", "runwide target\n", "add provider runwide target")
	h.PushToGate("provider-circuit-runwide")
	run := h.WaitForRun("provider-circuit-runwide", 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (anthropic backups carry the run after the openai circuit opens); error=%v", run.Status, deref(run.Error))
	}

	attempts := h.InvocationAttempts(t, run.ID)

	// The Review codex candidate is the circuit opener: a launched operational
	// failure, not a skip.
	review := attemptsForPurpose(attempts, types.PurposeInitialReview)
	if len(review) == 0 || review[0].Terminal == nil || review[0].Terminal.Outcome != types.InvocationOutcomeFailed || review[0].Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("initial review codex terminal = %+v, want failed with openai domain (opens the run-wide circuit)", review)
	}

	// After the circuit opens, every later codex candidate is skipped without
	// launching, spanning more than one step.
	skips := circuitSkips(attempts, types.FailureDomainOpenAI)
	if len(skips) == 0 {
		t.Fatalf("openai circuit skips = 0, want later steps to skip their codex candidate")
	}
	for _, s := range skips {
		if s.Start.Candidate.Runner != types.RunnerCodex || s.Start.Candidate.CandidateIndex != 0 {
			t.Fatalf("skipped candidate = {runner:%q index:%d purpose:%q}, want the primary codex candidate", s.Start.Candidate.Runner, s.Start.Candidate.CandidateIndex, s.Start.Purpose)
		}
	}
	purposes := distinctSkipPurposes(skips)
	if len(purposes) < 2 {
		t.Fatalf("openai circuit skipped %d distinct purpose(s) %v, want >1 (run-wide across steps)", len(purposes), purposes)
	}

	// No codex candidate ever produces a success after the circuit opens: the
	// OpenAI provider is out for the rest of the run.
	for _, a := range attempts {
		if a.Start.Candidate.Runner == types.RunnerCodex && a.Terminal != nil && a.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			t.Fatalf("codex attempt for %q succeeded after the circuit opened; the openai circuit must stay open run-wide", a.Start.Purpose)
		}
	}

	// A concrete later step: Test skips codex and succeeds on the anthropic backup.
	testAttempts := attemptsForPurpose(attempts, types.PurposeTestEvidence)
	if len(testAttempts) != 2 {
		t.Fatalf("test evidence attempts = %d %v, want 2 (codex skipped, claude backup)", len(testAttempts), candidateModels(testAttempts))
	}
	if testAttempts[0].Terminal == nil || testAttempts[0].Terminal.Outcome != types.InvocationOutcomeSkipped || testAttempts[0].Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("test codex terminal = %+v, want skipped with openai domain", testAttempts[0].Terminal)
	}
	if testAttempts[1].Start.Candidate.Runner != types.RunnerClaude || testAttempts[1].Terminal == nil || testAttempts[1].Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("test backup terminal = %+v, want anthropic success", testAttempts[1])
	}
}

// TestProviderAllDomainsFailClosed proves the run fails closed when every
// provider domain fails operationally for a routed purpose: both the codex
// (OpenAI) and claude (Anthropic) Review candidates fail operationally, so
// review_strong has no available candidate, no success is recorded for the
// purpose, and the run ends failed. Both domains recorded operational failures.
func TestProviderAllDomainsFailClosed(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: provider-failclosed"
    fail: operational
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("provider-failclosed", "provider.txt", "failclosed target\n", "add provider failclosed target")
	h.PushToGate("provider-failclosed")
	run := h.WaitForRun("provider-failclosed", 90*time.Second)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed (all provider circuits open, no candidate); error=%v", run.Status, deref(run.Error))
	}

	attempts := h.InvocationAttempts(t, run.ID)
	reviewAttempts := attemptsForPurpose(attempts, types.PurposeInitialReview)
	if len(reviewAttempts) != 2 {
		t.Fatalf("review attempts = %d %v, want 2 (codex then anthropic, both operational)", len(reviewAttempts), candidateModels(reviewAttempts))
	}
	if len(succeededAttemptsFor(attempts, types.PurposeInitialReview)) != 0 {
		t.Fatalf("succeeded review attempts = %d, want 0 (the purpose fails closed)", len(succeededAttemptsFor(attempts, types.PurposeInitialReview)))
	}

	codex := reviewAttempts[0]
	if codex.Start.Candidate.Runner != types.RunnerCodex || codex.Terminal == nil || codex.Terminal.Outcome != types.InvocationOutcomeFailed || codex.Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("codex review terminal = %+v, want failed with openai domain", codex.Terminal)
	}
	claude := reviewAttempts[1]
	if claude.Start.Candidate.Runner != types.RunnerClaude || claude.Terminal == nil || claude.Terminal.Outcome != types.InvocationOutcomeFailed || claude.Terminal.FailureDomain != types.FailureDomainAnthropic {
		t.Fatalf("claude review terminal = %+v, want failed with anthropic domain", claude.Terminal)
	}
}

// TestProviderNonOperationalNeverOpensCircuit proves a non-operational failure
// never opens a provider circuit. A tier-0 informational fixer succeeds and its
// independent verifier returns unresolved, which warrants escalation from
// fix_fast to tools_balanced. The tier-1 codex fixer then emits wire-valid
// output with no parseable structured result (fail: output). That model-output
// failure conclusively ends the non-blocking lineage as inconclusive: it does
// not fail over, open a circuit, or prevent later codex Test and Lint attempts.
func TestProviderNonOperationalNeverOpensCircuit(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: provider-nonop-control"
    text: "informational cleanup suggestion"
    structured:
      findings:
        - id: "info-1"
          severity: info
          file: "provider.txt"
          line: 1
          description: "a purely informational cleanup"
          action: auto-fix
      risk_level: low
      risk_rationale: "informational finding only"
  - match: "Fix the following"
    model: "luna"
    text: "first-tier cleanup attempt"
    edits:
      - path: "provider.txt"
        new: "first-tier cleanup\n"
    structured:
      summary: "first-tier cleanup attempt"
  - match: "Independently verify whether each of the following"
    text: "cleanup remains unresolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "unresolved"
          rationale: "the first-tier cleanup is insufficient"
      new_findings: []
  - match: "Fix the following"
    model: "terra"
    fail: output
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("provider-nonop-control", "provider.txt", "nonop target\n", "add provider nonop target")
	h.PushToGate("provider-nonop-control")
	run := h.WaitForRun("provider-nonop-control", 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (an inconclusive informational repair is non-blocking); error=%v", run.Status, deref(run.Error))
	}
	if run.BlockingRepairUnresolved {
		t.Fatal("run.BlockingRepairUnresolved = true, want false for an informational lineage")
	}

	attempts := h.InvocationAttempts(t, run.ID)

	// No attempt carries an operational failure domain or a circuit-skip
	// terminal. The model-output failure therefore leaves both provider circuits
	// closed for the entire run.
	for _, attempt := range attempts {
		if attempt.Terminal == nil {
			t.Fatalf("attempt %q for %q has no terminal outcome", attempt.ID, attempt.Start.Purpose)
		}
		if attempt.Terminal.FailureDomain != "" {
			t.Fatalf("attempt for %q terminal failure domain = %q, want empty throughout the non-operational journey", attempt.Start.Purpose, attempt.Terminal.FailureDomain)
		}
		if attempt.Terminal.Outcome == types.InvocationOutcomeSkipped {
			t.Fatalf("attempt for %q was circuit-skipped, want every candidate launched", attempt.Start.Purpose)
		}
	}
	if skips := circuitSkips(attempts, types.FailureDomainOpenAI); len(skips) != 0 {
		t.Fatalf("openai circuit skips = %d, want 0", len(skips))
	}
	if skips := circuitSkips(attempts, types.FailureDomainAnthropic); len(skips) != 0 {
		t.Fatalf("anthropic circuit skips = %d, want 0", len(skips))
	}

	// The unresolved tier-0 verifier is the evidence that warrants escalation.
	// Both repair tiers stay on the codex primary; only the second tier fails.
	fixers := attemptsForPurpose(attempts, types.PurposeInformationalRepair)
	if len(fixers) != 2 {
		t.Fatalf("informational repair attempts = %d %v, want exactly 2 (fix_fast then warranted tools_balanced)", len(fixers), candidateModels(fixers))
	}
	assertCandidate(t, fixers[0], "fix_fast", 0, "luna", types.EffortMedium)
	if fixers[0].Start.Candidate.Runner != types.RunnerCodex || fixers[0].Start.Candidate.CandidateIndex != 0 ||
		fixers[0].Terminal == nil || fixers[0].Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("tier-0 informational fixer = %+v, want succeeded primary codex candidate", fixers[0])
	}
	assertCandidate(t, fixers[1], "tools_balanced", 1, "terra", types.EffortHigh)
	if fixers[1].Start.Candidate.Runner != types.RunnerCodex || fixers[1].Start.Candidate.CandidateIndex != 0 ||
		fixers[1].Terminal == nil || fixers[1].Terminal.Outcome != types.InvocationOutcomeFailed || fixers[1].Terminal.FailureDomain != "" {
		t.Fatalf("tier-1 informational fixer = %+v, want failed primary codex candidate with no failure domain", fixers[1])
	}

	verifiers := attemptsForPurpose(attempts, types.PurposeInformationalRepairVerification)
	if len(verifiers) != 1 {
		t.Fatalf("informational verifier attempts = %d %v, want exactly 1 at tier 0", len(verifiers), candidateModels(verifiers))
	}
	assertCandidate(t, verifiers[0], "tools_balanced", 0, "terra", types.EffortHigh)
	if verifiers[0].Start.Candidate.Runner != types.RunnerCodex || verifiers[0].Start.Candidate.CandidateIndex != 0 ||
		verifiers[0].Terminal == nil || verifiers[0].Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("tier-0 informational verifier = %+v, want succeeded primary codex candidate", verifiers[0])
	}

	// The durable lineage records a real unresolved adjudication at tier 0,
	// followed by a terminal inconclusive tier-1 row. Invocation failure is not a
	// verifier verdict, so the second row deliberately has no attempt links.
	repairs := h.FindingRepairs(t, run.ID)
	if len(repairs) != 2 {
		t.Fatalf("finding repairs = %d, want exactly 2 durable tiers", len(repairs))
	}
	first, second := repairs[0], repairs[1]
	if first.LineageID == "" || second.LineageID != first.LineageID {
		t.Fatalf("repair lineage IDs = %q / %q, want one non-empty lineage", first.LineageID, second.LineageID)
	}
	if first.Severity != "info" || first.Action != string(types.ActionAutoFix) || first.Description != "a purely informational cleanup" ||
		first.Tier != 0 || first.RemainingBudget != 1 || first.Status != db.RepairStatusUnresolved ||
		first.Verdict != db.RepairVerdictUnresolved || first.VerdictRationale != "the first-tier cleanup is insufficient" ||
		first.FixerAttemptID != fixers[0].ID || first.VerifierAttemptID != verifiers[0].ID {
		t.Fatalf("tier-0 repair = %+v, want linked unresolved informational adjudication with one tier remaining", first)
	}
	if second.Severity != "info" || second.Action != string(types.ActionAutoFix) || second.Description != "a purely informational cleanup" ||
		second.Tier != 1 || second.RemainingBudget != 0 || second.Status != db.RepairStatusUnresolved ||
		second.Verdict != db.RepairVerdictInconclusive || second.VerdictRationale != "fixer invocation failed" ||
		second.FixerAttemptID != "" || second.VerifierAttemptID != "" {
		t.Fatalf("tier-1 repair = %+v, want terminal unlinked inconclusive informational disposition", second)
	}

	// Closed circuits keep the exact primary tools_balanced route available to
	// later Test and Lint work.
	assertLaterCodexSuccess := func(purpose types.Purpose) {
		t.Helper()
		got := attemptsForPurpose(attempts, purpose)
		if len(got) != 1 {
			t.Fatalf("purpose %q attempts = %d %v, want exactly one primary codex attempt", purpose, len(got), candidateModels(got))
		}
		assertCandidate(t, got[0], "tools_balanced", 0, "terra", types.EffortHigh)
		if got[0].Start.Candidate.Runner != types.RunnerCodex || got[0].Start.Candidate.CandidateIndex != 0 ||
			got[0].Terminal == nil || got[0].Terminal.Outcome != types.InvocationOutcomeSucceeded {
			t.Fatalf("purpose %q attempt = %+v, want succeeded primary codex candidate", purpose, got[0])
		}
	}
	assertLaterCodexSuccess(types.PurposeTestEvidence)
	assertLaterCodexSuccess(types.PurposeLintInspection)
}
