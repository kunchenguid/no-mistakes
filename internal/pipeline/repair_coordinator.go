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
	invoker         agent.Invoker
	db              *db.DB
	run             *db.Run
	stepResultID    string
	workDir         string
	branch          string
	defaultBranch   string
	intent          string
	baseSHA         string
	reviewAttemptID string
	maxTier         int
	checks          []repairCheck
	log             func(string)
	logChunk        func(string)
	reserveRound    func(trigger string) (*db.StepRound, error)
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

// maybeRepairReviewFinding routes every blocking auto-fix review finding through
// the escalation coordinator before the executor falls through to the approval
// gate. It runs only when routing is active; unresolved findings terminate
// safely (they remain and the step gates as before).
func (e *Executor) maybeRepairReviewFinding(ctx context.Context, sctx *StepContext, run *db.Run, sr *db.StepResult, defaultBranch, reviewRoundID, findingsJSON string, reserveRound func(string) (*db.StepRound, error)) {
	if sctx.Invoker == nil || e.config == nil || e.config.Routing.IsZero() || !routedPurposes[fixerPurpose] {
		return
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return
	}
	blocking := selectAllBlockingAutoFix(findings.Items)
	if len(blocking) == 0 {
		return
	}
	reviewAttemptID, byDisplay := e.reviewAttemptLineages(reviewRoundID)
	if reviewAttemptID == "" {
		return
	}
	var seeds []repairSeed
	for _, f := range blocking {
		lineageID := byDisplay[f.ID]
		if lineageID == "" {
			continue
		}
		seeds = append(seeds, repairSeed{LineageID: lineageID, Finding: f})
	}
	if len(seeds) == 0 {
		return
	}
	rc := &repairCoordinator{
		invoker:         sctx.Invoker,
		db:              e.db,
		run:             run,
		stepResultID:    sr.ID,
		workDir:         sctx.WorkDir,
		branch:          run.Branch,
		defaultBranch:   defaultBranch,
		intent:          sctx.UserIntent,
		baseSHA:         run.BaseSHA,
		reviewAttemptID: reviewAttemptID,
		maxTier:         e.repairBudget(fixerPurpose, 0),
		log:             sctx.Log,
		logChunk:        sctx.LogChunk,
		reserveRound:    reserveRound,
	}
	if _, err := rc.escalateBatch(ctx, seeds); err != nil {
		slog.Warn("review repair could not be conducted", "error", err)
	}
}

// selectAllBlockingAutoFix returns every blocking finding whose action is
// auto-fix — the batch the escalation coordinator repairs together.
func selectAllBlockingAutoFix(items []types.Finding) []types.Finding {
	var out []types.Finding
	for _, f := range items {
		if f.Action == types.ActionAutoFix && isBlockingSeverity(f.Severity) {
			out = append(out, f)
		}
	}
	return out
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
