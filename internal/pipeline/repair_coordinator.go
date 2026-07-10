package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// repairVerdictSchema is the strong verifier's adjudication of one lineage.
var repairVerdictSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"lineage_id": {"type": "string"},
		"status": {"type": "string", "enum": ["resolved", "unresolved", "inconclusive"]},
		"rationale": {"type": "string"}
	},
	"required": ["lineage_id", "status", "rationale"]
}`)

type repairVerdict struct {
	LineageID string `json:"lineage_id"`
	Status    string `json:"status"`
	Rationale string `json:"rationale"`
}

// repairCheck is one deterministic check the coordinator runs before the strong
// verifier. Run reports whether the check applies to this repair; an
// inapplicable check is recorded but never blocks the verifier.
type repairCheck struct {
	Command string
	Run     func(ctx context.Context) (applicable bool, exitCode int, output string)
}

// repairTarget is one blocking root finding selected for repair.
type repairTarget struct {
	LineageID       string
	Finding         types.Finding
	Intent          string
	BaseSHA         string
	Tier            int
	RemainingBudget int
}

// repairResult is the terminal disposition of one repair cycle.
type repairResult struct {
	RepairID  string
	Resolved  bool
	Verdict   string
	Rationale string
}

// repairCoordinator resolves one blocking root finding through a fresh fixer, a
// fresh strong verifier, and the deterministic checks in between. Fixer and
// verifier are separate journaled invocations routed through the provider
// cascade. Every non-`resolved` outcome terminates safely: the coordinator
// records the attempt and returns unresolved rather than looping.
type repairCoordinator struct {
	invoker       agent.Invoker
	db            *db.DB
	run           *db.Run
	stepResultID  string
	workDir       string
	branch        string
	defaultBranch string
	checks        []repairCheck
	log           func(string)
	logChunk      func(string)
	reserveRound  func(trigger string) (*db.StepRound, error)
}

// fixerPurpose is the routed Purpose whose first tier is fix_fast (Luna/Sonnet).
const fixerPurpose = types.PurposeStructuredFindingRepair

// verifierPurpose is the routed Purpose whose route is review_strong; it
// adjudicates a repaired lineage with a fresh strong invocation.
const verifierPurpose = types.PurposeNormalAggregateVerification

// attemptRepair runs one fix→checks→verify cycle for the target finding. It
// never returns an error for an unresolved outcome; an error means the cycle
// could not be conducted (round reservation, journal, or git failure).
func (rc *repairCoordinator) attemptRepair(ctx context.Context, target repairTarget) (repairResult, error) {
	round, err := rc.reserveRound("auto_fix")
	if err != nil {
		return repairResult{}, fmt.Errorf("reserve repair round: %w", err)
	}
	scope := types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: rc.run.ID, StepResultID: rc.stepResultID, StepRoundID: round.ID}
	started := time.Now()

	repairID, err := rc.db.StartFindingRepair(db.FindingRepairStart{
		RunID:           rc.run.ID,
		LineageID:       target.LineageID,
		StepResultID:    rc.stepResultID,
		StepRoundID:     round.ID,
		Severity:        target.Finding.Severity,
		Action:          target.Finding.Action,
		Description:     target.Finding.Description,
		File:            target.Finding.File,
		Line:            target.Finding.Line,
		Tier:            target.Tier,
		RemainingBudget: target.RemainingBudget,
	})
	if err != nil {
		_ = rc.db.TerminateReservedStepRound(round.ID, db.StepRoundFailed, time.Since(started).Milliseconds())
		return repairResult{}, fmt.Errorf("persist finding repair: %w", err)
	}
	result := repairResult{RepairID: repairID}

	// Terminate safely: record the disposition, complete the round, and return
	// without escalating or looping.
	finish := func(verdict, rationale, status string, resolved bool, summary string) (repairResult, error) {
		if derr := rc.db.ResolveFindingRepair(repairID, verdict, rationale, status); derr != nil {
			rc.logf("warning: record repair verdict: %v", derr)
		}
		if cerr := rc.db.CompleteReservedStepRound(round.ID, nil, ptrOrNil(summary), time.Since(started).Milliseconds()); cerr != nil {
			return repairResult{}, fmt.Errorf("complete repair round: %w", cerr)
		}
		result.Resolved = resolved
		result.Verdict = verdict
		result.Rationale = rationale
		return result, nil
	}

	preDiff := rc.reviewDiff(ctx, target.BaseSHA)

	// Fresh fixer. Only structured facts reach the fixer: intent, diff, the
	// finding lineage, and the remaining budget — never a prior transcript.
	rc.log("repairing blocking finding with a fresh fixer...")
	fixResult, fixErr := rc.invoker.Invoke(ctx, agent.InvocationRequest{
		Purpose: fixerPurpose,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: buildFixerPrompt(target, preDiff), CWD: rc.workDir, JSONSchema: commitSummarySchemaJSON, OnChunk: rc.logChunk},
	})
	if attemptID := rc.succeededAttemptID(round.ID, fixerPurpose); attemptID != "" {
		_ = rc.db.SetFindingRepairFixer(repairID, attemptID)
	}
	if fixErr != nil {
		rc.logf("fixer failed: %v", fixErr)
		return finish("", "", db.RepairStatusFailed, false, "")
	}
	summary := extractRepairSummary(fixResult)
	if err := rc.commitFix(ctx, summary); err != nil {
		return repairResult{}, fmt.Errorf("commit repair: %w", err)
	}

	// Applicable deterministic checks run before the verifier. A failed check
	// terminates safely without spending a verifier invocation.
	for _, check := range rc.checks {
		applicable, exitCode, output := check.Run(ctx)
		if err := rc.db.RecordFindingRepairCheck(repairID, check.Command, applicable, exitCode, output); err != nil {
			rc.logf("warning: record repair check: %v", err)
		}
		if applicable && exitCode != 0 {
			rc.logf("deterministic check failed (%s), leaving finding unresolved", check.Command)
			return finish("", "", db.RepairStatusUnresolved, false, summary)
		}
	}

	postDiff := rc.reviewDiff(ctx, target.BaseSHA)

	// Fresh strong verifier that explicitly adjudicates the selected lineage.
	rc.log("verifying the repair with a fresh strong reviewer...")
	verifyResult, verifyErr := rc.invoker.Invoke(ctx, agent.InvocationRequest{
		Purpose: verifierPurpose,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: buildVerifierPrompt(target, postDiff), CWD: rc.workDir, JSONSchema: repairVerdictSchema, OnChunk: rc.logChunk},
	})
	if attemptID := rc.succeededAttemptID(round.ID, verifierPurpose); attemptID != "" {
		_ = rc.db.SetFindingRepairVerifier(repairID, attemptID)
	}
	if verifyErr != nil {
		rc.logf("verifier failed: %v", verifyErr)
		return finish("", "", db.RepairStatusUnresolved, false, summary)
	}

	verdict, ok := parseRepairVerdict(verifyResult)
	// Fail closed: only an explicit `resolved` verdict that names exactly this
	// lineage and carries a rationale succeeds. Missing IDs, silence, malformed
	// adjudication, `unresolved`, and `inconclusive` all leave it unresolved.
	if !ok || verdict.Status != db.RepairVerdictResolved || verdict.LineageID != target.LineageID || strings.TrimSpace(verdict.Rationale) == "" {
		status := db.RepairStatusUnresolved
		return finish(verdict.Status, verdict.Rationale, status, false, summary)
	}
	rc.log("verifier resolved the finding")
	return finish(db.RepairVerdictResolved, verdict.Rationale, db.RepairStatusResolved, true, summary)
}

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
	message := fmt.Sprintf("no-mistakes(review): %s", summary)
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

func parseRepairVerdict(result *agent.Result) (repairVerdict, bool) {
	if result == nil || result.Output == nil {
		return repairVerdict{}, false
	}
	var verdict repairVerdict
	if err := json.Unmarshal(result.Output, &verdict); err != nil {
		return repairVerdict{}, false
	}
	return verdict, true
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

func buildFixerPrompt(target repairTarget, diff string) string {
	location := ""
	if target.Finding.File != "" {
		location = fmt.Sprintf("\nLocation: %s", target.Finding.File)
		if target.Finding.Line > 0 {
			location += fmt.Sprintf(":%d", target.Finding.Line)
		}
	}
	intent := strings.TrimSpace(target.Intent)
	if intent == "" {
		intent = "(no recorded intent)"
	}
	return fmt.Sprintf(`Fix exactly one code-review finding. Apply the smallest correct change that resolves it and nothing else.

