package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// authorityVerifierPurpose routes to authority_strong (Sol/Fable-xhigh); the
// final-tier fixer can succeed only after a fresh invocation of it.
const authorityVerifierPurpose = types.PurposeEscalatedAggregateVerification

// repairPolicy parameterizes the escalation cascade for one severity/action
// class. Blocking policies gate the pipeline until resolved; the informational
// policy never blocks and, routing only through fix_fast and tools_balanced,
// never reaches a Sol/Fable profile.
type repairPolicy struct {
	fixerPurpose         types.Purpose
	verifierPurpose      types.Purpose // strong verifier below the final tier
	finalVerifierPurpose types.Purpose // verifier at the final tier
	blocking             bool
	maxTier              int
}

func routeMaxTier(routing config.RoutingConfig, purpose types.Purpose) int {
	profiles, err := routing.ResolveRoute(purpose)
	if err != nil || len(profiles) == 0 {
		return 0
	}
	return len(profiles) - 1
}

// blockingRepairPolicy repairs error/warning auto-fix findings through the full
// fix_fast → fix_balanced → authority_strong cascade with a strong verifier.
func blockingRepairPolicy(routing config.RoutingConfig) repairPolicy {
	return repairPolicy{
		fixerPurpose:         types.PurposeStructuredFindingRepair,
		verifierPurpose:      types.PurposeNormalAggregateVerification,
		finalVerifierPurpose: types.PurposeEscalatedAggregateVerification,
		blocking:             true,
		maxTier:              routeMaxTier(routing, types.PurposeStructuredFindingRepair),
	}
}

// informationalRepairPolicy repairs info findings with the cheap two-tier
// fix_fast → tools_balanced cascade and a tools_balanced verifier; it never
// invokes a Sol/Fable profile and never blocks the gate.
func informationalRepairPolicy(routing config.RoutingConfig) repairPolicy {
	return repairPolicy{
		fixerPurpose:         types.PurposeInformationalRepair,
		verifierPurpose:      types.PurposeInformationalRepairVerification,
		finalVerifierPurpose: types.PurposeInformationalRepairVerification,
		blocking:             false,
		maxTier:              routeMaxTier(routing, types.PurposeInformationalRepair),
	}
}

// intentSensitiveRepairPolicy repairs consented ask-user findings starting at
// fix_balanced and escalating to authority_strong.
func intentSensitiveRepairPolicy(routing config.RoutingConfig) repairPolicy {
	return repairPolicy{
		fixerPurpose:         types.PurposeIntentSensitiveRepair,
		verifierPurpose:      types.PurposeNormalAggregateVerification,
		finalVerifierPurpose: types.PurposeEscalatedAggregateVerification,
		blocking:             true,
		maxTier:              routeMaxTier(routing, types.PurposeIntentSensitiveRepair),
	}
}

// unstructuredTestRepairPolicy repairs a failed configured test (or an
// unstructured test-log failure) through fix_balanced → authority_strong. The
// deterministic test-command re-run is the primary gate: a still-failing check
// advances the batch without spending a strong verifier.
func unstructuredTestRepairPolicy(routing config.RoutingConfig) repairPolicy {
	return repairPolicy{
		fixerPurpose:         types.PurposeUnstructuredTestRepair,
		verifierPurpose:      types.PurposeNormalAggregateVerification,
		finalVerifierPurpose: types.PurposeEscalatedAggregateVerification,
		blocking:             true,
		maxTier:              routeMaxTier(routing, types.PurposeUnstructuredTestRepair),
	}
}

// documentationRepairPolicy resolves documentation-authoring findings: a
// prose_fast author (fixer) closes doc gaps and a fresh tools_balanced
// documentation verifier adjudicates accuracy and completeness. The author
// route is single-tier, so an authoring-caused defect advances the lineage and
// fails closed rather than restarting on a fresh author budget.
func documentationRepairPolicy(routing config.RoutingConfig) repairPolicy {
	return repairPolicy{
		fixerPurpose:         types.PurposeDocumentationAuthoring,
		verifierPurpose:      types.PurposeDocumentationVerification,
		finalVerifierPurpose: types.PurposeDocumentationVerification,
		blocking:             true,
		maxTier:              routeMaxTier(routing, types.PurposeDocumentationAuthoring),
	}
}

