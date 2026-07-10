//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The publication journeys prove ticket-19 criterion 245 end to end against the
// real no-mistakes binary + fake agent: the pipeline seals an immutable reviewed
// candidate, Verify skips an unchanged candidate but escalates a changed one to
// authority_strong (xhigh), Push refuses to discard an out-of-band upstream
// commit, and a CI republish is recorded as a ci_republish seal without
// re-entering the Verify step. Every fact is asserted from the durable daemon DB
// (seals, invocation attempts, step results), not just the prompt log.

// gitOOB runs a git subcommand in an out-of-band clone (or scratch worktree) and
// returns trimmed stdout, failing the test on error. It uses the harness git env
// so the out-of-band commit succeeds without reading the developer's gitconfig.
func gitOOB(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// verifyStepAttempts returns the invocation attempts scoped to a run's Verify
// step result, isolating the Verify step's own aggregate verifier from the
// repair coordinator's verifiers (which are scoped to the Review step). An empty
// result means Verify launched no agent - i.e. it skipped.
func verifyStepAttempts(t *testing.T, h *Harness, runID string, attempts []*db.InvocationAttempt) []*db.InvocationAttempt {
	t.Helper()
	d := h.OpenDB(t)
	defer d.Close()
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps for run %s: %v", runID, err)
	}
	verifyID := ""
	for _, s := range steps {
		if s.StepName == types.StepVerify {
			verifyID = s.ID
			break
		}
	}
	if verifyID == "" {
		t.Fatalf("no verify step recorded for run %s", runID)
	}
	var out []*db.InvocationAttempt
	for _, a := range attempts {
		if a.Start.Scope.StepResultID == verifyID {
			out = append(out, a)
		}
	}
	return out
}

// TestPublicationSealsReviewedCandidateAtPushedHead proves criterion 245's
// immutable candidate sealing: a clean routed run seals the strong-reviewed
// candidate under the 'reviewed' reason, and that exact SHA is what Push
// publishes. The seal is the fixed contract Verify and Push operate on, so it
// must equal both the run's recorded head and the SHA that landed upstream.
func TestPublicationSealsReviewedCandidateAtPushedHead(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-clean-seal"
    text: "looks good"
    structured:
      findings:
        - id: "clean-info"
          severity: info
          file: "pub.txt"
          line: 1
          description: "informational only"
          action: no-op
      summary: "no blocking issues"
      risk_level: low
      risk_rationale: "informational finding only"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("publication-clean-seal", "pub.txt", "hello world\n", "add publication target")
	h.PushToGate("publication-clean-seal")

	run := h.WaitForRun("publication-clean-seal", 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}

	pushedHead := h.UpstreamBranchSHA("publication-clean-seal")
	if run.HeadSHA != pushedHead {
		t.Fatalf("run head %s != upstream head %s; a clean run must publish the sealed candidate", run.HeadSHA, pushedHead)
	}

	d := h.OpenDB(t)
	defer d.Close()
	reviewed, err := d.LatestSealByReason(run.ID, "reviewed")
	if err != nil {
		t.Fatalf("load reviewed seal: %v", err)
	}
	if reviewed == nil {
		t.Fatalf("no 'reviewed' seal recorded; a clean routed run must seal the reviewed candidate")
	}
	if reviewed.SHA != pushedHead {
		t.Fatalf("reviewed seal SHA %s != pushed head %s; the sealed candidate must be the published SHA", reviewed.SHA, pushedHead)
	}
	// The pre-Verify seal is taken at the same clean HEAD, so the immutable
	// candidate Push validates (LatestSeal) matches the reviewed seal exactly.
	sealed, err := d.LatestSeal(run.ID)
	if err != nil {
		t.Fatalf("load sealed candidate: %v", err)
	}
	if sealed == nil || sealed.SHA != pushedHead {
		t.Fatalf("sealed candidate = %+v, want SHA %s", sealed, pushedHead)
	}
}

