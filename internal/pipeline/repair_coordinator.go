package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// repairCheck is one deterministic check the coordinator runs before the strong
// verifier. Run reports whether the check applies to this repair; an
// inapplicable check is recorded but never blocks the verifier.
type repairCheck struct {
	Command string
	Run     func(ctx context.Context) (applicable bool, exitCode int, output string)
}

// repairCoordinator resolves blocking finding lineages through fresh fixers,
// fresh strong verifiers, and the deterministic checks between them, escalating
// unresolved lineages through the provider-routed quality cascade. Fixer and
// verifier are separate journaled invocations; every non-resolved outcome is
// recorded and either escalates or fails closed.
type repairCoordinator struct {
	invoker            agent.Invoker
	db                 *db.DB
	run                *db.Run
	stepResultID       string
	stepName           types.StepName
	workDir            string
	branch             string
	defaultBranch      string
	intent             string
	baseSHA            string
	producingAttemptID string
	policy             repairPolicy
	checks             []repairCheck
	log                func(string)
	logChunk           func(string)
	reserveRound       func(trigger string) (*db.StepRound, error)
}

// fixerPurpose is the routed Purpose whose first tier is fix_fast (Luna/Sonnet).
const fixerPurpose = types.PurposeStructuredFindingRepair

// verifierPurpose is the routed Purpose whose route is review_strong; it
// adjudicates a repaired lineage with a fresh strong invocation.
const verifierPurpose = types.PurposeNormalAggregateVerification

// succeededAttemptID returns the id of the latest succeeded attempt for a
// purpose in a round, so the coordinator can link the fixer/verifier it drove.
func (rc *repairCoordinator) succeededAttemptID(roundID string, purpose types.Purpose) (string, error) {
	attempts, err := rc.db.GetInvocationAttemptsByRound(roundID)
	if err != nil {
		return "", err
	}
	id := ""
	for _, attempt := range attempts {
		if attempt.Start.Purpose == purpose && attempt.Terminal != nil && attempt.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			id = attempt.ID
		}
	}
	return id, nil
}

// reviewDiff returns the changed-code diff for the fixer and verifier prompts.
// It resolves a usable base defensively: the run's recorded base, then the
// merge-base with the default branch, then the last commit. Diff context is
// helpful but not load-bearing, so an unresolvable base yields an empty diff
// rather than aborting the repair.
func (rc *repairCoordinator) reviewDiff(ctx context.Context, baseSHA string) string {
	if baseSHA != "" {
		if diff, err := git.Diff(ctx, rc.workDir, baseSHA, "HEAD"); err == nil {
			return diff
		}
	}
	if rc.defaultBranch != "" {
		if base, err := git.Run(ctx, rc.workDir, "merge-base", "HEAD", rc.defaultBranch); err == nil {
			if diff, err := git.Diff(ctx, rc.workDir, strings.TrimSpace(base), "HEAD"); err == nil {
				return diff
			}
		}
	}
	if diff, err := git.Diff(ctx, rc.workDir, "HEAD~1", "HEAD"); err == nil {
		return diff
	}
	return ""
}

func (rc *repairCoordinator) commitFix(ctx context.Context, summary string) error {
	status, _ := git.Run(ctx, rc.workDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		rc.log("fixer produced no changes to commit")
		return nil
	}
	if _, err := git.Run(ctx, rc.workDir, "add", "-A"); err != nil {
		return fmt.Errorf("stage repair changes: %w", err)
	}
	if summary == "" {
		summary = "apply review finding repair"
	}
	step := rc.stepName
	if step == "" {
		step = types.StepReview
	}
	message := fmt.Sprintf("no-mistakes(%s): %s", step, summary)
	if _, err := git.Run(ctx, rc.workDir, "commit", "-m", message); err != nil {
		return fmt.Errorf("commit repair changes: %w", err)
	}
	headSHA, err := git.HeadSHA(ctx, rc.workDir)
	if err != nil {
		return fmt.Errorf("resolve head after repair commit: %w", err)
	}
	if _, err := git.Run(ctx, rc.workDir, "update-ref", branchRef(rc.branch), headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	rc.run.HeadSHA = headSHA
	if err := rc.db.UpdateRunHeadSHA(rc.run.ID, headSHA); err != nil {
		return err
	}
	return nil
}

func (rc *repairCoordinator) logf(format string, args ...any) {
	if rc.log != nil {
		rc.log(fmt.Sprintf(format, args...))
	}
}

func extractRepairSummary(result *agent.Result) string {
	if result == nil || result.Output == nil {
		return ""
	}
	var s commitSummaryJSON
	if err := json.Unmarshal(result.Output, &s); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(s.Summary), " ")
}

