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
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The publication journeys prove ticket-19 criterion 245 end to end against the
// real no-mistakes binary + fake agent: the pipeline seals an immutable reviewed
// candidate, Verify skips an unchanged candidate but escalates a changed one to
// authority_strong (xhigh), and Push refuses to discard an out-of-band upstream
// commit. Every fact is asserted from the durable daemon DB (seals, invocation
// attempts, step results), not just the prompt log. CI republish is exercised at
// its real remote-and-seal seam in the focused pipeline step tests.

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

func assertPublicationCompleted(t *testing.T, h *Harness, run *ipc.RunInfo, branch string, verifyCount int, purpose types.Purpose, profile string, effort types.Effort) {
	t.Helper()
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	if run.AwaitingAgent {
		t.Fatal("completed publication run still reports a stale approval wait")
	}
	assertPipelineStepsInOrder(t, run.Steps)
	for _, want := range []struct {
		name   types.StepName
		status types.StepStatus
	}{
		{types.StepRebase, types.StepStatusCompleted},
		{types.StepReview, types.StepStatusCompleted},
		{types.StepTest, types.StepStatusCompleted},
		{types.StepDocument, types.StepStatusCompleted},
		{types.StepLint, types.StepStatusCompleted},
		{types.StepVerify, types.StepStatusCompleted},
		{types.StepPush, types.StepStatusCompleted},
		{types.StepPR, types.StepStatusSkipped},
		{types.StepCI, types.StepStatusSkipped},
	} {
		step, ok := findStep(run.Steps, want.name)
		if !ok {
			t.Fatalf("publication run has no %s step", want.name)
		}
		if step.Status != want.status {
			t.Fatalf("%s step status = %s, want %s", want.name, step.Status, want.status)
		}
	}
	if verifyCount > 0 {
		review, _ := findStep(run.Steps, types.StepReview)
		if review.FindingsJSON != nil {
			t.Fatalf("resolved Review still exposes stale findings: %s", *review.FindingsJSON)
		}
	}
	assertPublicationVerification(t, h, run, verifyCount, purpose, profile, effort)
	assertPublicationSeals(t, h, run.ID, run.HeadSHA)
	if upstream := h.UpstreamBranchSHA(branch); upstream != run.HeadSHA {
		t.Fatalf("upstream branch SHA = %s, want exact sealed run head %s", upstream, run.HeadSHA)
	}
	assertNoPRCreated(t, run)
	assertNoCIVerifyReentry(t, h, run)
}

func assertPublicationVerification(t *testing.T, h *Harness, run *ipc.RunInfo, wantCount int, purpose types.Purpose, profile string, effort types.Effort) {
	t.Helper()
	attempts := verifyStepAttempts(t, h, run.ID, h.InvocationAttempts(t, run.ID))
	if len(attempts) != wantCount {
		t.Fatalf("Verify launched %d verifier(s) %v, want exactly %d", len(attempts), candidateModels(attempts), wantCount)
	}
	if wantCount == 0 {
		return
	}
	attempt := attempts[0]
	if attempt.Start.Purpose != purpose {
		t.Fatalf("Verify purpose = %q, want %q", attempt.Start.Purpose, purpose)
	}
	candidate := attempt.Start.Candidate
	if candidate.Profile != profile || candidate.Tier != 0 || candidate.Effort != effort {
		t.Fatalf("Verify candidate = {profile:%q tier:%d effort:%q}, want {profile:%q tier:0 effort:%q}",
			candidate.Profile, candidate.Tier, candidate.Effort, profile, effort)
	}
	if attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("Verify verifier did not succeed: %+v", attempt.Terminal)
	}
}

func assertPublicationSeals(t *testing.T, h *Harness, runID, wantSHA string) {
	t.Helper()
	d := h.OpenDB(t)
	defer d.Close()
	reviewed, err := d.LatestSealByReason(runID, "reviewed")
	if err != nil {
		t.Fatalf("load reviewed seal: %v", err)
	}
	preVerify, err := d.LatestSealByReason(runID, "pre_verify")
	if err != nil {
		t.Fatalf("load pre-Verify seal: %v", err)
	}
	latest, err := d.LatestSeal(runID)
	if err != nil {
		t.Fatalf("load latest seal: %v", err)
	}
	if reviewed == nil || reviewed.SHA != wantSHA {
		t.Fatalf("reviewed seal = %+v, want exact SHA %s", reviewed, wantSHA)
	}
	if preVerify == nil || preVerify.SHA != wantSHA {
		t.Fatalf("pre-Verify seal = %+v, want exact SHA %s", preVerify, wantSHA)
	}
	if latest == nil || latest.SHA != wantSHA {
		t.Fatalf("latest seal = %+v, want exact SHA %s", latest, wantSHA)
	}
}