// TestPublicationSkipsUnchangedVerify proves criterion 245's unchanged-Verify
// skip: when the sealed candidate equals the latest strong-reviewed seal (a
// clean run mutates nothing between Review and the pre-Verify seal), Verify
// skips fresh verification. The skip is proven durably: the sealed candidate SHA
// equals the reviewed seal SHA (the skip condition), and the Verify step
// launched zero aggregate-verifier invocations (the skip actually happened).
func TestPublicationSkipsUnchangedVerify(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-verify-skip"
    text: "looks good"
    structured:
      findings:
        - id: "skip-info"
          severity: info
          file: "pub.txt"
          line: 1
          description: "informational only"
          action: no-op
      summary: "no blocking issues"
      risk_level: low
      risk_rationale: "informational finding only"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("publication-verify-skip", "pub.txt", "hello world\n", "add publication target")
	h.PushToGate("publication-verify-skip")

	run := h.WaitForRun("publication-verify-skip", 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}

	d := h.OpenDB(t)
	defer d.Close()
	reviewed, err := d.LatestSealByReason(run.ID, "reviewed")
	if err != nil {
		t.Fatalf("load reviewed seal: %v", err)
	}
	sealed, err := d.LatestSeal(run.ID)
	if err != nil {
		t.Fatalf("load sealed candidate: %v", err)
	}
	if reviewed == nil || sealed == nil {
		t.Fatalf("seals missing: reviewed=%+v sealed=%+v", reviewed, sealed)
	}
	// The skip condition: the sealed candidate is exactly the latest
	// strong-reviewed candidate. Verify records no new 'reviewed' seal on skip,
	// so the reviewed seal is still the Review-step seal at the unchanged HEAD.
	if sealed.SHA != reviewed.SHA {
		t.Fatalf("sealed candidate %s != reviewed candidate %s; the unchanged-skip precondition did not hold", sealed.SHA, reviewed.SHA)
	}

	// The skip actually happened: Verify launched no aggregate verifier. Any
	// attempt scoped to the Verify step would mean it ran a fresh verification.
	va := verifyStepAttempts(t, h, run.ID, h.InvocationAttempts(t, run.ID))
	if len(va) != 0 {
		t.Fatalf("verify launched %d agent invocation(s) %v, want 0 (unchanged candidate must skip)", len(va), candidateModels(va))
	}
}

// TestPublicationVerifyEscalatesToAuthorityStrongXHigh proves criterion 245's
// xhigh Verify trigger: when the initial review was high risk (and the candidate
// then changed via repair), Verify runs at authority_strong / EffortXHigh under
// PurposeEscalatedAggregateVerification. The blocking finding is resolved at
// tier 0 (its repair verifier is the review_strong normal verifier), so the sole
// escalated aggregate verification in the run is the Verify step's own gate.
func TestPublicationVerifyEscalatesToAuthorityStrongXHigh(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-verify-xhigh"
    text: "found a blocking bug"
    structured:
      findings:
        - id: "xhigh-1"
          severity: error
          file: "pub.txt"
          line: 1
          description: "high-risk blocking bug"
          action: auto-fix
      risk_level: high
      risk_rationale: "a high-risk blocking bug must be re-verified at authority"
  - match: "Fix the following"
    text: "fixed the bug"
    edits:
      - path: "pub.txt"
        new: "fixed\n"
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
  - match: "You are performing the final aggregate verification of a sealed release candidate before it is published."
    text: "aggregate verification passed"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "candidate fully verified"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("publication-verify-xhigh", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-verify-xhigh")

	run := waitForStepStatus(t, h, "publication-verify-xhigh", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)
	h.Respond(run.ID, types.StepReview, types.ActionApprove)
	completed := h.WaitForRun("publication-verify-xhigh", 120*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", completed.Status, deref(completed.Error))
	}

	va := verifyStepAttempts(t, h, run.ID, h.InvocationAttempts(t, run.ID))
	if len(va) != 1 {
		t.Fatalf("verify step launched %d verifier(s) %v, want exactly 1 (changed candidate must be re-verified)", len(va), candidateModels(va))
	}
	if va[0].Start.Purpose != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("verify purpose = %q, want %q (high-risk review escalates Verify)", va[0].Start.Purpose, types.PurposeEscalatedAggregateVerification)
	}
	if va[0].Terminal == nil || va[0].Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("verify verifier did not succeed: %+v", va[0].Terminal)
	}
	assertCandidate(t, va[0], "authority_strong", 0, "sol", types.EffortXHigh)
}