Finding (lineage %s, severity %s, action %s):
%s%s

User intent for the change under review:
%s

Remaining repair budget: %d escalation tier(s) after this attempt.

Diff currently under review:
%s

Rules:
- Address only this finding; make no unrelated changes.
- Prefer the minimal, targeted fix.
- Return a one-line commit summary as {"summary": "<what you changed>"}.`,
		target.LineageID, target.Finding.Severity, target.Finding.Action,
		target.Finding.Description, location, intent, target.RemainingBudget, diff)
}

func buildVerifierPrompt(target repairTarget, diff string) string {
	location := ""
	if target.Finding.File != "" {
		location = fmt.Sprintf(" (%s", target.Finding.File)
		if target.Finding.Line > 0 {
			location += fmt.Sprintf(":%d", target.Finding.Line)
		}
		location += ")"
	}
	return fmt.Sprintf(`Independently verify whether one specific code-review finding has been resolved by the latest changes. You did not write the fix; judge it fresh.

Finding lineage id: %s
Finding%s, severity %s:
%s

Changes to adjudicate:
%s

Return a JSON verdict of the form {"lineage_id": %q, "status": "resolved"|"unresolved"|"inconclusive", "rationale": "<one sentence>"}.
- "resolved": the finding is fully and correctly addressed by these changes.
- "unresolved": the finding is not addressed, or the change is wrong or incomplete.
- "inconclusive": the evidence does not let you determine resolution.
- The lineage_id field MUST be exactly %q.`,
		target.LineageID, location, target.Finding.Severity, target.Finding.Description, diff, target.LineageID, target.LineageID)
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

// maybeRepairReviewFinding routes one blocking auto-fix review finding through a
// single fresh verified repair before the executor falls through to the approval
// gate. It runs only when routing is active; a non-resolved outcome terminates
// safely (the finding remains and the step gates as before).
func (e *Executor) maybeRepairReviewFinding(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, defaultBranch, reviewRoundID, findingsJSON string, reserveRound func(string) (*db.StepRound, error)) {
	if sctx.Invoker == nil || e.config == nil || e.config.Routing.IsZero() || !routedPurposes[fixerPurpose] {
		return
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return
	}
	target, ok := selectBlockingAutoFix(findings.Items)
	if !ok {
		return
	}
	lineageID := e.lineageForFinding(reviewRoundID, target.ID)
	if lineageID == "" {
		return
	}
	rc := &repairCoordinator{
		invoker:       sctx.Invoker,
		db:            e.db,
		run:           run,
		stepResultID:  sr.ID,
		workDir:       sctx.WorkDir,
		branch:        run.Branch,
		defaultBranch: defaultBranch,
		log:           sctx.Log,
		logChunk:      sctx.LogChunk,
		reserveRound:  reserveRound,
	}
	rt := repairTarget{
		LineageID:       lineageID,
		Finding:         target,
		Intent:          sctx.UserIntent,
		BaseSHA:         run.BaseSHA,
		Tier:            0,
		RemainingBudget: e.repairBudget(fixerPurpose, 0),
	}
	if _, err := rc.attemptRepair(ctx, rt); err != nil {
		slog.Warn("review repair could not be conducted", "error", err)
	}
}

// selectBlockingAutoFix returns the first blocking finding whose action is
// auto-fix — the single finding this ticket's fast repair targets.
func selectBlockingAutoFix(items []types.Finding) (types.Finding, bool) {
	for _, f := range items {
		if f.Action == types.ActionAutoFix && (f.Severity == "error" || f.Severity == "warning") {
			return f, true
		}
	}
	return types.Finding{}, false
}

// lineageForFinding resolves the run-wide root lineage id for a review finding's
// display id, via the succeeded routed review attempt in the review round.
func (e *Executor) lineageForFinding(reviewRoundID, displayID string) string {
	attempts, err := e.db.GetInvocationAttemptsByRound(reviewRoundID)
	if err != nil {
		return ""
	}
	reviewAttemptID := ""
	for _, a := range attempts {
		if a.Start.Purpose == types.PurposeInitialReview && a.Terminal != nil && a.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			reviewAttemptID = a.ID
		}
	}
	if reviewAttemptID == "" {
		return ""
	}
	lineages, err := e.db.GetFindingLineagesByAttempt(reviewAttemptID)
	if err != nil {
		return ""
	}
	for _, l := range lineages {
		if l.DisplayID == displayID {
			return l.ID
		}
	}
	return ""
}

// repairBudget returns the escalation tiers remaining in a repair Purpose's
// route after the given tier.
func (e *Executor) repairBudget(purpose types.Purpose, tier int) int {
	profiles, err := e.config.Routing.ResolveRoute(purpose)
	if err != nil {
		return 0
	}
	remaining := len(profiles) - 1 - tier
	if remaining < 0 {
		return 0
	}
	return remaining
}