func branchRef(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// commitSummarySchemaJSON constrains the fixer to a one-line commit summary,
// mirroring the schema the legacy fix path uses.
var commitSummarySchemaJSON = json.RawMessage(`{
	"type": "object",
	"properties": {"summary": {"type": "string"}},
	"required": ["summary"]
}`)

type commitSummaryJSON struct {
	Summary string `json:"summary"`
}

// maybeRepairReviewFinding routes review findings through the repair coordinator
// before the executor falls through to the approval gate, applying the severity
// policy: blocking (error/warning) auto-fix findings escalate through the full
// cascade, informational auto-fix findings take the cheap non-blocking cascade,
// no-op findings are never repaired, and ask-user findings wait for consent. It
// runs only when routing is active; unresolved findings terminate safely.
type repairResult struct {
	Owned       bool
	Resolved    bool
	ResolvedIDs []string
	NewFindings []types.Finding
}

func repairResultFromStates(states map[string]*lineageState, seeds []repairSeed) repairResult {
	seedLineages := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		seedLineages[seed.LineageID] = struct{}{}
	}
	result := repairResult{Owned: len(states) > 0, Resolved: true}
	for lineageID, state := range states {
		if !state.resolved && state.finding.Action != types.ActionNoOp {
			result.Resolved = false
		}
		if state.resolved && state.finding.Action != types.ActionNoOp {
			result.ResolvedIDs = append(result.ResolvedIDs, state.finding.ID)
		}
		_, original := seedLineages[lineageID]
		if !original || (state.finding.Action == types.ActionAskUser && !state.resolved) {
			result.NewFindings = append(result.NewFindings, state.finding)
		}
	}
	return result
}

func (e *Executor) maybeRepairReviewFinding(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, defaultBranch, reviewRoundID, findingsJSON string, reserveRound func(string) (*db.StepRound, error)) (repairResult, error) {
	if sctx.Invoker == nil || e.config == nil || e.config.Routing.IsZero() || strings.TrimSpace(findingsJSON) == "" {
		return repairResult{}, nil
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return repairResult{}, fmt.Errorf("parse review findings for repair: %w", err)
	}
	blocking := selectFindings(findings.Items, isBlockingAutoFix)
	informational := selectFindings(findings.Items, isInformationalAutoFix)
	if len(blocking) == 0 && len(informational) == 0 {
		return repairResult{}, nil
	}
	reviewAttemptID, byDisplay, err := e.reviewAttemptLineages(reviewRoundID)
	if err != nil {
		return repairResult{}, err
	}
	if reviewAttemptID == "" {
		return repairResult{}, fmt.Errorf("repairable review findings have no producing attempt lineage")
	}
	rc := &repairCoordinator{
		invoker:            sctx.Invoker,
		db:                 e.db,
		run:                run,
		stepResultID:       sr.ID,
		workDir:            sctx.WorkDir,
		branch:             run.Branch,
		defaultBranch:      defaultBranch,
		intent:             sctx.UserIntent,
		baseSHA:            run.BaseSHA,
		stepName:           types.StepReview,
		producingAttemptID: reviewAttemptID,
		log:                sctx.Log,
		logChunk:           sctx.LogChunk,
		reserveRound:       reserveRound,
	}
	result := repairResult{Resolved: true}
	if len(blocking) > 0 {
		seeds, err := seedsForFindings(blocking, byDisplay)
		if err != nil {
			return repairResult{}, err
		}
		rc.policy = blockingRepairPolicy(e.config.Routing)
		states, err := rc.escalateBatch(ctx, seeds)
		if err != nil {
			return repairResult{}, err
		}
		blockingResult := repairResultFromStates(states, seeds)
		result.Owned = result.Owned || blockingResult.Owned
		result.Resolved = result.Resolved && blockingResult.Resolved
		result.NewFindings = append(result.NewFindings, blockingResult.NewFindings...)
		result.ResolvedIDs = append(result.ResolvedIDs, blockingResult.ResolvedIDs...)
	}
	if len(informational) > 0 {
		seeds, err := seedsForFindings(informational, byDisplay)
		if err != nil {
			return repairResult{}, err
		}
		rc.policy = informationalRepairPolicy(e.config.Routing)
		states, err := rc.escalateBatch(ctx, seeds)
		if err != nil {
			return repairResult{}, err
		}
		infoResult := repairResultFromStates(states, seeds)
		result.Owned = result.Owned || infoResult.Owned
		result.NewFindings = append(result.NewFindings, infoResult.NewFindings...)
		result.ResolvedIDs = append(result.ResolvedIDs, infoResult.ResolvedIDs...)
	}
	return result, nil
}

