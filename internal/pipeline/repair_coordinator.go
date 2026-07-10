package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
func (rc *repairCoordinator) succeededAttemptID(roundID string, purpose types.Purpose) string {
	attempts, err := rc.db.GetInvocationAttemptsByRound(roundID)
	if err != nil {
		return ""
	}
	id := ""
	for _, attempt := range attempts {
		if attempt.Start.Purpose == purpose && attempt.Terminal != nil && attempt.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			id = attempt.ID
		}
	}
	return id
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
func (e *Executor) maybeRepairReviewFinding(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, defaultBranch, reviewRoundID, findingsJSON string, reserveRound func(string) (*db.StepRound, error)) {
	if sctx.Invoker == nil || e.config == nil || e.config.Routing.IsZero() || !routedPurposes[fixerPurpose] {
		return
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return
	}
	blocking := selectFindings(findings.Items, isBlockingAutoFix)
	informational := selectFindings(findings.Items, isInformationalAutoFix)
	if len(blocking) == 0 && len(informational) == 0 {
		return
	}
	reviewAttemptID, byDisplay := e.reviewAttemptLineages(reviewRoundID)
	if reviewAttemptID == "" {
		return
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
	// Blocking findings escalate through the full cascade; informational
	// findings take the cheap non-blocking two-tier cascade. Each batch runs
	// under its own policy.
	if seeds := seedsForFindings(blocking, byDisplay); len(seeds) > 0 {
		rc.policy = blockingRepairPolicy(e.config.Routing)
		if _, err := rc.escalateBatch(ctx, seeds); err != nil {
			slog.Warn("blocking review repair could not be conducted", "error", err)
		}
	}
	if seeds := seedsForFindings(informational, byDisplay); len(seeds) > 0 {
		rc.policy = informationalRepairPolicy(e.config.Routing)
		if _, err := rc.escalateBatch(ctx, seeds); err != nil {
			slog.Warn("informational review repair could not be conducted", "error", err)
		}
	}
}

// maybeRepairStepFindings routes a non-review step's blocking findings through
// the common repair coordinator: a fresh fixer at the policy's first tier, the
// step's deterministic checks, then a fresh strong verifier, escalating
// unresolved lineages through the routed cascade and failing closed at the
// budget. It returns true when it took ownership of the repair, so the executor
// skips the legacy per-step auto-fix loop. A deterministic step failure carries
// no producing agent attempt, so its lineages are synthetic run-local roots;
// the coordinator still persists finding-repair rows against them, so
// unresolved blocking work surfaces on the run.
func (e *Executor) maybeRepairStepFindings(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, stepName types.StepName, findingsJSON string, checks []repairCheck, reserveRound func(string) (*db.StepRound, error)) bool {
	if sctx.Invoker == nil || e.config == nil || e.config.Routing.IsZero() {
		return false
	}
	policy, ok := stepRepairPolicyFor(e.config.Routing, stepName)
	if !ok || !routedPurposes[policy.fixerPurpose] {
		return false
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return false
	}
	blocking := selectFindings(findings.Items, func(f types.Finding) bool { return isBlockingSeverity(f.Severity) })
	if len(blocking) == 0 {
		return false
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
		policy:        policy,
		checks:        checks,
		log:           sctx.Log,
		logChunk:      sctx.LogChunk,
		reserveRound:  reserveRound,
	}
	if _, err := rc.escalateBatch(ctx, syntheticSeeds(run.ID, stepName, blocking)); err != nil {
		slog.Warn("step repair could not be conducted", "step", stepName, "error", err)
	}
	return true
}

// syntheticSeeds mints an in-memory root lineage per deterministic step finding.
// A configured-command failure has no producing agent attempt, so its lineage
// id is run-local rather than a durable finding lineage tied to an attempt.
func syntheticSeeds(runID string, stepName types.StepName, items []types.Finding) []repairSeed {
	seeds := make([]repairSeed, 0, len(items))
	for i, f := range items {
		seeds = append(seeds, repairSeed{LineageID: fmt.Sprintf("det:%s:%s:%d", stepName, runID, i), Finding: f})
	}
	return seeds
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

// seedsForFindings pairs each finding with its root lineage id, dropping any
// finding without a recorded lineage.
func seedsForFindings(items []types.Finding, byDisplay map[string]string) []repairSeed {
	var seeds []repairSeed
	for _, f := range items {
		if lineageID := byDisplay[f.ID]; lineageID != "" {
			seeds = append(seeds, repairSeed{LineageID: lineageID, Finding: f})
		}
	}
	return seeds
}

// reviewAttemptLineages returns the succeeded routed review attempt in a round
// and a display-id → root-lineage-id map for its findings.
func (e *Executor) reviewAttemptLineages(reviewRoundID string) (string, map[string]string) {
	attempts, err := e.db.GetInvocationAttemptsByRound(reviewRoundID)
	if err != nil {
		return "", nil
	}
	reviewAttemptID := ""
	for _, a := range attempts {
		if a.Start.Purpose == types.PurposeInitialReview && a.Terminal != nil && a.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			reviewAttemptID = a.ID
		}
	}
	if reviewAttemptID == "" {
		return "", nil
	}
	lineages, err := e.db.GetFindingLineagesByAttempt(reviewAttemptID)
	if err != nil {
		return "", nil
	}
	byDisplay := make(map[string]string, len(lineages))
	for _, l := range lineages {
		byDisplay[l.DisplayID] = l.ID
	}
	return reviewAttemptID, byDisplay
}

// routingActive reports whether routed repair is available for this run.
func (e *Executor) routingActive() bool {
	return e.config != nil && !e.config.Routing.IsZero() && routedPurposes[types.PurposeIntentSensitiveRepair]
}

// repairConsentedFindings repairs the user- or unattended-consented review
// findings through the intent-sensitive cascade (starting at fix_balanced). It
// is the only path that may repair an ask-user finding; no fixer runs for such
// a finding before this consent. Returns the terminal lineage states.
func (e *Executor) repairConsentedFindings(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, defaultBranch, reviewRoundID, findingsJSON string, findingIDs []string, reserveRound func(string) (*db.StepRound, error)) map[string]*lineageState {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return nil
	}
	consented := findByIDs(findings.Items, findingIDs)
	if len(consented) == 0 {
		return nil
	}
	reviewAttemptID, byDisplay := e.reviewAttemptLineages(reviewRoundID)
	if reviewAttemptID == "" {
		return nil
	}
	seeds := seedsForFindings(consented, byDisplay)
	if len(seeds) == 0 {
		return nil
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
		slog.Warn("consented review repair could not be conducted", "error", err)
	}
	return states
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
