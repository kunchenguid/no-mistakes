package pipeline

import (
	"context"
	"errors"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var ErrFatalGateReconciliation = errors.New("fatal gate reconciliation")

// StepContext provides shared resources to pipeline steps during execution.
type StepContext struct {
	Ctx                   context.Context
	Run                   *db.Run
	Repo                  *db.Repo
	WorkDir               string
	Agent                 agent.Agent
	Config                *config.Config
	DB                    *db.DB
	Log                   func(string) // discrete log line (newline-terminated, user-visible + file)
	LogChunk              func(string) // raw streaming chunk (user-visible + file)
	LogFile               func(string) // file-only log callback (not shown to user)
	Fixing                bool         // true when re-executing after a "fix" action
	SkipFixExecution      bool         // replay an already-completed fix round's review turn only
	ReviewStartingHeadSHA string
	PreviousFindings      string // JSON findings from the previous execution (set during fix loop)
	// StepResultID is the DB row ID of the current step's step_results record.
	// Steps use it to query their own round history for multi-round prompts.
	StepResultID string
	// EvidenceDir is where this run's test-evidence artifacts belong, always
	// outside the worktree. The executor resolves it once from the app root
	// (honoring test.evidence.local_root) so every consumer - the test step's
	// prompt and the PR step's publisher - names the same directory. Empty only
	// in embeddings that never gather evidence.
	EvidenceDir string
	Env         []string // extra environment variables for subprocesses (used in tests)
	// UserIntent is a short, possibly-empty summary of what the change author
	// was trying to accomplish. It's surfaced in step prompts so agents have
	// context beyond the diff. Its authority depends on IntentSource: an
	// explicit `--intent` is the author's own goal statement, while an
	// inferred summary comes from a local agent transcript.
	UserIntent string
	// IntentSource records the provenance of UserIntent so steps can weigh
	// its authority. db.RunIntentSourceAgent ("agent") means the driving
	// agent supplied it explicitly via `axi run --intent`; db.RunIntentSourceRerun
	// ("rerun") means that authoritative intent was inherited. Both are
	// authoritative acceptance criteria; an agent name ("claude", "codex", ...)
	// means it was inferred from a transcript (a hint). Empty when no intent exists.
	IntentSource string
	// UncertifiedFromSHA/ToSHA/SourceRunID name a previous run's fixer
	// commits on this branch whose re-review did not complete. They are set
	// on a later run's initial review (Fixing==false) so that review still
	// receives fix-round provenance. Empty when no such range applies.
	UncertifiedFromSHA     string
	UncertifiedToSHA       string
	UncertifiedSourceRunID string
	// UncertifiedPriorRounds are review rounds from the source run that left
	// the uncertified range. Nil when none apply.
	UncertifiedPriorRounds []*db.StepRound
	// PriorBranchDecisions are rounds from EARLIER runs on this branch that
	// recorded a human decision about their findings. Unlike the uncertified
	// range above, nothing clears them when a review completes: a decision the
	// user made about this branch keeps standing until the branch's run history
	// ages out of the loader's bound. Nil when none apply. Advisory prompt
	// context only.
	PriorBranchDecisions          []*db.BranchDecisionRound
	PriorBranchDecisionsTruncated bool
	// Sessions manages the run's durable review-fixer session. The session
	// machinery remains role-generic for legacy recovery; nil runs every
	// invocation cold.
	Sessions *RunSessions
	// Shared carries in-memory run-scoped results one step hands to a later
	// step in the same run (e.g. the combined document+lint pass).
	Shared             *RunShared
	CIReadinessChanged func(ready, declaredNoCI bool)
	// OnPRMerged is a best-effort hook after a merged PR state is persisted.
	// Eval uses it to relabel auto-fix/shipped-unfixed gold; nil is a no-op.
	OnPRMerged func(ctx context.Context, runID string)
}

// RunAgentSession executes one turn of a durable review-loop role session,
// running cold when sessions are unavailable. The invocation is bounded by
// RunAgent's deadline. Only the review step's fixer turns use this; every
// other agent invocation - including every review turn, which must stay
// independent of the session that prescribed the fixes under review - goes
// through RunAgent and stays session-isolated.
func (sctx *StepContext) RunAgentSession(role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(sctx.Ctx, opts, role)
}

// StepOutcome is the result of executing a pipeline step.
type StepOutcome struct {
	NeedsApproval bool // whether the step pauses for user action
	AutoFixable   bool
	Findings      string // JSON findings for TUI display (optional)
	ExitCode      int    // process exit code (0 = success)
	PRURL         string // PR/MR URL if this step created or found one
	Skipped       bool   // mark the step as skipped without failing the run
	SkipRemaining bool   // skip all subsequent steps (e.g. empty diff after rebase)
	// FixSummary, when non-empty, is the agent's one-line commit summary for
	// the fix attempt performed during this round. Steps populate it in fix
	// mode so the executor can persist it on the round record and later
	// rounds can reference what was previously attempted.
	FixSummary string
	// ReviewApprovedHeadSHA is set only by a successfully executed full review
	// round. The executor durably records it only when the review step actually
	// completes, never while that outcome is parked or after a failed round.
	ReviewApprovedHeadSHA string

	// DurationOverrideMS, when positive, replaces the wall-clock duration
	// reported for this step. Used by demo mode to show realistic durations
	// without actually waiting.
	DurationOverrideMS int64
}

// Step is the interface that each pipeline step implements.
type Step interface {
	// Name returns the step's identity in the fixed pipeline sequence.
	Name() types.StepName

	// Execute runs the step logic and returns an outcome.
	// A step that returns NeedsApproval=true will pause the pipeline
	// until the user responds with an approval action.
	Execute(sctx *StepContext) (*StepOutcome, error)
}

// ApprovalGateReconciler is implemented by a step whose parked approval gate
// can become obsolete when an external source of truth changes. The executor
// invokes it with a bounded context while also waiting for an approval. A true
// result completes the step through the normal success path; false or an error
// leaves the gate parked. Implementations must be read-only and fail closed.
type ApprovalGateReconciler interface {
	ReconcileApprovalGate(sctx *StepContext) (resolved bool, err error)
}