// maybeRepairStepFindings routes a non-review step's auto-fix findings through
// the common repair coordinator. Blocking findings use the step's strong
// escalation policy and gate completion until resolved. Informational findings
// use the cheap informational policy, remain visible when unresolved, and never
// block the gate. Both paths run applicable deterministic checks before a fresh
// verifier. A deterministic step failure carries no producing agent attempt, so
// its lineages are synthetic run-local roots; the coordinator still persists
// finding-repair rows against them so unresolved blocking work surfaces on the
// run.
func (e *Executor) maybeRepairStepFindings(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, stepName types.StepName, findingsJSON string, checks []repairCheck, reserveRound func(string) (*db.StepRound, error)) (repairResult, error) {
	if sctx.Invoker == nil || e.config == nil || e.config.Routing.IsZero() || strings.TrimSpace(findingsJSON) == "" {
		return repairResult{}, nil
	}
	blockingPolicy, ok := stepRepairPolicyFor(e.config.Routing, stepName)
	if !ok {
		return repairResult{}, nil
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return repairResult{}, fmt.Errorf("parse %s findings for repair: %w", stepName, err)
	}
	blocking := selectFindings(findings.Items, isBlockingAutoFix)
	informational := selectFindings(findings.Items, isInformationalAutoFix)
	if len(blocking) == 0 && len(informational) == 0 {
		return repairResult{}, nil
	}
	rc := &repairCoordinator{
		invoker:       sctx.Invoker,
		db:            e.db,
		run:           run,
		stepResultID:  sr.ID,
		stepName:      stepName,
		workDir:       sctx.WorkDir,
		branch:        run.Branch,
		defaultBranch: sctx.Repo.DefaultBranch,
		intent:        sctx.UserIntent,
		baseSHA:       run.BaseSHA,
		checks:        checks,
		log:           sctx.Log,
		logChunk:      sctx.LogChunk,
		reserveRound:  reserveRound,
	}
	result := repairResult{Resolved: true}
	if len(blocking) > 0 {
		rc.policy = blockingPolicy
		seeds := syntheticSeeds(run.ID, stepName, "blocking", blocking)
		states, err := rc.escalateBatch(ctx, seeds)
		if err != nil {
			return repairResult{}, err
		}
		blockingResult := repairResultFromStates(states, seeds)
		result.Owned = blockingResult.Owned
		result.Resolved = blockingResult.Resolved
		result.ResolvedIDs = append(result.ResolvedIDs, blockingResult.ResolvedIDs...)
		result.NewFindings = append(result.NewFindings, blockingResult.NewFindings...)
	}
	if len(informational) > 0 {
		rc.policy = informationalRepairPolicy(e.config.Routing)
		seeds := syntheticSeeds(run.ID, stepName, "informational", informational)
		states, err := rc.escalateBatch(ctx, seeds)
		if err != nil {
			return repairResult{}, err
		}
		infoResult := repairResultFromStates(states, seeds)
		result.Owned = result.Owned || infoResult.Owned
		result.ResolvedIDs = append(result.ResolvedIDs, infoResult.ResolvedIDs...)
		result.NewFindings = append(result.NewFindings, infoResult.NewFindings...)
	}
	return result, nil
}