func assertNoCIVerifyReentry(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	ci, ok := findStep(run.Steps, types.StepCI)
	if !ok {
		t.Fatal("publication run has no CI step")
	}
	for _, attempt := range h.InvocationAttempts(t, run.ID) {
		if attempt.Start.Scope.StepResultID != ci.ID {
			continue
		}
		switch attempt.Start.Purpose {
		case types.PurposeNormalAggregateVerification, types.PurposeEscalatedAggregateVerification:
			t.Fatalf("CI re-entered Verify with purpose %q in attempt %s", attempt.Start.Purpose, attempt.ID)
		}
	}
}

func assertResolvedPublicationRepair(t *testing.T, h *Harness, run *ipc.RunInfo, description string, fixerPurpose, verifierPurpose types.Purpose) {
	t.Helper()
	repairs := h.FindingRepairs(t, run.ID)
	if len(repairs) != 1 {
		t.Fatalf("finding repairs = %d, want exactly one durable resolved repair", len(repairs))
	}
	repair := repairs[0]
	review, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatal("publication run has no Review step")
	}
	if repair.Description != description || repair.Tier != 0 || repair.Status != db.RepairStatusResolved || repair.Verdict != db.RepairVerdictResolved {
		t.Fatalf("repair = %+v, want description=%q tier=0 status/verdict=resolved", repair, description)
	}
	if repair.LineageID == "" || repair.StepRoundID == "" || repair.StepResultID != review.ID {
		t.Fatalf("repair durable scope = lineage:%q round:%q step:%q, want populated lineage/round linked to Review %q",
			repair.LineageID, repair.StepRoundID, repair.StepResultID, review.ID)
	}
	if repair.FixerAttemptID == "" || repair.VerifierAttemptID == "" || repair.FixerAttemptID == repair.VerifierAttemptID {
		t.Fatalf("repair attempt links = fixer:%q verifier:%q, want distinct durable links", repair.FixerAttemptID, repair.VerifierAttemptID)
	}
	attempts := h.InvocationAttempts(t, run.ID)
	fixer := publicationAttemptByID(t, attempts, repair.FixerAttemptID)
	verifier := publicationAttemptByID(t, attempts, repair.VerifierAttemptID)
	if fixer.Start.Purpose != fixerPurpose || verifier.Start.Purpose != verifierPurpose {
		t.Fatalf("repair linked purposes = fixer:%q verifier:%q, want %q/%q",
			fixer.Start.Purpose, verifier.Start.Purpose, fixerPurpose, verifierPurpose)
	}
	for role, attempt := range map[string]*db.InvocationAttempt{"fixer": fixer, "verifier": verifier} {
		if attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeSucceeded {
			t.Fatalf("linked %s attempt did not succeed: %+v", role, attempt.Terminal)
		}
	}
}

func publicationAttemptByID(t *testing.T, attempts []*db.InvocationAttempt, id string) *db.InvocationAttempt {
	t.Helper()
	for _, attempt := range attempts {
		if attempt.ID == id {
			return attempt
		}
	}
	t.Fatalf("durably linked invocation attempt %q not found", id)
	return nil
}

func assertPublicationInitialRisk(t *testing.T, h *Harness, run *ipc.RunInfo, want string) {
	t.Helper()
	review, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatal("publication run has no Review step")
	}
	d := h.OpenDB(t)
	defer d.Close()
	rounds, err := d.GetRoundsByStep(review.ID)
	if err != nil {
		t.Fatalf("load Review rounds: %v", err)
	}
	for _, round := range rounds {
		if round.Round != 1 || round.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err != nil {
			t.Fatalf("parse durable initial Review findings: %v", err)
		}
		if findings.RiskLevel != want {
			t.Fatalf("durable initial Review risk = %q, want %q", findings.RiskLevel, want)
		}
		return
	}
	t.Fatal("durable initial Review round with findings was not recorded")
}