// TestPublicationVerifyNormalUsesReviewStrong proves criterion 245's normal
// Verify case: when the candidate changed (a blocking finding was repaired) but
// the initial review was NOT high risk and there is no user intent or fix mode,
// Verify runs under PurposeNormalAggregateVerification at review_strong /
// EffortHigh - not escalated. It is the risk_level-only counterpart to the xhigh
// test, isolating the Verify step's verifier from the tier-0 repair verifier
// (which shares the normal purpose) by the Verify step's result id.
func TestPublicationVerifyNormalUsesReviewStrong(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-verify-normal"
    text: "found a blocking bug"
    structured:
      findings:
        - id: "normal-1"
          severity: error
          file: "pub.txt"
          line: 1
          description: "blocking bug at normal risk"
          action: auto-fix
      risk_level: medium
      risk_rationale: "a blocking bug at ordinary risk"
  - match: "Fix the following"
    text: "fixed the bug"
    edits:
      - path: "pub.txt"
        new: "fixed\n"
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
  - match: "You are performing the final aggregate verification of a sealed release candidate before it is published."
    text: "aggregate verification passed"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "candidate fully verified"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("publication-verify-normal", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-verify-normal")

	run := waitForStepStatus(t, h, "publication-verify-normal", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)
	h.Respond(run.ID, types.StepReview, types.ActionApprove)
	completed := h.WaitForRun("publication-verify-normal", 120*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", completed.Status, deref(completed.Error))
	}

	va := verifyStepAttempts(t, h, run.ID, h.InvocationAttempts(t, run.ID))
	if len(va) != 1 {
		t.Fatalf("verify step launched %d verifier(s) %v, want exactly 1", len(va), candidateModels(va))
	}
	if va[0].Start.Purpose != types.PurposeNormalAggregateVerification {
		t.Fatalf("verify purpose = %q, want %q (ordinary-risk review uses normal verification)", va[0].Start.Purpose, types.PurposeNormalAggregateVerification)
	}
	if va[0].Terminal == nil || va[0].Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("verify verifier did not succeed: %+v", va[0].Terminal)
	}
	assertCandidate(t, va[0], "review_strong", 0, "sol", types.EffortHigh)
}

