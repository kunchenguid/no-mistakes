package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// verdictSpec is a test's per-verifier-call adjudication of a batch.
type verdictSpec struct {
	resolved    map[string]bool // lineageID → resolved (absent/false ⇒ unresolved)
	newFindings []newFindingSpec
}

type newFindingSpec struct {
	description string
	severity    string
	action      string
	causedBy    string
}

var verifyLineageRE = regexp.MustCompile(`lineage (\S+), severity`)

// fakeRepairInvoker journals fixer/verifier attempts like the routed invoker,
// records the tier of each fixer call and the purpose of each verifier call,
// and drives verdicts through a per-call spec that sees the batch's lineage ids.
type fakeRepairInvoker struct {
	db               *db.DB
	verify           func(callIdx int, lineageIDs []string) verdictSpec
	fixEdit          func(callIdx int)
	fixerTiers       []int
	verifierPurposes []types.Purpose
	fixCalls         int
	verifyCalls      int
}

func (f *fakeRepairInvoker) Invoke(_ context.Context, req agent.InvocationRequest) (*agent.Result, error) {
	def, _ := types.PurposeDefinitionFor(req.Purpose)
	profile := profileForRequest(req)
	attemptID, err := f.db.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      req.Purpose,
		Role:         def.Role,
		Scope:        req.Scope,
		CandidateKey: profile + ":0:codex",
		Candidate:    types.InvocationCandidate{Profile: profile, Tier: req.Tier, CandidateIndex: 0, Runner: types.RunnerCodex, Model: "m", Effort: types.EffortMedium},
	})
	if err != nil {
		return nil, err
	}
	if def.Role == types.InvocationRoleFixer {
		f.fixerTiers = append(f.fixerTiers, req.Tier)
		if f.fixEdit != nil {
			f.fixEdit(f.fixCalls)
		}
		f.fixCalls++
		_ = f.db.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
		return &agent.Result{Output: []byte(`{"summary":"apply repair"}`)}, nil
	}
	f.verifierPurposes = append(f.verifierPurposes, req.Purpose)
	lineageIDs := verifyLineageRE.FindAllStringSubmatch(req.Payload.Prompt, -1)
	ids := make([]string, 0, len(lineageIDs))
	for _, m := range lineageIDs {
		ids = append(ids, m[1])
	}
	spec := verdictSpec{}
	if f.verify != nil {
		spec = f.verify(f.verifyCalls, ids)
	}
	f.verifyCalls++
	_ = f.db.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	return &agent.Result{Output: []byte(marshalBatchVerdict(ids, spec))}, nil
}

func profileForRequest(req agent.InvocationRequest) string {
	switch req.Purpose {
	case types.PurposeNormalAggregateVerification:
		return "review_strong"
	case types.PurposeEscalatedAggregateVerification:
		return "authority_strong"
	case types.PurposeInformationalRepairVerification:
		return "tools_balanced"
	case types.PurposeIntentSensitiveRepair:
		return []string{"fix_balanced", "authority_strong"}[req.Tier]
	case types.PurposeInformationalRepair:
		return []string{"fix_fast", "tools_balanced"}[req.Tier]
	default: // structured_finding_repair fixer
		return []string{"fix_fast", "fix_balanced", "authority_strong"}[req.Tier]
	}
}

func marshalBatchVerdict(lineageIDs []string, spec verdictSpec) string {
	var bv batchVerdict
	for _, id := range lineageIDs {
		status := db.RepairVerdictUnresolved
		if spec.resolved[id] {
			status = db.RepairVerdictResolved
		}
		bv.Verdicts = append(bv.Verdicts, struct {
			LineageID string `json:"lineage_id"`
			Status    string `json:"status"`
			Rationale string `json:"rationale"`
		}{LineageID: id, Status: status, Rationale: "adjudicated"})
	}
	for _, nf := range spec.newFindings {
		bv.NewFindings = append(bv.NewFindings, struct {
			Description       string `json:"description"`
			Severity          string `json:"severity"`
			Action            string `json:"action"`
			CausedByLineageID string `json:"caused_by_lineage_id"`
		}{Description: nf.description, Severity: nf.severity, Action: nf.action, CausedByLineageID: nf.causedBy})
	}
	data, _ := json.Marshal(bv)
	return string(data)
}