// syntheticSeeds mints an in-memory root lineage per deterministic step finding.
// A configured-command failure has no producing agent attempt, so its lineage
// id is run-local rather than a durable finding lineage tied to an attempt.
func syntheticSeeds(runID string, stepName types.StepName, class string, items []types.Finding) []repairSeed {
	seeds := make([]repairSeed, 0, len(items))
	for i, f := range items {
		seeds = append(seeds, repairSeed{LineageID: fmt.Sprintf("det:%s:%s:%s:%d", stepName, runID, class, i), Finding: f})
	}
	return seeds
}

func hasBlockingFindingsJSON(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return true
	}
	for _, finding := range findings.Items {
		if isBlockingSeverity(finding.Severity) {
			return true
		}
	}
	return false
}

func isBlockingAutoFix(f types.Finding) bool {
	return f.Action == types.ActionAutoFix && isBlockingSeverity(f.Severity)
}

func isInformationalAutoFix(f types.Finding) bool {
	return f.Action == types.ActionAutoFix && f.Severity == "info"
}

// selectFindings returns the findings matching pred, preserving order.
func selectFindings(items []types.Finding, pred func(types.Finding) bool) []types.Finding {
	var out []types.Finding
	for _, f := range items {
		if pred(f) {
			out = append(out, f)
		}
	}
	return out
}

// seedsForFindings pairs every finding with its mandatory root lineage.
func seedsForFindings(items []types.Finding, byDisplay map[string]string) ([]repairSeed, error) {
	seeds := make([]repairSeed, 0, len(items))
	for _, f := range items {
		lineageID := byDisplay[f.ID]
		if lineageID == "" {
			return nil, fmt.Errorf("finding %q has no durable lineage", f.ID)
		}
		seeds = append(seeds, repairSeed{LineageID: lineageID, Finding: f})
	}
	return seeds, nil
}

// reviewAttemptLineages returns the succeeded routed review attempt in a round
// and a display-id → root-lineage-id map for its findings.
func (e *Executor) reviewAttemptLineages(reviewRoundID string) (string, map[string]string, error) {
	attempts, err := e.db.GetInvocationAttemptsByRound(reviewRoundID)
	if err != nil {
		return "", nil, fmt.Errorf("load review attempts for repair: %w", err)
	}
	var reviewAttempt *db.InvocationAttempt
	for _, attempt := range attempts {
		if attempt.Start.Purpose == types.PurposeInitialReview && attempt.Terminal != nil && attempt.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			reviewAttempt = attempt
		}
	}
	if reviewAttempt == nil {
		return "", nil, nil
	}
	runID := reviewAttempt.Start.Scope.RunID
	if runID == "" {
		return "", nil, fmt.Errorf("succeeded review attempt has no run scope")
	}
	lineages, err := e.db.GetFindingLineagesByRun(runID)
	if err != nil {
		return "", nil, fmt.Errorf("load run finding lineages: %w", err)
	}
	byDisplay := make(map[string]string, len(lineages))
	for _, lineage := range lineages {
		if _, duplicate := byDisplay[lineage.DisplayID]; duplicate {
			return "", nil, fmt.Errorf("review finding display id %q has duplicate lineages", lineage.DisplayID)
		}
		byDisplay[lineage.DisplayID] = lineage.ID
	}
	return reviewAttempt.ID, byDisplay, nil
}