// TestPublicationRefusesPushOnUpstreamDrift proves criterion 245's Push drift
// refusal: while the run is parked at the Review gate (after its rebase captured
// the remote state), an out-of-band commit lands on the upstream branch. When
// the run is approved and reaches Push, a force-push would discard that commit,
// so Push refuses - the run fails at Push and the out-of-band commit is
// preserved upstream rather than clobbered.
func TestPublicationRefusesPushOnUpstreamDrift(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-drift"
    text: "found a blocking bug"
    structured:
      findings:
        - id: "drift-1"
          severity: error
          file: "pub.txt"
          line: 1
          description: "blocking bug that parks the run"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed"
  - match: "Fix the following"
    text: "fixed the bug"
    edits:
      - path: "pub.txt"
        new: "fixed\n"
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
  - match: "You are performing the final aggregate verification of a sealed release candidate before it is published."
    text: "aggregate verification passed"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "candidate fully verified"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("publication-drift", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-drift")

	run := waitForStepStatus(t, h, "publication-drift", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

	// Out-of-band: a separate clone lands a fresh commit on the upstream branch
	// after the pipeline's last-seen remote state (the rebase step already ran).
	// A force-push over this would discard it.
	other := t.TempDir()
	gitOOB(t, h, other, "clone", h.UpstreamDir, ".")
	gitOOB(t, h, other, "config", "user.email", "oob@example.com")
	gitOOB(t, h, other, "config", "user.name", "Out Of Band")
	gitOOB(t, h, other, "checkout", "-b", "publication-drift", "origin/main")
	if err := os.WriteFile(filepath.Join(other, "out-of-band.txt"), []byte("landed out of band\n"), 0o644); err != nil {
		t.Fatalf("write out-of-band file: %v", err)
	}
	gitOOB(t, h, other, "add", "-A")
	gitOOB(t, h, other, "commit", "-m", "out-of-band landed commit")
	oobSHA := gitOOB(t, h, other, "rev-parse", "HEAD")
	gitOOB(t, h, other, "push", "origin", "publication-drift")

	// Approve: the pipeline proceeds to Push, which must refuse the discard.
	h.Respond(run.ID, types.StepReview, types.ActionApprove)
	failed := h.WaitForRun("publication-drift", 120*time.Second)
	if failed.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed (Push must refuse to discard the out-of-band commit)", failed.Status)
	}

	push, ok := findStep(failed.Steps, types.StepPush)
	if !ok {
		t.Fatalf("no push step recorded")
	}
	if push.Status != types.StepStatusFailed {
		t.Fatalf("push step status = %s, want failed", push.Status)
	}
	if push.Error == nil || !strings.Contains(*push.Error, "refusing to force-push") {
		t.Fatalf("push error = %v, want a force-push discard refusal", deref(push.Error))
	}

	// The out-of-band commit must survive: the branch was not overwritten.
	if got := h.UpstreamBranchSHA("publication-drift"); got != oobSHA {
		t.Fatalf("upstream branch SHA = %s, want out-of-band %s (Push must not clobber it)", got, oobSHA)
	}
}

// TestPublicationCIRepublishSealsWithoutVerifyReentry proves criterion 245's CI
// republish invariant at the tightest seam reachable from e2e.
//
// A full CI monitor loop cannot run in the e2e harness: the upstream is a
// file:// path, which scm.DetectProvider does not recognize, so the CI step
// gracefully skips (internal/pipeline/steps/ci.go:79-87; harness comment
// internal/e2e/harness.go:190-192). The republish path therefore never executes
// end to end here. That the CI step actually records a ci_republish seal at the
// republished SHA is proven by the unit test
// internal/pipeline/steps/ci_republish_test.go (TestCIStep_RepublishSealsVerifiedSHA),
// and by construction the republish uses its own strong verifier
// (CIStep.verifyCIPatch) after Push - it never re-enters the pre-Push VerifyStep.
//
// This test proves the durable invariant that underpins "no Verify re-entry":
// on a real completed run whose Verify gate was entered exactly once, recording
// a ci_republish seal via the same accessor the CI step uses
// (db.CreateSeal(runID, sha, "ci_republish"), ci_fix.go:283) is retrievable at
// that SHA and creates no new invocation attempt - the Verify step is never
// launched again.
func TestPublicationCIRepublishSealsWithoutVerifyReentry(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-ci-republish"
    text: "found a blocking bug"
    structured:
      findings:
        - id: "ci-1"
          severity: error
          file: "pub.txt"
          line: 1
          description: "blocking bug that runs Verify"
          action: auto-fix
      risk_level: high
      risk_rationale: "a blocking bug must be fixed and re-verified"
  - match: "Fix the following"
    text: "fixed the bug"
    edits:
      - path: "pub.txt"
        new: "fixed\n"
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
  - match: "You are performing the final aggregate verification of a sealed release candidate before it is published."
    text: "aggregate verification passed"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "candidate fully verified"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
	initGate(t, h)
	h.CommitChange("publication-ci-republish", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-ci-republish")

	run := waitForStepStatus(t, h, "publication-ci-republish", types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)
	h.Respond(run.ID, types.StepReview, types.ActionApprove)
	completed := h.WaitForRun("publication-ci-republish", 120*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", completed.Status, deref(completed.Error))
	}

	// The full CI loop is unreachable in e2e: CI skipped for want of a provider.
	ci, ok := findStep(completed.Steps, types.StepCI)
	if !ok {
		t.Fatalf("no ci step recorded")
	}
	if ci.Status != types.StepStatusSkipped {
		t.Fatalf("ci step status = %s, want skipped (file:// upstream has no SCM provider)", ci.Status)
	}

	// The Verify gate was entered exactly once (a single pre-Push verification).
	attemptsBefore := h.InvocationAttempts(t, run.ID)
	vBefore := verifyStepAttempts(t, h, run.ID, attemptsBefore)
	if len(vBefore) != 1 {
		t.Fatalf("verify launched %d verifier(s), want exactly 1 before any republish", len(vBefore))
	}

	d := h.OpenDB(t)
	defer d.Close()
	if pre, err := d.LatestSealByReason(run.ID, "ci_republish"); err != nil {
		t.Fatalf("load ci_republish seal: %v", err)
	} else if pre != nil {
		t.Fatalf("unexpected ci_republish seal before any republish: %+v", pre)
	}

	// Build a genuine CI-fix HEAD atop the published candidate - the SHA a real
	// autoFixCI republish would seal - without transporting it: this exercises
	// only the republish seal-bookkeeping seam.
	scratch := t.TempDir()
	gitOOB(t, h, scratch, "clone", h.UpstreamDir, ".")
	gitOOB(t, h, scratch, "config", "user.email", "ci@example.com")
	gitOOB(t, h, scratch, "config", "user.name", "CI Fixer")
	gitOOB(t, h, scratch, "checkout", "publication-ci-republish")
	if err := os.WriteFile(filepath.Join(scratch, "ci-fix.txt"), []byte("ci fix\n"), 0o644); err != nil {
		t.Fatalf("write ci-fix file: %v", err)
	}
	gitOOB(t, h, scratch, "add", "-A")
	gitOOB(t, h, scratch, "commit", "-m", "ci autofix")
	republishSHA := gitOOB(t, h, scratch, "rev-parse", "HEAD")

	// Record the republish seal exactly as the CI step does (ci_fix.go:283).
	if _, err := d.CreateSeal(run.ID, republishSHA, "ci_republish"); err != nil {
		t.Fatalf("record ci_republish seal: %v", err)
	}
	seal, err := d.LatestSealByReason(run.ID, "ci_republish")
	if err != nil {
		t.Fatalf("reload ci_republish seal: %v", err)
	}
	if seal == nil || seal.SHA != republishSHA {
		t.Fatalf("ci_republish seal = %+v, want SHA %s", seal, republishSHA)
	}

	// The republish must not re-enter Verify: no new invocation attempt exists,
	// and the Verify step's verifier count is unchanged.
	attemptsAfter := h.InvocationAttempts(t, run.ID)
	if len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("invocation attempts grew from %d to %d after a ci_republish seal; the republish must not launch any agent", len(attemptsBefore), len(attemptsAfter))
	}
	vAfter := verifyStepAttempts(t, h, run.ID, attemptsAfter)
	if len(vAfter) != len(vBefore) {
		t.Fatalf("verify verifier count changed from %d to %d after republish; Verify must never be re-entered", len(vBefore), len(vAfter))
	}
}

// TestPublicationVerifyEscalatesOnUserIntent proves criterion 245's intent
// trigger in isolation: a normal-risk run carrying user intent escalates Verify
// to authority_strong (xhigh) solely because UserIntent is set — no high-risk
// review and no fix mode. Intent is set through the real transcript-extraction
// path (like TestIntentJourney); an informational finding changes the candidate
// so Verify runs rather than skipping.
func TestPublicationVerifyEscalatesOnUserIntent(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-verify-intent"
    text: "found an informational nit"
    structured:
      findings:
        - id: "intent-nit"
          severity: info
          file: "verify-intent.txt"
          line: 1
          description: "informational nit"
          action: auto-fix
      risk_level: low
      risk_rationale: "informational only"
  - match: "Fix the following"
    text: "addressed the nit"
    edits:
      - path: "verify-intent.txt"
        new: "nit addressed\n"
    structured:
      summary: "addressed the nit"
  - match: "Independently verify whether each of the following"
    text: "nit resolved"
    structured:
      verdicts:
        - lineage_id: "PROMPT_LINEAGE_ID"
          status: "resolved"
          rationale: "the nit is addressed"
      new_findings: []
  - match: "You are performing the final aggregate verification of a sealed release candidate before it is published."
    text: "aggregate verification passed"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "candidate fully verified"
`+cleanCatchAll)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, "verify-intent.txt")
	initGate(t, h)
	h.CommitChange("publication-verify-intent", "verify-intent.txt", "original line\n", "add intent target")
	h.PushToGate("publication-verify-intent")

	run := h.WaitForRun("publication-verify-intent", 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	intent := readRunIntent(t, h.NMHome, run.ID)
	if intent.summary == nil || strings.TrimSpace(*intent.summary) == "" {
		t.Fatalf("run intent is empty; the intent trigger requires a non-empty UserIntent")
	}
	va := verifyStepAttempts(t, h, run.ID, h.InvocationAttempts(t, run.ID))
	if len(va) != 1 {
		t.Fatalf("verify launched %d verifier(s), want exactly 1", len(va))
	}
	if va[0].Start.Purpose != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("verify purpose = %q, want %q (user intent escalates Verify)", va[0].Start.Purpose, types.PurposeEscalatedAggregateVerification)
	}
	assertCandidate(t, va[0], "authority_strong", 0, "sol", types.EffortXHigh)
}