// stepRepairPolicyFor returns the repair policy for a non-review step whose
// blocking findings route through the common coordinator, and whether such a
// policy exists. Steps without a routed repair keep their legacy path.
func stepRepairPolicyFor(routing config.RoutingConfig, stepName types.StepName) (repairPolicy, bool) {
	switch stepName {
	case types.StepTest:
		return unstructuredTestRepairPolicy(routing), true
	case types.StepLint:
		// Structured lint repair uses the approved structured cascade
		// (fix_fast → fix_balanced → authority_strong) with a strong verifier.
		return blockingRepairPolicy(routing), true
	case types.StepDocument:
		return documentationRepairPolicy(routing), true
	case types.StepVerify:
		// Verify's aggregate findings repair through the structured cascade
		// (fix_fast → fix_balanced → authority_strong) with a strong aggregate verifier.
		return blockingRepairPolicy(routing), true
	default:
		return repairPolicy{}, false
	}
}

// batchVerdictSchema is the strong verifier's per-lineage adjudication of a
// batch plus any new findings the fix introduced or exposed.
var batchVerdictSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"verdicts": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"lineage_id": {"type": "string"},
					"status": {"type": "string", "enum": ["resolved", "unresolved", "inconclusive"]},
					"rationale": {"type": "string"}
				},
				"required": ["lineage_id", "status", "rationale"]
			}
		},
		"new_findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"description": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]},
					"caused_by_lineage_id": {"type": "string"}
				},
				"required": ["description", "severity", "action", "caused_by_lineage_id"]
			}
		}
	},
	"required": ["verdicts", "new_findings"]
}`)

type batchVerdict struct {
	Verdicts []struct {
		LineageID string `json:"lineage_id"`
		Status    string `json:"status"`
		Rationale string `json:"rationale"`
	} `json:"verdicts"`
	NewFindings []struct {
		Description       string `json:"description"`
		Severity          string `json:"severity"`
		Action            string `json:"action"`
		CausedByLineageID string `json:"caused_by_lineage_id"`
	} `json:"new_findings"`
}

// lineageState tracks one blocking root lineage through the escalation cascade.
type lineageState struct {
	lineageID string
	finding   types.Finding
	tier      int
	resolved  bool
	failed    bool
	verdict   string
	rationale string
}

// repairSeed is a blocking root finding entering the escalation cascade.
type repairSeed struct {
	LineageID string
	Finding   types.Finding
}

// escalateBatch drives blocking lineages through fix_fast → fix_balanced →
// authority_strong. At each tier the still-unresolved batch is fixed together
// by one fresh fixer, applicable deterministic checks run, and (unless a check
// failed) one fresh strong verifier adjudicates every lineage. Resolved
// lineages drop out; unresolved ones advance a tier until the budget is spent,
// then fail closed. It returns the terminal state of every root lineage
// (including patch-caused and unrelated roots the verifier surfaced).
func (rc *repairCoordinator) escalateBatch(ctx context.Context, seeds []repairSeed) (map[string]*lineageState, error) {
	states := make([]*lineageState, 0, len(seeds))
	byLineage := make(map[string]*lineageState)
	for _, s := range seeds {
		st := &lineageState{lineageID: s.LineageID, finding: s.Finding}
		states = append(states, st)
		byLineage[st.lineageID] = st
	}

	// A generous cap bounds pathological verifier loops (each fix exposing new
	// unrelated roots) without truncating legitimate escalation.
	maxIterations := (rc.policy.maxTier + 1) * (len(seeds) + 8)
	for i := 0; i < maxIterations; i++ {
		batch, tier := rc.lowestActiveTier(states)
		if len(batch) == 0 {
			break
		}
		newRoots, err := rc.runTierBatch(ctx, batch, tier)
		if err != nil {
			return byLineage, err
		}
		for _, st := range newRoots {
			states = append(states, st)
			byLineage[st.lineageID] = st
		}
	}
	active := unresolvedStates(states)
	if len(active) == 0 {
		return byLineage, nil
	}
	if err := rc.persistIterationCap(active); err != nil {
		return byLineage, err
	}
	return byLineage, fmt.Errorf("repair iteration cap reached with %d unresolved lineage(s)", len(active))
}

// lowestActiveTier returns the active (unresolved, unfailed) states sharing the
// lowest current tier, so escalation processes one tier of one batch at a time.
func (rc *repairCoordinator) lowestActiveTier(states []*lineageState) ([]*lineageState, int) {
	tier := -1
	for _, st := range states {
		if st.resolved || st.failed {
			continue
		}
		if tier == -1 || st.tier < tier {
			tier = st.tier
		}
	}
	if tier == -1 {
		return nil, 0
	}
	var batch []*lineageState
	for _, st := range states {
		if !st.resolved && !st.failed && st.tier == tier {
			batch = append(batch, st)
		}
	}
	return batch, tier
}

func unresolvedStates(states []*lineageState) []*lineageState {
	active := make([]*lineageState, 0, len(states))
	for _, st := range states {
		if !st.resolved && !st.failed {
			active = append(active, st)
		}
	}
	return active
}

func (rc *repairCoordinator) persistIterationCap(active []*lineageState) error {
	round, err := rc.reserveRound("repair_exhausted")
	if err != nil {
		return fmt.Errorf("reserve iteration-cap round: %w", err)
	}
	started := time.Now()
	abort := func(cause error) error {
		if roundErr := rc.db.TerminateReservedStepRound(round.ID, db.StepRoundFailed, time.Since(started).Milliseconds()); roundErr != nil {
			return errors.Join(cause, fmt.Errorf("terminate iteration-cap round: %w", roundErr))
		}
		return cause
	}
	for _, st := range active {
		remaining := rc.policy.maxTier - st.tier
		if remaining < 0 {
			remaining = 0
		}
		id, err := rc.db.StartFindingRepair(db.FindingRepairStart{
			RunID: rc.run.ID, LineageID: st.lineageID, StepResultID: rc.stepResultID, StepRoundID: round.ID,
			Severity: st.finding.Severity, Action: st.finding.Action, Description: st.finding.Description,
			File: st.finding.File, Line: st.finding.Line, Tier: st.tier, RemainingBudget: remaining,
		})
		if err != nil {
			return abort(fmt.Errorf("persist iteration-cap repair: %w", err))
		}
		st.failed = true
		st.verdict = db.RepairVerdictInconclusive
		st.rationale = "repair iteration cap reached"
		if err := rc.db.ResolveFindingRepair(id, st.verdict, st.rationale, db.RepairStatusUnresolved); err != nil {
			return abort(fmt.Errorf("resolve iteration-cap repair: %w", err))
		}
	}
	if err := rc.db.CompleteReservedStepRound(round.ID, nil, nil, time.Since(started).Milliseconds()); err != nil {
		return abort(fmt.Errorf("complete iteration-cap round: %w", err))
	}
	return nil
}

func (rc *repairCoordinator) runTierBatch(ctx context.Context, batch []*lineageState, tier int) ([]*lineageState, error) {
	round, err := rc.reserveRound("auto_fix")
	if err != nil {
		return nil, fmt.Errorf("reserve repair round: %w", err)
	}
	scope := types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: rc.run.ID, StepResultID: rc.stepResultID, StepRoundID: round.ID}
	started := time.Now()
	remaining := rc.policy.maxTier - tier
	var roundFindings *string
	abortRound := func(cause error) error {
		roundErr := rc.db.TerminateReservedStepRound(round.ID, db.StepRoundFailed, time.Since(started).Milliseconds())
		if roundErr != nil {
			return errors.Join(cause, fmt.Errorf("terminate failed repair round: %w", roundErr))
		}
		return cause
	}
	completeRound := func(summary string) error {
		if err := rc.db.CompleteReservedStepRound(round.ID, roundFindings, ptrOrNil(summary), time.Since(started).Milliseconds()); err != nil {
			return abortRound(fmt.Errorf("complete repair round: %w", err))
		}
		return nil
	}

	repairID := make(map[string]string, len(batch))
	for _, st := range batch {
		id, err := rc.db.StartFindingRepair(db.FindingRepairStart{
			RunID: rc.run.ID, LineageID: st.lineageID, StepResultID: rc.stepResultID, StepRoundID: round.ID,
			Severity: st.finding.Severity, Action: st.finding.Action, Description: st.finding.Description,
			File: st.finding.File, Line: st.finding.Line, Tier: tier, RemainingBudget: remaining,
		})
		if err != nil {
			return nil, abortRound(fmt.Errorf("persist finding repair: %w", err))
		}
		repairID[st.lineageID] = id
	}

	advance := func(st *lineageState, verdict, rationale string) error {
		if strings.TrimSpace(verdict) == "" {
			verdict = db.RepairVerdictInconclusive
		}
		if tier >= rc.policy.maxTier {
			st.failed = true
		} else {
			st.tier++
		}
		st.verdict, st.rationale = verdict, rationale
		if err := rc.db.ResolveFindingRepair(repairID[st.lineageID], verdict, rationale, db.RepairStatusUnresolved); err != nil {
			return err
		}
		return nil
	}
	failBatch := func(verdict, rationale string) error {
		for _, st := range batch {
			st.failed = true
			st.verdict, st.rationale = verdict, rationale
			if err := rc.db.ResolveFindingRepair(repairID[st.lineageID], verdict, rationale, db.RepairStatusUnresolved); err != nil {
				return err
			}
		}
		return nil
	}

	diff := rc.reviewDiff(ctx, rc.baseSHA)
	rc.logf("repairing %d finding(s) at tier %d with a fresh fixer...", len(batch), tier)
	fixResult, fixErr := rc.invoker.Invoke(ctx, agent.InvocationRequest{
		Purpose: rc.policy.fixerPurpose, Tier: tier, Scope: scope,
		Payload: agent.RunOpts{Prompt: buildBatchFixPrompt(batch, rc.intent, remaining, diff), CWD: rc.workDir, JSONSchema: commitSummarySchemaJSON, OnChunk: rc.logChunk},
	})
	if fixErr != nil {
		rc.logf("fixer failed at tier %d: %v", tier, fixErr)
		if err := failBatch(db.RepairVerdictInconclusive, "fixer invocation failed"); err != nil {
			return nil, abortRound(fmt.Errorf("record failed fixer outcome: %w", err))
		}
		if err := completeRound(""); err != nil {
			return nil, err
		}
		var unavailable *agent.ProfileUnavailableError
		if errors.As(fixErr, &unavailable) {
			return nil, fmt.Errorf("repair fixer profile unavailable: %w", fixErr)
		}
		return nil, nil
	}
	fixerAttemptID, err := rc.succeededAttemptID(round.ID, rc.policy.fixerPurpose)
	if err != nil {
		return nil, abortRound(fmt.Errorf("load fixer attempt: %w", err))
	}
	if fixerAttemptID == "" {
		return nil, abortRound(fmt.Errorf("successful fixer invocation did not journal a succeeded attempt"))
	}
	for _, st := range batch {
		if err := rc.db.SetFindingRepairFixer(repairID[st.lineageID], fixerAttemptID); err != nil {
			return nil, abortRound(fmt.Errorf("link finding repair fixer: %w", err))
		}
	}

	summary := extractRepairSummary(fixResult)
	if err := rc.commitFix(ctx, summary); err != nil {
		return nil, abortRound(fmt.Errorf("commit repair: %w", err))
	}

	for _, check := range rc.checks {
		applicable, exitCode, output := check.Run(ctx)
		for _, st := range batch {
			if err := rc.db.RecordFindingRepairCheck(repairID[st.lineageID], check.Command, applicable, exitCode, output); err != nil {
				return nil, abortRound(fmt.Errorf("persist finding repair check: %w", err))
			}
		}
		if applicable && exitCode != 0 {
			rc.logf("deterministic check failed (%s); advancing the batch without a verifier", check.Command)
			for _, st := range batch {
				if err := advance(st, db.RepairVerdictUnresolved, fmt.Sprintf("deterministic check failed: %s", check.Command)); err != nil {
					return nil, abortRound(fmt.Errorf("record failed deterministic check: %w", err))
				}
			}
			if err := completeRound(summary); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}

	vpurpose := rc.policy.verifierPurpose
	if tier >= rc.policy.maxTier {
		vpurpose = rc.policy.finalVerifierPurpose
	}
	rc.logf("verifying the batch with a fresh strong reviewer...")
	verifyResult, verifyErr := rc.invoker.Invoke(ctx, agent.InvocationRequest{
		Purpose: vpurpose, Scope: scope,
		Payload: agent.RunOpts{Prompt: buildBatchVerifyPrompt(batch, rc.reviewDiff(ctx, rc.baseSHA)), CWD: rc.workDir, JSONSchema: batchVerdictSchema, OnChunk: rc.logChunk},
	})
	if verifyErr != nil {
		rc.logf("verifier failed at tier %d: %v", tier, verifyErr)
		if err := failBatch(db.RepairVerdictInconclusive, "verifier invocation failed"); err != nil {
			return nil, abortRound(fmt.Errorf("record failed verifier outcome: %w", err))
		}
		if err := completeRound(summary); err != nil {
			return nil, err
		}
		var unavailable *agent.ProfileUnavailableError
		if errors.As(verifyErr, &unavailable) {
			return nil, fmt.Errorf("repair verifier profile unavailable: %w", verifyErr)
		}
		return nil, nil
	}
	verifierAttemptID, err := rc.succeededAttemptID(round.ID, vpurpose)
	if err != nil {
		return nil, abortRound(fmt.Errorf("load verifier attempt: %w", err))
	}
	if verifierAttemptID == "" {
		return nil, abortRound(fmt.Errorf("successful verifier invocation did not journal a succeeded attempt"))
	}
	for _, st := range batch {
		if err := rc.db.SetFindingRepairVerifier(repairID[st.lineageID], verifierAttemptID); err != nil {
			return nil, abortRound(fmt.Errorf("link finding repair verifier: %w", err))
		}
	}

	bv, ok := parseBatchVerdict(verifyResult)
	if !ok {
		for _, st := range batch {
			if err := advance(st, db.RepairVerdictInconclusive, "malformed batch adjudication"); err != nil {
				return nil, abortRound(fmt.Errorf("record malformed adjudication: %w", err))
			}
		}
		if err := completeRound(summary); err != nil {
			return nil, err
		}
		return nil, nil
	}
	verdicts, valid := validateBatchVerdicts(batch, bv)
	if !valid {
		for _, st := range batch {
			if err := advance(st, db.RepairVerdictInconclusive, "batch adjudication did not contain exactly one verdict for every requested lineage"); err != nil {
				return nil, abortRound(fmt.Errorf("record inconclusive adjudication: %w", err))
			}
		}
		if err := completeRound(summary); err != nil {
			return nil, err
		}
		return nil, nil
	}

	patchCaused := make(map[string]types.Finding)
	consentRequired := make(map[string]types.Finding)
	batchByLineage := make(map[string]*lineageState, len(batch))
	for _, state := range batch {
		batchByLineage[state.lineageID] = state
	}
	surfaced := make([]types.Finding, 0, len(bv.NewFindings))
	var newRoots []*lineageState
	for _, nf := range bv.NewFindings {
		f := types.Finding{Severity: nf.Severity, Action: nf.Action, Description: nf.Description}
		if _, isRoot := repairID[nf.CausedByLineageID]; isRoot && nf.CausedByLineageID != "" {
			f.ID = batchByLineage[nf.CausedByLineageID].finding.ID
			surfaced = append(surfaced, f)
			switch nf.Action {
			case types.ActionAutoFix:
				patchCaused[nf.CausedByLineageID] = f
			case types.ActionAskUser:
				consentRequired[nf.CausedByLineageID] = f
			case types.ActionNoOp:
			}
			continue
		}
		root, err := rc.newUnrelatedRoot(f, verifierAttemptID)
		if err != nil {
			return nil, abortRound(err)
		}
		surfaced = append(surfaced, root.finding)
		switch nf.Action {
		case types.ActionAutoFix:
		case types.ActionAskUser:
			root.failed = true
			root.verdict = db.RepairVerdictUnresolved
			root.rationale = "verifier-created finding requires consent"
			id, err := rc.db.StartFindingRepair(db.FindingRepairStart{
				RunID: rc.run.ID, LineageID: root.lineageID, StepResultID: rc.stepResultID, StepRoundID: round.ID,
				Severity: f.Severity, Action: f.Action, Description: f.Description, Tier: tier, RemainingBudget: remaining,
			})
			if err != nil {
				return nil, abortRound(fmt.Errorf("persist consent-required finding: %w", err))
			}
			if err := rc.db.ResolveFindingRepair(id, root.verdict, root.rationale, db.RepairStatusUnresolved); err != nil {
				return nil, abortRound(fmt.Errorf("record consent-required finding: %w", err))
			}
		case types.ActionNoOp:
			root.resolved = true
		default:
			root.failed = true
			root.verdict = db.RepairVerdictInconclusive
			root.rationale = "verifier returned an unknown finding action"
		}
		newRoots = append(newRoots, root)
	}
	if len(surfaced) > 0 {
		raw, err := types.MarshalFindingsJSON(types.Findings{Items: surfaced, Summary: fmt.Sprintf("%d verifier-created finding(s)", len(surfaced))})
		if err != nil {
			return nil, abortRound(fmt.Errorf("marshal verifier-created findings: %w", err))
		}
		roundFindings = &raw
	}

	for _, st := range batch {
		if finding, needsConsent := consentRequired[st.lineageID]; needsConsent {
			st.finding = finding
			st.failed = true
			st.verdict = db.RepairVerdictUnresolved
			st.rationale = "patch introduced a finding that requires consent"
			if err := rc.db.ResolveFindingRepair(repairID[st.lineageID], st.verdict, st.rationale, db.RepairStatusUnresolved); err != nil {
				return nil, abortRound(fmt.Errorf("record consent-required patch finding: %w", err))
			}
			continue
		}
		if pf, caused := patchCaused[st.lineageID]; caused {
			if tier < rc.policy.maxTier {
				st.finding = pf
			}
			if err := advance(st, db.RepairVerdictUnresolved, "patch introduced a new auto-fix issue under this lineage"); err != nil {
				return nil, abortRound(fmt.Errorf("record patch-caused finding: %w", err))
			}
			continue
		}
		v := verdicts[st.lineageID]
		if v.status == db.RepairVerdictResolved && strings.TrimSpace(v.rationale) != "" {
			st.resolved = true
			st.verdict, st.rationale = db.RepairVerdictResolved, v.rationale
			if err := rc.db.ResolveFindingRepair(repairID[st.lineageID], db.RepairVerdictResolved, v.rationale, db.RepairStatusResolved); err != nil {
				return nil, abortRound(fmt.Errorf("record resolved finding repair: %w", err))
			}
			continue
		}
		if err := advance(st, v.status, v.rationale); err != nil {
			return nil, abortRound(fmt.Errorf("record unresolved finding repair: %w", err))
		}
	}
	if err := completeRound(summary); err != nil {
		return nil, err
	}
	return newRoots, nil
}

type batchLineVerdict struct {
	status    string
	rationale string
}

// validateBatchVerdicts requires an exact one-to-one adjudication of the
// requested batch. Duplicate, unknown, or missing lineage IDs make the entire
// verdict inconclusive: accepting a partial or ambiguous answer could silently
// approve a blocking finding.
func validateBatchVerdicts(batch []*lineageState, bv batchVerdict) (map[string]batchLineVerdict, bool) {
	requested := make(map[string]struct{}, len(batch))
	for _, st := range batch {
		requested[st.lineageID] = struct{}{}
	}
	if len(bv.Verdicts) != len(requested) {
		return nil, false
	}
	verdicts := make(map[string]batchLineVerdict, len(bv.Verdicts))
	for _, v := range bv.Verdicts {
		if _, ok := requested[v.LineageID]; !ok {
			return nil, false
		}
		if _, duplicate := verdicts[v.LineageID]; duplicate {
			return nil, false
		}
		switch v.Status {
		case db.RepairVerdictResolved, db.RepairVerdictUnresolved, db.RepairVerdictInconclusive:
		default:
			return nil, false
		}
		verdicts[v.LineageID] = batchLineVerdict{status: v.Status, rationale: v.Rationale}
	}
	return verdicts, true
}

// newUnrelatedRoot mints a fresh run-wide root lineage for an unrelated finding
// the verifier surfaced, so it is tracked independently rather than folded into
// an existing lineage.
func (rc *repairCoordinator) newUnrelatedRoot(f types.Finding, producingAttemptID string) (*lineageState, error) {
	lineages, err := rc.db.CreateFindingLineages(rc.run.ID, producingAttemptID, []string{""})
	if err != nil {
		return nil, fmt.Errorf("persist verifier-created finding lineage: %w", err)
	}
	if len(lineages) != 1 {
		return nil, fmt.Errorf("persist verifier-created finding lineage: created %d roots, want 1", len(lineages))
	}
	f.ID = lineages[0].DisplayID
	return &lineageState{lineageID: lineages[0].ID, finding: f}, nil
}

func parseBatchVerdict(result *agent.Result) (batchVerdict, bool) {
	if result == nil || result.Output == nil {
		return batchVerdict{}, false
	}
	var bv batchVerdict
	if err := json.Unmarshal(result.Output, &bv); err != nil {
		return batchVerdict{}, false
	}
	return bv, true
}

func isBlockingSeverity(severity string) bool {
	return severity == "error" || severity == "warning"
}

func buildBatchFixPrompt(batch []*lineageState, intent string, remaining int, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix the following %d code-review finding(s). Apply the smallest correct change for each and nothing unrelated.\n\n", len(batch))
	for _, st := range batch {
		loc := ""
		if st.finding.File != "" {
			loc = " (" + st.finding.File
			if st.finding.Line > 0 {
				loc += fmt.Sprintf(":%d", st.finding.Line)
			}
			loc += ")"
		}
		fmt.Fprintf(&b, "- lineage %s, severity %s%s: %s\n", st.lineageID, st.finding.Severity, loc, st.finding.Description)
	}
	in := strings.TrimSpace(intent)
	if in == "" {
		in = "(no recorded intent)"
	}
	fmt.Fprintf(&b, "\nUser intent for the change under review:\n%s\n\nRemaining repair budget: %d escalation tier(s) after this attempt.\n\nDiff currently under review:\n%s\n\nReturn a one-line commit summary as {\"summary\": \"<what you changed>\"}.", in, remaining, diff)
	return b.String()
}

func buildBatchVerifyPrompt(batch []*lineageState, diff string) string {
	var b strings.Builder
	b.WriteString("Independently verify whether each of the following code-review findings has been resolved by the latest changes. You did not write the fix; judge it fresh.\n\n")
	for _, st := range batch {
		fmt.Fprintf(&b, "- lineage %s, severity %s: %s\n", st.lineageID, st.finding.Severity, st.finding.Description)
	}
	b.WriteString("\nChanges to adjudicate:\n")
	b.WriteString(diff)
	b.WriteString("\n\nReturn a JSON object with:\n")
	b.WriteString("- \"verdicts\": one entry per lineage above, {\"lineage_id\", \"status\": \"resolved\"|\"unresolved\"|\"inconclusive\", \"rationale\"}. Use the exact lineage_id values given.\n")
	b.WriteString("- \"new_findings\": any new blocking issue the changes introduced or exposed, {\"description\", \"severity\", \"action\", \"caused_by_lineage_id\"}. Set caused_by_lineage_id to the lineage whose fix caused it, or \"\" when unrelated.\n")
	b.WriteString("Only an explicit \"resolved\" verdict with a rationale counts as resolved; when unsure, use \"unresolved\" or \"inconclusive\".")
	return b.String()
}