func repairFixture(t *testing.T, fake *fakeRepairInvoker, findings []types.Finding) (*repairCoordinator, []repairSeed) {
	t.Helper()
	database, _, run, _ := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseSHA := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "checkout", "-b", "feature")
	writeTestFile(t, dir, "app.go", "package app\n\nfunc F(p *int) int { return *p }\n")
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "-m", "feature")
	run.Branch = "feature"

	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round1, _ := database.ReserveStepRound(step.ID, 1, "initial")
	reviewScope := types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round1.ID}
	reviewAttempt, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose: types.PurposeInitialReview, Role: types.InvocationRoleVerifier, Scope: reviewScope,
		CandidateKey: "review_strong:0:codex",
		Candidate:    types.InvocationCandidate{Profile: "review_strong", Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh},
	})
	if err != nil {
		t.Fatalf("start review attempt: %v", err)
	}
	_ = database.FinishInvocationAttempt(reviewAttempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	displayIDs := make([]string, len(findings))
	for i, f := range findings {
		displayIDs[i] = f.ID
	}
	lineages, err := database.CreateFindingLineages(run.ID, reviewAttempt, displayIDs)
	if err != nil || len(lineages) != len(findings) {
		t.Fatalf("create lineages: %v (%v)", lineages, err)
	}
	_ = database.CompleteReservedStepRound(round1.ID, nil, nil, 0)

	fake.db = database
	if fake.fixEdit == nil {
		fake.fixEdit = func(i int) { writeTestFile(t, dir, fmt.Sprintf("fix%d.go", i), "package app\n") }
	}
	roundNum := 1
	rc := &repairCoordinator{
		invoker:            fake,
		db:                 database,
		run:                run,
		stepResultID:       step.ID,
		workDir:            dir,
		branch:             run.Branch,
		intent:             "add F",
		baseSHA:            baseSHA,
		producingAttemptID: reviewAttempt,
		policy:             blockingRepairPolicy(config.DefaultRoutingConfig()),
		log:                func(string) {},
		logChunk:           func(string) {},
		reserveRound: func(trigger string) (*db.StepRound, error) {
			roundNum++
			return database.ReserveStepRound(step.ID, roundNum, trigger)
		},
	}
	seeds := make([]repairSeed, len(findings))
	for i := range findings {
		seeds[i] = repairSeed{LineageID: lineages[i].ID, Finding: findings[i]}
	}
	return rc, seeds
}

func blockingFinding(id, desc string) types.Finding {
	return types.Finding{ID: id, Severity: "error", Action: types.ActionAutoFix, Description: desc, File: "app.go", Line: 3}
}

func TestEscalateResolvesAtFixFast(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(_ int, ids []string) verdictSpec {
		return verdictSpec{resolved: allResolved(ids)}
	}}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("review-1", "nil deref")})
	states, err := rc.escalateBatch(context.Background(), seeds)
	if err != nil {
		t.Fatalf("escalateBatch: %v", err)
	}
	if !states[seeds[0].LineageID].resolved {
		t.Fatalf("lineage not resolved: %+v", states[seeds[0].LineageID])
	}
	if len(fake.fixerTiers) != 1 || fake.fixerTiers[0] != 0 {
		t.Fatalf("fixer tiers = %v, want [0]", fake.fixerTiers)
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(seeds[0].LineageID)
	if len(repairs) != 1 || repairs[0].Tier != 0 || repairs[0].Status != db.RepairStatusResolved {
		t.Fatalf("repairs = %+v, want one resolved tier-0", repairs)
	}
}

func TestEscalateAdvancesThenResolves(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(call int, ids []string) verdictSpec {
		if call == 0 {
			return verdictSpec{} // unresolved at fix_fast
		}
		return verdictSpec{resolved: allResolved(ids)} // resolved at fix_balanced
	}}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("review-1", "nil deref")})
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if !states[seeds[0].LineageID].resolved {
		t.Fatal("lineage should resolve at fix_balanced")
	}
	if fmt.Sprint(fake.fixerTiers) != "[0 1]" {
		t.Fatalf("fixer tiers = %v, want [0 1]", fake.fixerTiers)
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(seeds[0].LineageID)
	if len(repairs) != 2 || repairs[0].Tier != 0 || repairs[0].Status != db.RepairStatusUnresolved || repairs[1].Tier != 1 || repairs[1].Status != db.RepairStatusResolved {
		t.Fatalf("repairs = %+v, want tier0 unresolved then tier1 resolved", repairs)
	}
}