func assertNoPublicationRepairs(t *testing.T, h *Harness, runID string) {
	t.Helper()
	if repairs := h.FindingRepairs(t, runID); len(repairs) != 0 {
		t.Fatalf("clean publication recorded %d repair row(s), want none", len(repairs))
	}
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

	run := h.WaitForRun("publication-clean-seal", 30*time.Second)
	assertPublicationCompleted(t, h, run, "publication-clean-seal", 0, "", "", "")
	assertNoPublicationRepairs(t, h, run.ID)
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

	run := h.WaitForRun("publication-verify-skip", 30*time.Second)
	assertPublicationCompleted(t, h, run, "publication-verify-skip", 0, "", "", "")
	assertNoPublicationRepairs(t, h, run.ID)
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
	originalHead := h.CommitChange("publication-verify-xhigh", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-verify-xhigh")

	run := h.WaitForRun("publication-verify-xhigh", 30*time.Second)
	if run.HeadSHA == originalHead {
		t.Fatalf("resolved repair did not change candidate head %s", originalHead)
	}
	assertPublicationInitialRisk(t, h, run, "high")
	assertResolvedPublicationRepair(t, h, run, "high-risk blocking bug", types.PurposeStructuredFindingRepair, types.PurposeNormalAggregateVerification)
	assertPublicationCompleted(t, h, run, "publication-verify-xhigh", 1, types.PurposeEscalatedAggregateVerification, "authority_strong", types.EffortXHigh)
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
	originalHead := h.CommitChange("publication-verify-normal", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-verify-normal")

	run := h.WaitForRun("publication-verify-normal", 30*time.Second)
	if run.HeadSHA == originalHead {
		t.Fatalf("resolved repair did not change candidate head %s", originalHead)
	}
	assertPublicationInitialRisk(t, h, run, "medium")
	assertResolvedPublicationRepair(t, h, run, "blocking bug at normal risk", types.PurposeStructuredFindingRepair, types.PurposeNormalAggregateVerification)
	assertPublicationCompleted(t, h, run, "publication-verify-normal", 1, types.PurposeNormalAggregateVerification, "review_strong", types.EffortHigh)
}

// TestPublicationRefusesPushOnUpstreamDrift proves criterion 245's Push drift
// refusal: after the automatic repair resolves, a separate ask-user finding
// deliberately parks Review so an out-of-band commit can land after Rebase.
// Approval continues through verification to Push, which must refuse to discard
// that unseen commit and leave it intact upstream.
func TestPublicationRefusesPushOnUpstreamDrift(t *testing.T) {
	scenario := writeScenario(t, `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: publication-drift"
    text: "found a repairable bug and a separate user decision"
    structured:
      findings:
        - id: "drift-1"
          severity: error
          file: "pub.txt"
          line: 1
          description: "blocking bug repaired before the deliberate gate"
          action: auto-fix
        - id: "drift-decision"
          severity: warning
          file: "pub.txt"
          line: 1
          description: "human decision keeps the run parked before publication"
          action: ask-user
      risk_level: high
      risk_rationale: "the repair and remaining user decision require high assurance"
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
	originalHead := h.CommitChange("publication-drift", "pub.txt", "buggy line\n", "add publication target")
	h.PushToGate("publication-drift")

	run := waitForStepStatus(t, h, "publication-drift", types.StepReview, types.StepStatusAwaitingApproval, 15*time.Second)

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
	failed := h.WaitForRun("publication-drift", 30*time.Second)
	if failed.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed (Push must refuse to discard the out-of-band commit)", failed.Status)
	}
	if failed.HeadSHA == originalHead {
		t.Fatalf("resolved repair did not change candidate head %s", originalHead)
	}
	assertPublicationInitialRisk(t, h, failed, "high")
	assertResolvedPublicationRepair(t, h, failed, "blocking bug repaired before the deliberate gate", types.PurposeStructuredFindingRepair, types.PurposeNormalAggregateVerification)
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepVerify} {
		step, ok := findStep(failed.Steps, name)
		if !ok || step.Status != types.StepStatusCompleted {
			t.Fatalf("%s step = %+v, want completed before the Push refusal", name, step)
		}
	}
	assertPublicationVerification(t, h, failed, 1, types.PurposeEscalatedAggregateVerification, "authority_strong", types.EffortXHigh)
	assertPublicationSeals(t, h, failed.ID, failed.HeadSHA)
	assertNoCIVerifyReentry(t, h, failed)
	for _, name := range []types.StepName{types.StepPR, types.StepCI} {
		step, ok := findStep(failed.Steps, name)
		if !ok || step.Status != types.StepStatusPending {
			t.Fatalf("%s step = %+v, want pending because Push failed closed first", name, step)
		}
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
	originalHead := h.CommitChange("publication-verify-intent", "verify-intent.txt", "original line\n", "add intent target")
	h.PushToGate("publication-verify-intent")

	run := h.WaitForRun("publication-verify-intent", 30*time.Second)
	if run.HeadSHA == originalHead {
		t.Fatalf("resolved repair did not change candidate head %s", originalHead)
	}
	intent := readRunIntent(t, h.NMHome, run.ID)
	if intent.summary == nil || strings.TrimSpace(*intent.summary) == "" {
		t.Fatalf("run intent is empty; the intent trigger requires a non-empty UserIntent")
	}
	assertPublicationInitialRisk(t, h, run, "low")
	assertResolvedPublicationRepair(t, h, run, "informational nit", types.PurposeInformationalRepair, types.PurposeInformationalRepairVerification)
	assertPublicationCompleted(t, h, run, "publication-verify-intent", 1, types.PurposeEscalatedAggregateVerification, "authority_strong", types.EffortXHigh)
}