// routingActive reports whether routed repair is available for this run.
func (e *Executor) routingActive() bool {
	return e.config != nil && !e.config.Routing.IsZero()
}

// repairConsentedFindings repairs the user- or unattended-consented review
// findings through the intent-sensitive cascade (starting at fix_balanced). It
// is the only path that may repair an ask-user finding; no fixer runs for such
// a finding before this consent. Returns the terminal lineage states.
func (e *Executor) repairConsentedFindings(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, defaultBranch, reviewRoundID, findingsJSON string, findingIDs []string, reserveRound func(string) (*db.StepRound, error)) (repairResult, error) {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return repairResult{}, fmt.Errorf("parse consented review findings: %w", err)
	}
	consented := findByIDs(findings.Items, findingIDs)
	if len(consented) == 0 {
		return repairResult{}, nil
	}
	reviewAttemptID, byDisplay, err := e.reviewAttemptLineages(reviewRoundID)
	if err != nil {
		return repairResult{}, err
	}
	if reviewAttemptID == "" {
		return repairResult{}, fmt.Errorf("consented review findings have no producing attempt lineage")
	}
	seeds, err := seedsForFindings(consented, byDisplay)
	if err != nil {
		return repairResult{}, err
	}
	rc := &repairCoordinator{
		invoker:            sctx.Invoker,
		db:                 e.db,
		run:                run,
		stepResultID:       sr.ID,
		workDir:            sctx.WorkDir,
		branch:             run.Branch,
		defaultBranch:      defaultBranch,
		intent:             sctx.UserIntent,
		baseSHA:            run.BaseSHA,
		stepName:           types.StepReview,
		producingAttemptID: reviewAttemptID,
		policy:             intentSensitiveRepairPolicy(e.config.Routing),
		log:                sctx.Log,
		logChunk:           sctx.LogChunk,
		reserveRound:       reserveRound,
	}
	states, err := rc.escalateBatch(ctx, seeds)
	if err != nil {
		return repairResult{}, err
	}
	return repairResultFromStates(states, seeds), nil
}

// findByIDs returns the actionable findings whose display id was consented; a
// no-op finding is never repaired even when its id is selected.
func findByIDs(items []types.Finding, ids []string) []types.Finding {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []types.Finding
	for _, f := range items {
		if want[f.ID] && f.Action != types.ActionNoOp {
			out = append(out, f)
		}
	}
	return out
}

func mergeRepairFindingsJSON(raw string, additional []types.Finding) (string, error) {
	if len(additional) == 0 {
		return raw, nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", fmt.Errorf("parse findings before merging verifier output: %w", err)
	}
	byID := make(map[string]int, len(findings.Items))
	for i, finding := range findings.Items {
		byID[finding.ID] = i
	}
	for _, finding := range additional {
		if i, exists := byID[finding.ID]; exists && finding.ID != "" {
			findings.Items[i] = finding
			continue
		}
		byID[finding.ID] = len(findings.Items)
		findings.Items = append(findings.Items, finding)
	}
	findings.Summary = fmt.Sprintf("%d finding(s), including verifier-created findings", len(findings.Items))
	merged, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return "", fmt.Errorf("marshal findings with verifier output: %w", err)
	}
	return merged, nil
}

func removeFindingsByID(raw string, ids []string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", fmt.Errorf("parse findings before removing resolved selection: %w", err)
	}
	remove := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		remove[id] = struct{}{}
	}
	remaining := findings.Items[:0]
	for _, finding := range findings.Items {
		if _, selected := remove[finding.ID]; !selected {
			remaining = append(remaining, finding)
		}
	}
	if len(remaining) == 0 {
		return "", nil
	}
	findings.Items = remaining
	findings.Summary = fmt.Sprintf("%d finding(s) remain after the selected repair", len(remaining))
	raw, err = types.MarshalFindingsJSON(findings)
	if err != nil {
		return "", fmt.Errorf("marshal unselected findings: %w", err)
	}
	return raw, nil
}