func TestEscalateFailsClosedAtAuthority(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(int, []string) verdictSpec { return verdictSpec{} }}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("review-1", "nil deref")})
	states, _ := rc.escalateBatch(context.Background(), seeds)
	st := states[seeds[0].LineageID]
	if !st.failed || st.resolved {
		t.Fatalf("lineage should fail closed, got %+v", st)
	}
	if fmt.Sprint(fake.fixerTiers) != "[0 1 2]" {
		t.Fatalf("fixer tiers = %v, want [0 1 2]", fake.fixerTiers)
	}
	last := fake.verifierPurposes[len(fake.verifierPurposes)-1]
	if last != authorityVerifierPurpose {
		t.Fatalf("final verifier purpose = %q, want authority", last)
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(seeds[0].LineageID)
	if len(repairs) != 3 {
		t.Fatalf("repairs = %d, want 3 tiers", len(repairs))
	}
}

func TestEscalateBatchesAndDropsResolved(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(call int, ids []string) verdictSpec {
		if call == 0 {
			// Resolve the first lineage, leave the second unresolved.
			return verdictSpec{resolved: map[string]bool{ids[0]: true}}
		}
		return verdictSpec{resolved: allResolved(ids)}
	}}
	findings := []types.Finding{blockingFinding("review-1", "bug a"), blockingFinding("review-2", "bug b")}
	rc, seeds := repairFixture(t, fake, findings)
	// seeds[0] is resolved at tier 0; ensure the verify closure resolves the
	// right one by ordering: the batch verify prompt lists lineages in state
	// order, so ids[0] is seeds[0].
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if !states[seeds[0].LineageID].resolved || !states[seeds[1].LineageID].resolved {
		t.Fatalf("both lineages should end resolved: %+v", states)
	}
	a, _ := rc.db.GetFindingRepairsByLineage(seeds[0].LineageID)
	b, _ := rc.db.GetFindingRepairsByLineage(seeds[1].LineageID)
	if len(a) != 1 || a[0].Status != db.RepairStatusResolved {
		t.Fatalf("lineage A repairs = %+v, want one resolved (dropped after tier 0)", a)
	}
	if len(b) != 2 || b[0].Status != db.RepairStatusUnresolved || b[1].Status != db.RepairStatusResolved {
		t.Fatalf("lineage B repairs = %+v, want tier0 unresolved then tier1 resolved", b)
	}
	if fmt.Sprint(fake.fixerTiers) != "[0 1]" {
		t.Fatalf("fixer tiers = %v, want [0 1]", fake.fixerTiers)
	}
}

func TestEscalateCheckFailureAdvancesWithoutVerifier(t *testing.T) {
	checkCalls := 0
	fake := &fakeRepairInvoker{verify: func(_ int, ids []string) verdictSpec { return verdictSpec{resolved: allResolved(ids)} }}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("review-1", "nil deref")})
	rc.checks = []repairCheck{{
		Command: "make test",
		Run: func(context.Context) (bool, int, string) {
			checkCalls++
			if checkCalls == 1 {
				return true, 1, "FAIL" // fail at tier 0 → advance without a verifier
			}
			return true, 0, "PASS" // pass at tier 1 → verifier resolves
		},
	}}
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if !states[seeds[0].LineageID].resolved {
		t.Fatal("lineage should resolve at tier 1 after the tier-0 check failure")
	}
	if len(fake.verifierPurposes) != 1 {
		t.Fatalf("verifier ran %d times, want 1 (tier-0 check failure skips its verifier)", len(fake.verifierPurposes))
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(seeds[0].LineageID)
	if len(repairs) != 2 || repairs[0].Status != db.RepairStatusUnresolved {
		t.Fatalf("repairs = %+v, want tier0 unresolved (check) then tier1 resolved", repairs)
	}
}

func TestEscalatePatchCausedInheritsNextTier(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(call int, ids []string) verdictSpec {
		if call == 0 {
			// The fix resolves the finding but introduces a new blocking issue.
			return verdictSpec{
				resolved:    map[string]bool{ids[0]: true},
				newFindings: []newFindingSpec{{description: "regression from the fix", severity: "error", action: "auto-fix", causedBy: ids[0]}},
			}
		}
		return verdictSpec{resolved: allResolved(ids)}
	}}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("review-1", "nil deref")})
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if !states[seeds[0].LineageID].resolved {
		t.Fatal("lineage should resolve at tier 1 after the patch-caused regression")
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(seeds[0].LineageID)
	if len(repairs) != 2 {
		t.Fatalf("repairs = %d, want 2 (patch-caused finding inherits next tier)", len(repairs))
	}
	if repairs[1].Tier != 1 || repairs[1].Description != "regression from the fix" {
		t.Fatalf("tier-1 repair = %+v, want the patch-caused finding content", repairs[1])
	}
}

