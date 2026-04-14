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
	Ctx               context.Context
	Run               *db.Run
	Repo              *db.Repo
	WorkDir           string
	Agent             agent.Agent
	Config            *config.Config
	DB                *db.DB
	Log               func(string) // streaming log callback (user-visible + file)
	LogFile           func(string) // file-only log callback (not shown to user)
	Fixing            bool         // true when re-executing after a "fix" action
	PreviousFindings  string       // JSON findings from the previous execution (set during fix loop)
	DismissedFindings string       // JSON findings the user explicitly deselected (excluded from fix)
	Env               []string     // extra environment variables for subprocesses (used in tests)
}

// StepOutcome is the result of executing a pipeline step.
type StepOutcome struct {
	NeedsApproval bool // whether the step pauses for user action
	AutoFixable   bool
	Findings      string // JSON findings for TUI display (optional)
	ExitCode      int    // process exit code (0 = success)
	PRURL         string // PR/MR URL if this step created or found one
	SkipRemaining bool   // skip all subsequent steps (e.g. empty diff after rebase)
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
