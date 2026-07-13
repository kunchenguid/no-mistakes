package pipeline

import (
	"context"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepContext provides shared resources to pipeline steps during execution.
type StepContext struct {
	Ctx              context.Context
	Run              *db.Run
	Repo             *db.Repo
	WorkDir          string
	Agent            agent.Agent
	Config           *config.Config
	DB               *db.DB
	Log              func(string) // discrete log line (newline-terminated, user-visible + file)
	LogChunk         func(string) // raw streaming chunk (user-visible + file)
	LogFile          func(string) // file-only log callback (not shown to user)
	Fixing           bool         // true when re-executing after a "fix" action
	PreviousFindings string       // JSON findings from the previous execution (set during fix loop)
	// StepResultID is the DB row ID of the current step's step_results record.
	// Steps use it to query their own round history for multi-round prompts.
	StepResultID string
	Env          []string // extra environment variables for subprocesses (used in tests)
	// UserIntent is a short, possibly-empty summary of what the change author
	// was trying to accomplish. It's surfaced in step prompts so agents have
	// context beyond the diff. Its authority depends on IntentSource: an
	// explicit `--intent` is the author's own goal statement, while an
	// inferred summary comes from a local agent transcript.
	UserIntent string
	// IntentSource records the provenance of UserIntent so steps can weigh
	// its authority. db.RunIntentSourceAgent ("agent") means the driving
	// agent supplied it explicitly via `axi run --intent` (authoritative
	// acceptance criteria); an agent name ("claude", "codex", ...) means it
	// was inferred from a transcript (a hint). Empty when no intent exists.
	IntentSource string
	// Sessions manages the run's durable review-loop agent sessions
	// (reviewer and fixer roles). nil runs every invocation cold.
	Sessions *RunSessions
	// Shared carries in-memory run-scoped results one step hands to a later
	// step in the same run (e.g. the combined document+lint pass).
	Shared *RunShared
}

// RunAgentSession executes one turn of a durable review-loop role session,
// running cold when sessions are unavailable. Only the review step's
// reviewer/fixer turns use this; every other agent invocation goes through
// sctx.Agent.Run directly and stays session-isolated.
func (sctx *StepContext) RunAgentSession(role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	if sctx.Sessions == nil {
		return sctx.Agent.Run(sctx.Ctx, opts)
	}
	return sctx.Sessions.Run(sctx.Ctx, sctx.Agent, role, opts, sctx.Log)
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