func TestEscalateUnrelatedFindingCreatesSeparateRoot(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(call int, ids []string) verdictSpec {
		if call == 0 {
			return verdictSpec{
				resolved:    map[string]bool{ids[0]: true},
				newFindings: []newFindingSpec{{description: "unrelated new bug", severity: "error", action: "auto-fix", causedBy: ""}},
			}
		}
		return verdictSpec{resolved: allResolved(ids)}
	}}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("review-1", "nil deref")})
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if len(states) != 2 {
		t.Fatalf("states = %d, want 2 (original + unrelated new root)", len(states))
	}
	byRun, _ := rc.db.GetFindingRepairsByRun(rc.run.ID)
	descs := map[string]bool{}
	for _, r := range byRun {
		descs[r.Description] = true
	}
	if !descs["nil deref"] || !descs["unrelated new bug"] {
		t.Fatalf("repairs should cover both roots, got %v", descs)
	}
}

func allResolved(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestEscalateInfoPolicyNeverUsesSolFable(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(int, []string) verdictSpec { return verdictSpec{} }}
	rc, seeds := repairFixture(t, fake, []types.Finding{{ID: "review-1", Severity: "info", Action: types.ActionAutoFix, Description: "style nit", File: "app.go", Line: 3}})
	rc.policy = informationalRepairPolicy(config.DefaultRoutingConfig())
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if !states[seeds[0].LineageID].failed {
		t.Fatal("an unresolved info finding should exhaust its two cheap tiers")
	}
	if fmt.Sprint(fake.fixerTiers) != "[0 1]" {
		t.Fatalf("info fixer tiers = %v, want [0 1] (cheap two-tier)", fake.fixerTiers)
	}
	for _, p := range fake.verifierPurposes {
		if p != types.PurposeInformationalRepairVerification {
			t.Fatalf("info verifier = %q, want informational_repair_verification (tools_balanced)", p)
		}
	}
	attempts, _ := rc.db.GetInvocationAttemptsByStepResult(rc.stepResultID)
	for _, a := range attempts {
		if a.Start.Purpose == types.PurposeInitialReview {
			continue // the setup's review runs at review_strong by design
		}
		if p := a.Start.Candidate.Profile; p == "review_strong" || p == "authority_strong" {
			t.Fatalf("info repair used a Sol/Fable profile %q", p)
		}
	}
}

func TestEscalateIntentSensitiveStartsAtFixBalanced(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(_ int, ids []string) verdictSpec { return verdictSpec{resolved: allResolved(ids)} }}
	rc, seeds := repairFixture(t, fake, []types.Finding{{ID: "review-1", Severity: "error", Action: types.ActionAskUser, Description: "intent-sensitive change", File: "app.go", Line: 3}})
	rc.policy = intentSensitiveRepairPolicy(config.DefaultRoutingConfig())
	states, _ := rc.escalateBatch(context.Background(), seeds)
	if !states[seeds[0].LineageID].resolved {
		t.Fatal("a consented intent-sensitive finding should resolve at fix_balanced")
	}
	attempts, _ := rc.db.GetInvocationAttemptsByStepResult(rc.stepResultID)
	var firstFixer *db.InvocationAttempt
	for _, a := range attempts {
		if a.Start.Purpose == types.PurposeIntentSensitiveRepair {
			firstFixer = a
			break
		}
	}
	if firstFixer == nil || firstFixer.Start.Candidate.Profile != "fix_balanced" {
		t.Fatalf("intent-sensitive fixer must start at fix_balanced, got %+v", firstFixer)
	}
}

// TestStepRepairCheckAdvancesWithoutVerifier proves the configured test-command
// re-run gates the verifier: a still-failing check advances the batch to the
// next tier without spending a strong verifier, and a passing check lets the
// verifier resolve the lineage (ticket 11 criterion).
func TestStepRepairCheckAdvancesWithoutVerifier(t *testing.T) {
	fake := &fakeRepairInvoker{verify: func(_ int, ids []string) verdictSpec {
		return verdictSpec{resolved: allResolved(ids)}
	}}
	rc, seeds := repairFixture(t, fake, []types.Finding{blockingFinding("test-1", "tests failed")})
	rc.stepName = types.StepTest
	rc.policy = unstructuredTestRepairPolicy(config.DefaultRoutingConfig())
	checkCalls := 0
	rc.checks = []repairCheck{{
		Command: "make test",
		Run: func(context.Context) (bool, int, string) {
			checkCalls++
			if checkCalls == 1 {
				return true, 1, "still failing"
			}
			return true, 0, "green"
		},
	}}
	states, err := rc.escalateBatch(context.Background(), seeds)
	if err != nil {
		t.Fatalf("escalateBatch: %v", err)
	}
	// fix_balanced (tier 0) check fails -> advance without verifier;
	// authority_strong (tier 1) check passes -> verifier resolves.
	if len(fake.fixerTiers) != 2 || fake.fixerTiers[0] != 0 || fake.fixerTiers[1] != 1 {
		t.Fatalf("fixer tiers = %v, want [0 1] (fix_balanced then authority_strong)", fake.fixerTiers)
	}
	if fake.verifyCalls != 1 {
		t.Fatalf("verify calls = %d, want 1 (no verifier at the tier where the check failed)", fake.verifyCalls)
	}
	if !states[seeds[0].LineageID].resolved {
		t.Fatalf("lineage not resolved after the check passed: %+v", states[seeds[0].LineageID])
	}
}

// TestUnstructuredTestRepairPolicyStartsAtFixBalanced pins the test repair route
// to fix_balanced → authority_strong and confirms the purpose is routed.
func TestUnstructuredTestRepairPolicyStartsAtFixBalanced(t *testing.T) {
	policy, ok := stepRepairPolicyFor(config.DefaultRoutingConfig(), types.StepTest)
	if !ok {
		t.Fatal("expected a repair policy for the Test step")
	}
	if policy.fixerPurpose != types.PurposeUnstructuredTestRepair {
		t.Fatalf("test fixer purpose = %q, want unstructured_test_repair", policy.fixerPurpose)
	}
	profiles, err := config.DefaultRoutingConfig().ResolveRoute(types.PurposeUnstructuredTestRepair)
	if err != nil || len(profiles) != 2 || profiles[0].Name != config.ProfileFixBalanced || profiles[1].Name != config.ProfileAuthorityStrong {
		t.Fatalf("test repair route = %+v (err %v), want [fix_balanced authority_strong]", profiles, err)
	}
	if policy.maxTier != 1 {
		t.Fatalf("test repair maxTier = %d, want 1", policy.maxTier)
	}
}

// TestLintRepairUsesStructuredPolicyAndRoutes confirms no-command lint
// inspection is routed and blocking lint findings take the structured repair
// cascade with a strong verifier (ticket 12).
func TestLintRepairUsesStructuredPolicyAndRoutes(t *testing.T) {
	policy, ok := stepRepairPolicyFor(config.DefaultRoutingConfig(), types.StepLint)
	if !ok {
		t.Fatal("expected a repair policy for the Lint step")
	}
	if policy.fixerPurpose != types.PurposeStructuredFindingRepair {
		t.Fatalf("lint fixer purpose = %q, want structured_finding_repair", policy.fixerPurpose)
	}
	if policy.verifierPurpose != types.PurposeNormalAggregateVerification {
		t.Fatalf("lint verifier purpose = %q, want a strong verifier", policy.verifierPurpose)
	}
}

// TestDocumentationRepairUsesVerifierAndRoutes confirms doc authoring and its
// independent verifier are routed and paired in the document policy (ticket 13).
func TestDocumentationRepairUsesVerifierAndRoutes(t *testing.T) {
	policy, ok := stepRepairPolicyFor(config.DefaultRoutingConfig(), types.StepDocument)
	if !ok {
		t.Fatal("expected a repair policy for the Document step")
	}
	if policy.fixerPurpose != types.PurposeDocumentationAuthoring {
		t.Fatalf("doc fixer = %q, want documentation_authoring", policy.fixerPurpose)
	}
	if policy.verifierPurpose != types.PurposeDocumentationVerification || policy.finalVerifierPurpose != types.PurposeDocumentationVerification {
		t.Fatalf("doc verifier = %q/%q, want documentation_verification", policy.verifierPurpose, policy.finalVerifierPurpose)
	}
}

// TestVerifyRepairRoutesThroughStructuredCascade confirms Verify's aggregate
// findings repair through the structured cascade with a strong verifier (ticket 17).
func TestVerifyRepairRoutesThroughStructuredCascade(t *testing.T) {
	policy, ok := stepRepairPolicyFor(config.DefaultRoutingConfig(), types.StepVerify)
	if !ok {
		t.Fatal("expected a repair policy for the Verify step")
	}
	if policy.fixerPurpose != types.PurposeStructuredFindingRepair {
		t.Fatalf("verify fixer purpose = %q, want structured_finding_repair", policy.fixerPurpose)
	}
	if policy.finalVerifierPurpose != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("verify final verifier = %q, want escalated aggregate verification", policy.finalVerifierPurpose)
	}
}
