package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/gateguidance"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// EventFunc is called when a pipeline event occurs, for streaming to subscribers.
type EventFunc func(ipc.Event)

const (
	defaultGateReconcileInterval = config.DefaultGateReconcileInterval
	defaultGateReconcileTimeout  = config.DefaultGateReconcileTimeout
)

type approvalResponse struct {
	action        types.ApprovalAction
	findingIDs    []string
	instructions  map[string]string
	addedFindings []types.Finding
}

// Executor runs pipeline steps sequentially and coordinates approval interactions.
type Executor struct {
	db     *db.DB
	paths  *paths.Paths
	config *config.Config
	forge  *forgecontext.Context
	agent  agent.Agent
	steps  []Step
	skips  map[types.StepName]bool

	onEvent EventFunc

	// sessions manages this run's durable review-loop agent sessions; shared
	// carries run-scoped step-to-step results. Both are created per Execute.
	sessions *RunSessions
	shared   *RunShared
	workDir  string

	mu          sync.Mutex
	approvalCh  chan approvalResponse // buffered channel for approval responses
	waiting     bool                  // true when blocked on approval
	waitingStep types.StepName        // which step is currently awaiting approval

	gateReconcileInterval time.Duration
	gateReconcileTimeout  time.Duration
	onPRMerged            func(context.Context, string)
}

// SetOnPRMerged registers a best-effort hook invoked after a merged PR state
// is persisted. The pipeline never fails the run if the hook errors.
func (e *Executor) SetOnPRMerged(fn func(context.Context, string)) {
	if e == nil {
		return
	}
	e.onPRMerged = fn
}

// SetForgeContext configures the immutable provider context used by every
// subprocess in this run. A nil context preserves ambient behavior.
func (e *Executor) SetForgeContext(ctx *forgecontext.Context) {
	e.forge = ctx
}

// SetSkippedSteps configures steps that should be marked skipped without running.
func (e *Executor) SetSkippedSteps(steps []types.StepName) {
	if len(steps) == 0 {
		e.skips = nil
		return
	}
	e.skips = make(map[types.StepName]bool, len(steps))
	for _, step := range steps {
		e.skips[step] = true
	}
}

// NewExecutor creates a pipeline executor.
func NewExecutor(database *db.DB, p *paths.Paths, cfg *config.Config, ag agent.Agent, steps []Step, onEvent EventFunc) *Executor {
	if onEvent == nil {
		onEvent = func(ipc.Event) {}
	}
	exec := &Executor{
		db:                    database,
		paths:                 p,
		config:                cfg,
		agent:                 ag,
		steps:                 steps,
		onEvent:               onEvent,
		approvalCh:            make(chan approvalResponse, 1),
		gateReconcileInterval: defaultGateReconcileInterval,
		gateReconcileTimeout:  defaultGateReconcileTimeout,
	}
	if cfg != nil {
		// Global config is the production path for these timings; SetGate*
		// remains for tests and specialized embeddings.
		exec.SetGateReconcileTimings(cfg.GateReconcileInterval, cfg.GateReconcileTimeout)
	}
	return exec
}

// runEvidenceDir resolves where this run's test evidence is written. The
// executor is the single owner of that answer for the pipeline: steps read it
// from StepContext rather than recomputing a path, which is what let the
// steering preamble and the test step drift apart while both hardcoded the
// system temp directory.
func (e *Executor) runEvidenceDir(runID string) string {
	if e.paths == nil {
		return ""
	}
	configured := ""
	if e.config != nil {
		configured = e.config.Test.Evidence.LocalRoot
	}
	return e.paths.RunEvidenceDir(configured, runID)
}

// SetGateReconcileTimings overrides the interval between approval-gate
// reconciliation checks and the deadline for each check. It is primarily used
// by deterministic tests and specialized embeddings; non-positive values keep
// the production defaults.
func (e *Executor) SetGateReconcileTimings(interval, timeout time.Duration) {
	if interval > 0 {
		e.gateReconcileInterval = interval
	}
	if timeout > 0 {
		e.gateReconcileTimeout = timeout
	}
}

// Respond sends a user approval action to the currently waiting step.
// The step parameter must match the step currently awaiting approval.
// Returns an error if no step is awaiting approval or if the step name doesn't match.
func (e *Executor) Respond(step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	return e.RespondWithOverrides(step, action, findingIDs, nil, nil)
}

// RespondWithOverrides is like Respond but also carries per-finding user
// instructions and user-authored findings. Both are merged into the round's
// findings on a fix action before the fix agent runs.
func (e *Executor) RespondWithOverrides(step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	e.mu.Lock()
	if !e.waiting {
		e.mu.Unlock()
		return fmt.Errorf("no step awaiting approval")
	}
	if step != e.waitingStep {
		e.mu.Unlock()
		return fmt.Errorf("step mismatch: responding to %q but %q is awaiting approval", step, e.waitingStep)
	}
	e.waiting = false
	e.mu.Unlock()

	e.approvalCh <- approvalResponse{
		action:        action,
		findingIDs:    findingIDs,
		instructions:  instructions,
		addedFindings: addedFindings,
	}
	return nil
}

// Execute runs the pipeline steps sequentially for a given run.
// The workDir is the directory where steps execute (typically a git worktree).
// If the context is cancelled with a cause (via context.WithCancelCause),
// the cause message is preserved as the run's error in the DB.
func (e *Executor) Execute(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	e.workDir = workDir
	ctx = e.runContext(ctx)
	// Mark run as running. Route write failures through failRun so the
	// in-memory lifecycle and subscriber stream still become terminal instead
	// of leaving a silent pending run.
	if err := e.db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		return e.failRun(run, repo, fmt.Errorf("update run status: %w", err))
	}
	run.Status = types.RunRunning
	e.emitRunEvent(ipc.EventRunUpdated, run, repo)

	// Create log directory for this run
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}

	e.initializeRunScopes(run.ID)

	// Create step result records in DB
	stepRecords := make(map[types.StepName]*db.StepResult)
	for _, step := range e.steps {
		sr, err := e.db.InsertStepResult(run.ID, step.Name())
		if err != nil {
			return e.failRun(run, repo, fmt.Errorf("insert step result: %w", err))
		}
		stepRecords[step.Name()] = sr
	}

	// Execute steps sequentially. A late repair may send the same run back
	// through validation before any new head is published.
	for i := 0; i < len(e.steps); i++ {
		step := e.steps[i]
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx))
		}

		sr := stepRecords[step.Name()]
		if e.skips[step.Name()] {
			if err := e.db.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				return e.failRun(run, repo, fmt.Errorf("skip step %s: %w", step.Name(), err), ctx)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, step.Name(), string(types.StepStatusSkipped), "", "", nil)
			continue
		}
		state, err := e.durableExecutionState(sr.ID)
		if err != nil {
			return e.failRun(run, repo, fmt.Errorf("restore step %s execution state: %w", step.Name(), err), ctx)
		}
		skipRemaining, restartFrom, err := e.executeStep(ctx, step, sr, run, repo, workDir, logDir, state)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			// Mark all subsequent steps as skipped
			for _, remaining := range e.steps[i+1:] {
				rsr := stepRecords[remaining.Name()]
				if dbErr := e.db.CompleteStepWithStatus(rsr.ID, types.StepStatusSkipped, 0, 0, ""); dbErr != nil {
					slog.Warn("failed to finalize skipped step", "step", remaining.Name(), "error", dbErr)
				}
				e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, remaining.Name(), string(types.StepStatusSkipped), "", "", nil)
			}
			break
		}
		if restartFrom != "" {
			restartIndex, err := e.prepareRestart(run.ID, restartFrom, i)
			if err != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", step.Name(), restartFrom), ctx)
			}
			i = restartIndex - 1
		}
	}

	// Mark run as completed. A failure here must emit a terminal failure rather
	// than leaving a silent running row after every step has finished.
	if err := e.completeRun(run, repo); err != nil {
		return e.failRun(run, repo, fmt.Errorf("update run status: %w", err))
	}
	return nil
}

func (e *Executor) stepIndex(name types.StepName) (int, error) {
	for index, step := range e.steps {
		if step.Name() == name {
			return index, nil
		}
	}
	return 0, fmt.Errorf("step %s is not in the pipeline", name)
}

func (e *Executor) prepareRestart(runID string, name types.StepName, currentIndex int) (int, error) {
	index, err := e.stepIndex(name)
	if err != nil || index >= currentIndex {
		return 0, fmt.Errorf("invalid restart boundary")
	}
	if err := e.db.ResetStepsFrom(runID, e.steps[index].Name().Order()); err != nil {
		return 0, err
	}
	return index, nil
}

func (e *Executor) initializeRunScopes(runID string) {
	sessionsEnabled := e.config != nil && e.config.SessionReuse && e.agent != nil
	e.sessions = NewRunSessions(e.db, runID, e.agent, sessionsEnabled)
	e.shared = &RunShared{}
}

type stepExecutionState struct {
	fixing           bool
	previousFindings string
	roundNum         int
	autoFixAttempts  int
	executionMS      int64
	currentRoundID   string
}

func (e *Executor) durableExecutionState(stepResultID string) (stepExecutionState, error) {
	rounds, err := e.db.GetRoundsByStep(stepResultID)
	if err != nil {
		return stepExecutionState{}, err
	}
	state := stepExecutionState{}
	for _, round := range rounds {
		state.roundNum = max(state.roundNum, round.Round)
		if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
			state.autoFixAttempts++
		}
	}
	return state, nil
}

type recoveredGate struct {
	index           int
	step            Step
	stepResult      *db.StepResult
	findings        string
	round           int
	autoFixes       int
	lastRoundID     string
	reviewedHeadSHA string
}

func ValidateRecoveredRun(database *db.DB, run *db.Run, steps []Step) error {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		return fmt.Errorf("run is not a recoverable parked run")
	}
	_, err := (&Executor{db: database, steps: steps}).recoveredGate(run.ID)
	return err
}

// Resume restores a run that was durably parked at an approval gate when the
// daemon stopped. It only accepts a fully recorded gate and otherwise returns
// an error so startup recovery can fail the run rather than guessing.
func (e *Executor) Resume(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	e.workDir = workDir
	ctx = e.runContext(ctx)
	if repo == nil {
		return fmt.Errorf("recovered run has no repository")
	}
	if err := ValidateRecoveredRun(e.db, run, e.steps); err != nil {
		return err
	}
	gate, err := e.recoveredGate(run.ID)
	if err != nil {
		return err
	}
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}
	e.initializeRunScopes(run.ID)

	parkStart := time.Unix(*run.AwaitingAgentSince, 0)
	duration := recoveredStepDuration(gate.stepResult)
	completeRecoveredGate := func() error {
		if gate.step.Name() == types.StepReview {
			if gate.reviewedHeadSHA == "" {
				return fmt.Errorf("recovered review has no durable reviewed head candidate")
			}
			if err := e.db.CompleteReviewStep(gate.stepResult.ID, run.ID, gate.reviewedHeadSHA, recoveredExitCode(gate.stepResult), duration, recoveredLogPath(gate.stepResult)); err != nil {
				return err
			}
			reviewedHead := gate.reviewedHeadSHA
			run.ReviewApprovedHeadSHA = &reviewedHead
			ClearUncertifiedPipelineRangeIfCertified(ctx, e.db, repo.ID, run.Branch, reviewedHead, workDir)
			return nil
		}
		return e.db.CompleteStepWithStatus(gate.stepResult.ID, types.StepStatusCompleted, recoveredExitCode(gate.stepResult), duration, recoveredLogPath(gate.stepResult))
	}
	completeReconciledGate := func() error {
		if err := completeRecoveredGate(); err != nil {
			return e.failRun(run, repo, fmt.Errorf("complete reconciled step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusCompleted), "", "", &duration)
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	}
	reconcileCtx := &StepContext{
		Ctx:          ctx,
		Run:          run,
		Repo:         repo,
		WorkDir:      workDir,
		GateDir:      e.paths.RepoDir(repo.ID),
		Config:       e.config,
		ForgeContext: e.forge,
		DB:           e.db,
		Agent:        e.agent,
		Sessions:     e.sessions,
		Shared:       e.shared,
		Log: func(message string) {
			slog.Info("recovered approval gate reconciliation", "run_id", run.ID, "step", gate.step.Name(), "message", message)
		},
		LogChunk:   func(string) {},
		LogFile:    func(string) {},
		OnPRMerged: e.onPRMerged,
	}
	if reconciled, reconcileErr := e.reconcileApprovalGate(ctx, gate.step, reconcileCtx); reconciled {
		if dbErr := e.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds()); dbErr != nil {
			return e.failRun(run, repo, fmt.Errorf("complete reconciled awaiting-agent state: %w", dbErr), ctx)
		}
		return completeReconciledGate()
	} else if reconcileErr != nil && ctx.Err() == nil {
		if errors.Is(reconcileErr, ErrFatalGateReconciliation) {
			if dbErr := e.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds()); dbErr != nil {
				return e.failRun(run, repo, fmt.Errorf("complete fatal reconciliation awaiting-agent state: %w", dbErr), ctx)
			}
			if dbErr := e.db.FailStep(gate.stepResult.ID, reconcileErr.Error(), duration); dbErr != nil {
				slog.Warn("failed to mark recovered step as failed in db", "step", gate.step.Name(), "error", dbErr)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", reconcileErr.Error(), &duration)
			return e.failRun(run, repo, fmt.Errorf("step %s: reconcile approval gate: %w", gate.step.Name(), reconcileErr), ctx)
		}
		slog.Warn("could not reconcile recovered approval gate; preserving it", "run_id", run.ID, "step", gate.step.Name(), "error", reconcileErr)
	}

	e.mu.Lock()
	e.waiting = true
	e.waitingStep = gate.step.Name()
	e.mu.Unlock()
	e.emitStepEventWithFindingsAndError(
		ipc.EventStepCompleted,
		run,
		repo,
		gate.step.Name(),
		string(gate.stepResult.Status),
		gate.findings,
		"",
		gate.stepResult.DurationMS,
	)

	response, reconciled, err := e.waitForApprovalOrReconcile(ctx, gate.step, reconcileCtx, false)
	if dbErr := e.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds()); dbErr != nil {
		slog.Warn("failed to complete awaiting-agent state in db", "step", gate.step.Name(), "run", run.ID, "error", dbErr)
	}
	if err != nil {
		if dbErr := e.db.FailStep(gate.stepResult.ID, err.Error(), duration); dbErr != nil {
			slog.Warn("failed to mark recovered step as failed in db", "step", gate.step.Name(), "error", dbErr)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", err.Error(), &duration)
		return e.failRun(run, repo, fmt.Errorf("step %s: waiting for approval: %w", gate.step.Name(), err), ctx)
	}
	if reconciled {
		return completeReconciledGate()
	}

	approvalFields := telemetry.Fields{
		"step":       string(gate.step.Name()),
		"action":     string(response.action),
		"fix_review": gate.stepResult.Status == types.StepStatusFixReview,
	}
	if agentName := e.telemetryAgentName(); agentName != "" {
		approvalFields["agent"] = agentName
	}
	if selectedCount := selectedFindingCount(gate.findings, response.findingIDs); selectedCount > 0 {
		approvalFields["selected_findings_count"] = selectedCount
	}
	telemetry.Track("approval", approvalFields)
	switch response.action {
	case types.ActionApprove:
		e.recordDeclinedRound(gate.lastRoundID, gate.findings, gate.step.Name(), gate.round)
		if err := e.applyApprovalOverride(gate.step, reconcileCtx, gate.stepResult.ID); err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if err := completeRecoveredGate(); err != nil {
			return e.failRun(run, repo, fmt.Errorf("complete recovered step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusCompleted), "", "", &duration)
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	case types.ActionSkip:
		e.recordDeclinedRound(gate.lastRoundID, gate.findings, gate.step.Name(), gate.round)
		if err := e.db.CompleteStepWithStatus(gate.stepResult.ID, types.StepStatusSkipped, recoveredExitCode(gate.stepResult), duration, recoveredLogPath(gate.stepResult)); err != nil {
			return e.failRun(run, repo, fmt.Errorf("skip recovered step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusSkipped), "", "", &duration)
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	case types.ActionAbort:
		e.recordDeclinedRound(gate.lastRoundID, gate.findings, gate.step.Name(), gate.round)
		if dbErr := e.db.FailStep(gate.stepResult.ID, "aborted by user", duration); dbErr != nil {
			slog.Warn("failed to mark recovered step as aborted", "step", gate.step.Name(), "error", dbErr)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", "aborted by user", &duration)
		return e.failRun(run, repo, fmt.Errorf("step %s: aborted by user", gate.step.Name()), ctx)
	case types.ActionFix:
		telemetry.Track("fix", e.fixTelemetryFields("user", gate.step.Name(), selectedFindingCount(gate.findings, response.findingIDs), 0))
		selected := filterFindingsJSON(gate.findings, response.findingIDs)
		merged := mergeUserOverridesJSON(selected, response.instructions, response.addedFindings)
		if gate.lastRoundID != "" {
			allSelectedIDs := combineSelectedFindingIDs(response.findingIDs, merged)
			if idsJSON := marshalFindingIDs(allSelectedIDs); idsJSON != "" {
				var userFindingsJSON *string
				if merged != "" && merged != selected {
					userFindingsJSON = &merged
				}
				if dbErr := e.db.SetStepRoundUserDecision(gate.lastRoundID, &idsJSON, db.RoundSelectionSourceUser, userFindingsJSON); dbErr != nil {
					slog.Warn("failed to record recovered user decision", "step", gate.step.Name(), "round", gate.round, "error", dbErr)
				}
			}
		}
		if dbErr := e.db.UpdateStepStatus(gate.stepResult.ID, types.StepStatusFixing); dbErr != nil {
			return e.failRun(run, repo, fmt.Errorf("mark recovered step %s fixing: %w", gate.step.Name(), dbErr), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFixing), "", "", nil)
		skipRemaining, restartFrom, err := e.executeStep(ctx, gate.step, gate.stepResult, run, repo, workDir, logDir, stepExecutionState{
			fixing:           true,
			previousFindings: merged,
			roundNum:         gate.round,
			autoFixAttempts:  gate.autoFixes,
			executionMS:      duration,
			currentRoundID:   gate.lastRoundID,
		})
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(run, repo, gate.index+1)
		}
		if restartFrom != "" {
			restartIndex, indexErr := e.prepareRestart(run.ID, restartFrom, gate.index)
			if indexErr != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", gate.step.Name(), restartFrom), ctx)
			}
			return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, restartIndex, true)
		}
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	default:
		return e.failRun(run, repo, fmt.Errorf("step %s: unsupported approval action %q", gate.step.Name(), response.action), ctx)
	}
}

func (e *Executor) runContext(ctx context.Context) context.Context {
	if e.forge == nil {
		return ctx
	}
	return git.WithEnvironment(ctx, e.forge.Environment)
}

func (e *Executor) recoveredGate(runID string) (*recoveredGate, error) {
	results, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get recovered steps: %w", err)
	}
	if len(results) != len(e.steps) {
		return nil, fmt.Errorf("recovered run has %d step records for %d steps", len(results), len(e.steps))
	}

	var gate *recoveredGate
	for index, result := range results {
		if result.StepName != e.steps[index].Name() {
			return nil, fmt.Errorf("recovered step %d is %q, want %q", index, result.StepName, e.steps[index].Name())
		}
		if result.Status == types.StepStatusAwaitingApproval || result.Status == types.StepStatusFixReview {
			if gate != nil || result.FindingsJSON == nil || result.StartedAt == nil || result.DurationMS == nil || result.AgentPID != nil {
				return nil, fmt.Errorf("recovered approval gate is incomplete")
			}
			rounds, err := e.db.GetRoundsByStep(result.ID)
			if err != nil || len(rounds) == 0 {
				return nil, fmt.Errorf("recovered approval gate has no complete round")
			}
			latest := rounds[len(rounds)-1]
			if latest.FindingsJSON == nil || *latest.FindingsJSON != *result.FindingsJSON {
				return nil, fmt.Errorf("recovered approval gate findings are incomplete")
			}
			autoFixes := 0
			for _, round := range rounds {
				if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
					autoFixes++
				}
			}
			gate = &recoveredGate{
				index:       index,
				step:        e.steps[index],
				stepResult:  result,
				findings:    *result.FindingsJSON,
				round:       latest.Round,
				autoFixes:   autoFixes,
				lastRoundID: latest.ID,
			}
			if latest.ReviewedHeadSHA != nil {
				gate.reviewedHeadSHA = *latest.ReviewedHeadSHA
			}
			continue
		}
		if gate == nil {
			if result.Status != types.StepStatusCompleted && result.Status != types.StepStatusSkipped {
				return nil, fmt.Errorf("recovered step %s is %s before approval gate", result.StepName, result.Status)
			}
			continue
		}
		if result.Status != types.StepStatusPending && result.Status != types.StepStatusSkipped {
			return nil, fmt.Errorf("recovered step %s is %s after approval gate", result.StepName, result.Status)
		}
	}
	if gate == nil {
		return nil, fmt.Errorf("recovered run has no approval gate")
	}
	return gate, nil
}

func (e *Executor) executeRecoveredRemainder(ctx context.Context, run *db.Run, repo *db.Repo, workDir, logDir string, start int, revalidating bool) error {
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("get recovered steps: %w", err), ctx)
	}
	for index := start; index < len(e.steps); index++ {
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx), ctx)
		}
		if index >= len(results) || results[index].StepName != e.steps[index].Name() || (!revalidating && results[index].Status != types.StepStatusPending && results[index].Status != types.StepStatusSkipped) {
			return e.failRun(run, repo, fmt.Errorf("recovered step plan changed at %d", index), ctx)
		}
		if results[index].Status == types.StepStatusSkipped {
			continue
		}
		state, stateErr := e.durableExecutionState(results[index].ID)
		if stateErr != nil {
			return e.failRun(run, repo, fmt.Errorf("restore step %s execution state: %w", e.steps[index].Name(), stateErr), ctx)
		}
		skipRemaining, restartFrom, err := e.executeStep(ctx, e.steps[index], results[index], run, repo, workDir, logDir, state)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(run, repo, index+1)
		}
		if restartFrom != "" {
			restartIndex, indexErr := e.prepareRestart(run.ID, restartFrom, index)
			if indexErr != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", e.steps[index].Name(), restartFrom), ctx)
			}
			revalidating = true
			index = restartIndex - 1
		}
	}
	if err := e.completeRun(run, repo); err != nil {
		return e.failRun(run, repo, fmt.Errorf("complete recovered run: %w", err), ctx)
	}
	return nil
}

func (e *Executor) skipRecoveredRemainder(run *db.Run, repo *db.Repo, start int) error {
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("get recovered steps: %w", err))
	}
	for index := start; index < len(e.steps); index++ {
		if index >= len(results) || results[index].StepName != e.steps[index].Name() || results[index].Status != types.StepStatusPending {
			return e.failRun(run, repo, fmt.Errorf("recovered step plan changed at %d", index))
		}
		if err := e.db.CompleteStepWithStatus(results[index].ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
			return e.failRun(run, repo, fmt.Errorf("skip recovered step %s: %w", e.steps[index].Name(), err))
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, e.steps[index].Name(), string(types.StepStatusSkipped), "", "", nil)
	}
	if err := e.completeRun(run, repo); err != nil {
		return e.failRun(run, repo, fmt.Errorf("complete recovered run: %w", err))
	}
	return nil
}

func recoveredStepDuration(step *db.StepResult) int64 {
	if step != nil && step.DurationMS != nil {
		return *step.DurationMS
	}
	return 0
}

func recoveredExitCode(step *db.StepResult) int {
	if step != nil && step.ExitCode != nil {
		return *step.ExitCode
	}
	return 0
}

func recoveredLogPath(step *db.StepResult) string {
	if step != nil && step.LogPath != nil {
		return *step.LogPath
	}
	return ""
}

// executeStep runs a single step with approval coordination.
// Returns whether to skip the remainder, an optional earlier restart step,
// and any execution error.
func (e *Executor) executeStep(ctx context.Context, step Step, sr *db.StepResult, run *db.Run, repo *db.Repo, workDir, logDir string, state stepExecutionState) (bool, types.StepName, error) {
	stepName := step.Name()
	logPath := filepath.Join(logDir, string(stepName)+".log")
	finalExitCode := 0
	autoFixLimit := 0
	if e.config != nil {
		autoFixLimit = e.config.AutoFixLimit(stepName)
	}

	// Mark step as running
	if err := e.db.StartStepWithAutoFixLimit(sr.ID, autoFixLimit); err != nil {
		return false, "", fmt.Errorf("start step %s: %w", stepName, err)
	}
	e.emitStepEvent(ipc.EventStepStarted, run, repo, stepName, string(types.StepStatusRunning))

	// Track execution-only time, excluding approval wait periods.
	phaseStart := time.Now()
	executionMS := state.executionMS
	var durationOverrideMS int64 // sum of step-reported overrides (demo mode)

	// Open log file for persistent step logging
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, "", fmt.Errorf("create step log file %s: %w", stepName, err)
	}
	defer logFile.Close()

	// Build step context with log callback that emits events and writes to file.
	// lastChunkNewline tracks whether the most recent chunk ended with \n,
	// so Log knows whether it needs a leading \n to flush a streaming partial.
	lastChunkNewline := true
	userIntent := ""
	userIntentSource := ""
	if run != nil {
		if run.Intent != nil {
			userIntent = *run.Intent
		}
		// Propagate provenance alongside the text so steps can distinguish an
		// explicit, authoritative `--intent` (Source=="agent") from a
		// transcript-inferred hint. Dropping this is the provenance-erasure
		// bug that let an authoritative intent be demoted to an ignorable hint.
		if run.IntentSource != nil {
			userIntentSource = *run.IntentSource
		}
	}
	lastLogActivityAt := time.Time{}
	touchLogActivity := func(text string, force bool) {
		if activity := stepActivityFromLog(text); activity != "" {
			now := time.Now()
			if !force && !lastLogActivityAt.IsZero() && now.Sub(lastLogActivityAt) < stepActivityThrottleInterval {
				return
			}
			lastLogActivityAt = now
			if dbErr := e.db.TouchStepActivity(sr.ID, activity); dbErr != nil {
				slog.Warn("failed to touch step activity in db", "step", stepName, "error", dbErr)
			}
		}
	}
	writeLog := func(text string) {
		if text != "" {
			prefix := ""
			if !lastChunkNewline {
				prefix = "\n"
			}
			text = prefix + strings.TrimRight(text, "\n") + "\n\n"
			lastChunkNewline = true
		}
		e.emitLogChunk(run, repo, stepName, text)
		fmt.Fprint(logFile, text)
		touchLogActivity(text, true)
	}
	writeLogChunk := func(text string) {
		if text != "" {
			lastChunkNewline = strings.HasSuffix(text, "\n")
		}
		e.emitLogChunk(run, repo, stepName, text)
		fmt.Fprint(logFile, text)
		touchLogActivity(text, strings.Contains(text, "\n"))
	}
	onAgentLifecycle := func(event agent.LifecycleEvent) {
		text := event.Message
		if text == "" {
			text = fmt.Sprintf("%s %s", event.Agent, event.Phase)
		}
		switch event.Phase {
		case agent.LifecyclePhaseStart:
			pid := event.PID
			if dbErr := e.db.SetStepAgentActivity(sr.ID, text, &pid); dbErr != nil {
				slog.Warn("failed to set step agent activity in db", "step", stepName, "error", dbErr)
			}
		case agent.LifecyclePhaseExit:
			if dbErr := e.db.SetStepAgentActivity(sr.ID, text, nil); dbErr != nil {
				slog.Warn("failed to set step agent activity in db", "step", stepName, "error", dbErr)
			}
		case agent.LifecyclePhaseActivity:
			// Subprocess liveness, not narrative: record that the agent is still
			// producing bytes so `axi status` can distinguish a working fix round
			// from a wedged one, but never write it to the step log. A long turn
			// emits these every few seconds and the log is what an operator reads.
			if dbErr := e.db.TouchStepActivity(sr.ID, text); dbErr != nil {
				slog.Warn("failed to touch step activity in db", "step", stepName, "error", dbErr)
			}
			return
		default:
			if dbErr := e.db.TouchStepActivity(sr.ID, text); dbErr != nil {
				slog.Warn("failed to touch step activity in db", "step", stepName, "error", dbErr)
			}
		}
		writeLog(text)
	}
	// roundNum is shared with the perf wrapper's round closure below: an
	// invocation during execution of round N+1 sees roundNum still at N.
	autoFixAttempts := state.autoFixAttempts
	roundNum := state.roundNum

	stepAgent := e.agent
	if stepAgent != nil {
		// Innermost: default-by-construction invocation deadline so a step
		// that calls Agent.Run directly cannot hang the run.
		stepAgent = &timeoutAgent{inner: stepAgent, timeout: AgentTimeout(e.config)}
		stepAgent = &gateStepBoundaryAgent{inner: stepAgent, phase: stepName}
		stepAgent = &lifecycleAgent{inner: stepAgent, onLifecycle: onAgentLifecycle}
		stepAgent = &perfRecordingAgent{
			inner:    stepAgent,
			db:       e.db,
			runID:    run.ID,
			stepName: stepName,
			round:    func() int { return roundNum + 1 },
		}
	}
	ciReady := run.CIReadyAt != nil
	ciReadyNoCI := run.CIReadyNoCI
	ciReadinessChanged := func(ready, declaredNoCI bool) {
		declaredNoCI = ready && declaredNoCI
		if ciReady == ready && ciReadyNoCI == declaredNoCI {
			return
		}
		ciReady = ready
		ciReadyNoCI = declaredNoCI
		e.emitCIReadinessEvent(run, repo, ready, declaredNoCI)
	}
	sctx := &StepContext{
		Ctx:              ctx,
		Run:              run,
		Repo:             repo,
		WorkDir:          workDir,
		GateDir:          e.paths.RepoDir(repo.ID),
		Agent:            stepAgent,
		Config:           e.config,
		ForgeContext:     e.forge,
		DB:               e.db,
		StepResultID:     sr.ID,
		UserIntent:       userIntent,
		IntentSource:     userIntentSource,
		Sessions:         e.sessions,
		Shared:           e.shared,
		EvidenceDir:      e.runEvidenceDir(run.ID),
		Fixing:           state.fixing,
		PreviousFindings: state.previousFindings,
		Log:              writeLog,
		LogChunk:         writeLogChunk,
		LogFile: func(text string) {
			fmt.Fprintln(logFile, text)
			touchLogActivity(text, true)
		},
		CIReadinessChanged: ciReadinessChanged,
		OnPRMerged:         e.onPRMerged,
	}
	if stepName == types.StepReview {
		BindUncertifiedPipelineRange(sctx)
	}
	// Every step, not just review: the steps that used to re-apply a declined
	// change were precisely the ones a decision never reached.
	BindBranchDecisions(sctx)

	nextTrigger := "initial"
	if sctx.Fixing {
		nextTrigger = "auto_fix"
	}
	skipRemaining := false
	stepSkipped := false
	currentRoundID := state.currentRoundID
	var reviewApprovedHeadSHA string
	var restartFrom types.StepName

	// Execute with possible fix loop
	for {
		reviewStartingHeadSHA := run.HeadSHA
		sctx.ReviewStartingHeadSHA = reviewStartingHeadSHA
		outcome, err := step.Execute(sctx)
		roundNum++
		roundDuration := time.Since(phaseStart).Milliseconds()
		if err != nil {
			durationMS := executionMS + roundDuration
			// Persist the failure reason to the step's own log file. The error
			// often carries the only detail of why the step failed (e.g. git
			// stderr from a rejected push); without this the step log shows the
			// work starting but never why it stopped. Redact defensively so a
			// credentialled upstream URL that slipped into a wrapped error can
			// never land in the log file.
			redactedErr := safeurl.RedactText(err.Error())
			fmt.Fprintf(logFile, "\nerror: %s\n", redactedErr)
			touchLogActivity("error: "+redactedErr, true)
			if dbErr := e.db.FailStep(sr.ID, redactedErr, durationMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", redactedErr, &durationMS)
			return false, "", fmt.Errorf("step %s failed: %s", stepName, redactedErr)
		}
		restartFrom = outcome.RestartFrom

		if stepName == types.StepReview {
			reviewApprovedHeadSHA = outcome.ReviewApprovedHeadSHA
		}
		outcome.Findings = normalizeFindingsJSON(outcome.Findings, string(stepName))
		finalExitCode = outcome.ExitCode
		durationOverrideMS += outcome.DurationOverrideMS

		if outcome.Findings != "" {
			if dbErr := e.db.SetStepFindings(sr.ID, outcome.Findings); dbErr != nil {
				slog.Warn("failed to set step findings in db", "step", stepName, "error", dbErr)
			}
		} else {
			if dbErr := e.db.ClearStepFindings(sr.ID); dbErr != nil {
				slog.Warn("failed to clear step findings in db", "step", stepName, "error", dbErr)
			}
		}

		// Persist this execution round.
		var findingsPtr *string
		if outcome.Findings != "" {
			findingsPtr = &outcome.Findings
		}
		var fixSummaryPtr *string
		if outcome.FixSummary != "" {
			s := outcome.FixSummary
			fixSummaryPtr = &s
		}
		var inserted *db.StepRound
		var dbErr error
		roundTrigger := nextTrigger
		if stepName == types.StepCI && restartFrom != "" && !sctx.Fixing {
			roundTrigger = "auto_fix"
		}
		if stepName == types.StepReview {
			if e.config != nil && e.config.CaptureEvalProvenance {
				inserted, dbErr = e.db.InsertReviewStepRoundWithProvenance(sr.ID, roundNum, roundTrigger, findingsPtr, fixSummaryPtr, reviewApprovedHeadSHA, reviewStartingHeadSHA, e.config.TrustedConfigSHA, e.config.ReplayGlobalYAML, e.config.ReplayRepoYAML, roundDuration)
			} else {
				inserted, dbErr = e.db.InsertReviewStepRound(sr.ID, roundNum, roundTrigger, findingsPtr, fixSummaryPtr, reviewApprovedHeadSHA, roundDuration)
			}
		} else {
			inserted, dbErr = e.db.InsertStepRound(sr.ID, roundNum, roundTrigger, findingsPtr, fixSummaryPtr, roundDuration)
		}
		if dbErr != nil {
			currentRoundID = roundInsertID(currentRoundID, inserted, dbErr)
			slog.Warn("failed to insert step round", "step", stepName, "round", roundNum, "error", dbErr)
		} else {
			currentRoundID = roundInsertID(currentRoundID, inserted, nil)
		}

		// If the step produced a PR URL, propagate it to the run and emit an update.
		if outcome.PRURL != "" {
			run.PRURL = &outcome.PRURL
			e.emitRunEvent(ipc.EventRunUpdated, run, repo)
		}

		// Check if auto-fix should be attempted.
		// Only auto-fix findings whose action is "auto-fix".
		// This runs before the NeedsApproval check so that all severity
		// levels (including "info") get a chance at automatic fixing.
		if outcome.AutoFixable && autoFixLimit > 0 && autoFixAttempts < autoFixLimit {
			fixableFindings := autoFixableFindingsJSON(outcome.Findings)
			if fixableFindings != "" {
				autoFixAttempts++
				telemetry.Track("fix", e.fixTelemetryFields("auto", stepName, findingsCount(fixableFindings), autoFixAttempts))
				slog.Info("auto-fixing step", "step", stepName, "attempt", autoFixAttempts, "max", autoFixLimit)
				executionMS += time.Since(phaseStart).Milliseconds()
				fixCount := findingsCount(fixableFindings)
				writeLog(fmt.Sprintf("auto-fix round %d/%d starting after round %d (%d %s)", autoFixAttempts, autoFixLimit, roundNum, fixCount, pluralize(fixCount, "finding", "findings")))
				if dbErr := e.db.UpdateStepStatus(sr.ID, types.StepStatusFixing); dbErr != nil {
					slog.Warn("failed to update step status in db", "step", stepName, "status", "fixing", "error", dbErr)
				}
				if currentRoundID != "" {
					if idsJSON := findingIDsJSON(fixableFindings); idsJSON != "" {
						if dbErr := e.db.SetStepRoundSelection(currentRoundID, &idsJSON, db.RoundSelectionSourceAutoFix); dbErr != nil {
							slog.Warn("failed to record selected finding ids", "step", stepName, "round", roundNum, "error", dbErr)
						}
					}
				}
				e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFixing), "", "", nil)
				phaseStart = time.Now()
				sctx.Fixing = true
				sctx.PreviousFindings = fixableFindings
				nextTrigger = "auto_fix"
				continue
			}
		}

		if !outcome.NeedsApproval && !hasAskUserFindingsJSON(outcome.Findings) {
			// Step completed without needing approval.
			// Any remaining info-only or non-blocking findings
			// are acceptable and don't block the pipeline.
			skipRemaining = outcome.SkipRemaining
			stepSkipped = outcome.Skipped
			break
		}

		// Freeze execution timer before entering approval wait.
		executionMS += time.Since(phaseStart).Milliseconds()

		// Determine approval status: fix_review after a fix cycle, awaiting_approval otherwise.
		// The working-tree diff that shows what the agent changed is NOT
		// attached here: it is unbounded, and one frame over the transport
		// limit kills the whole subscription and hides every event after it.
		// Consumers fetch it on demand from the run's worktree instead
		// (ipc.MethodGetStepDiff).
		approvalStatus := types.StepStatusAwaitingApproval
		if sctx.Fixing {
			approvalStatus = types.StepStatusFixReview
		}

		// Mark executor as ready to receive approval before updating DB or
		// emitting events, so that callers who poll the DB status can
		// immediately call Respond once they see it.
		e.mu.Lock()
		e.waiting = true
		e.waitingStep = stepName
		e.mu.Unlock()

		// Parking starts before the gate becomes observable. This includes the
		// small handoff from publishing the gate to receiving a response, and
		// prevents a prompt response from being omitted from the parked total.
		parkStart := time.Now()

		// Surface the park as a pollable, run-level signal so a supervisor can
		// tell in one `axi status` read that the run is waiting for the agent
		// to drive this gate (versus actively running/fixing/ci). Observability
		// only: it does not change the wait below. Cleared once the wait ends.
		if dbErr := e.db.ParkStepForApproval(run.ID, sr.ID, approvalStatus, executionMS, findingsPtr); dbErr != nil {
			e.mu.Lock()
			e.waiting = false
			e.waitingStep = ""
			e.mu.Unlock()
			return false, "", fmt.Errorf("persist %s approval gate: %w", stepName, dbErr)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(approvalStatus), outcome.Findings, "", &executionMS)

		response, reconciled, err := e.waitForApprovalOrReconcile(ctx, step, sctx, true)
		if dbErr := e.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds()); dbErr != nil {
			slog.Warn("failed to complete awaiting-agent state in db", "step", stepName, "run", run.ID, "error", dbErr)
		}
		if err != nil {
			if dbErr := e.db.FailStep(sr.ID, err.Error(), executionMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", err.Error(), &executionMS)
			return false, "", fmt.Errorf("step %s: waiting for approval: %w", stepName, err)
		}
		if reconciled {
			phaseStart = time.Now()
			goto done
		}

		approvalFields := telemetry.Fields{
			"step":       string(stepName),
			"action":     string(response.action),
			"fix_review": sctx.Fixing,
		}
		if agentName := e.telemetryAgentName(); agentName != "" {
			approvalFields["agent"] = agentName
		}
		if selectedCount := selectedFindingCount(outcome.Findings, response.findingIDs); selectedCount > 0 {
			approvalFields["selected_findings_count"] = selectedCount
		}
		telemetry.Track("approval", approvalFields)

		switch response.action {
		case types.ActionApprove:
			// Approved - execution already frozen in executionMS, reset phaseStart
			// so the done label computes no additional elapsed.
			e.recordDeclinedRound(currentRoundID, outcome.Findings, stepName, roundNum)
			if err := e.applyApprovalOverride(step, sctx, sr.ID); err != nil {
				return false, "", err
			}
			phaseStart = time.Now()
			goto done

		case types.ActionSkip:
			// Skip - mark step skipped and return (not an error)
			e.recordDeclinedRound(currentRoundID, outcome.Findings, stepName, roundNum)
			if err := e.db.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, finalExitCode, executionMS, logPath); err != nil {
				return false, "", fmt.Errorf("complete step %s (skip): %w", stepName, err)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusSkipped), "", "", &executionMS)
			return false, "", nil

		case types.ActionAbort:
			e.recordDeclinedRound(currentRoundID, outcome.Findings, stepName, roundNum)
			if dbErr := e.db.FailStep(sr.ID, "aborted by user", executionMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "aborted by user", &executionMS)
			return false, "", fmt.Errorf("step %s: aborted by user", stepName)

		case types.ActionFix:
			telemetry.Track("fix", e.fixTelemetryFields("user", stepName, selectedFindingCount(outcome.Findings, response.findingIDs), 0))
			// Fix - mark step as fixing, resume execution timer, re-execute.
			phaseStart = time.Now()
			selectedCount := selectedFindingCount(outcome.Findings, response.findingIDs)
			writeLog(fmt.Sprintf("user-fix round starting after round %d (%d %s selected)", roundNum, selectedCount, pluralize(selectedCount, "finding", "findings")))
			if dbErr := e.db.UpdateStepStatus(sr.ID, types.StepStatusFixing); dbErr != nil {
				slog.Warn("failed to update step status in db", "step", stepName, "status", "fixing", "error", dbErr)
			}
			sctx.Fixing = true
			selectedFindings := filterFindingsJSON(outcome.Findings, response.findingIDs)
			mergedFindings := mergeUserOverridesJSON(selectedFindings, response.instructions, response.addedFindings)
			sctx.PreviousFindings = mergedFindings
			nextTrigger = "auto_fix"
			if currentRoundID != "" {
				allSelectedIDs := combineSelectedFindingIDs(response.findingIDs, mergedFindings)
				if idsJSON := marshalFindingIDs(allSelectedIDs); idsJSON != "" {
					var userFindingsJSON *string
					if mergedFindings != "" && mergedFindings != selectedFindings {
						userFindingsJSON = &mergedFindings
					}
					if dbErr := e.db.SetStepRoundUserDecision(currentRoundID, &idsJSON, db.RoundSelectionSourceUser, userFindingsJSON); dbErr != nil {
						slog.Warn("failed to record user decision", "step", stepName, "round", roundNum, "error", dbErr)
					}
				}
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFixing), "", "", nil)
			slog.Info("step fix requested, re-executing", "step", stepName)
			continue // loop back to step.Execute
		}
	}

done:
	// Mark step completed with execution-only timing.
	durationMS := executionMS + time.Since(phaseStart).Milliseconds()
	if durationOverrideMS > 0 {
		durationMS = durationOverrideMS
	}
	status := types.StepStatusCompleted
	if stepSkipped {
		status = types.StepStatusSkipped
	}
	// A review round's captured head becomes authority only when the review
	// actually completes. Parked outcomes stay in the loop above, failures
	// return earlier, and skipped reviews deliberately leave the binding empty.
	// Completion and authority replacement are one DB transaction.
	if stepName == types.StepReview && status == types.StepStatusCompleted && reviewApprovedHeadSHA != "" {
		if err := e.db.CompleteReviewStep(sr.ID, run.ID, reviewApprovedHeadSHA, finalExitCode, durationMS, logPath); err != nil {
			return false, "", fmt.Errorf("complete step %s: %w", stepName, err)
		}
		reviewedHead := reviewApprovedHeadSHA
		run.ReviewApprovedHeadSHA = &reviewedHead
		ClearUncertifiedPipelineRangeIfCertified(ctx, e.db, repo.ID, run.Branch, reviewedHead, workDir)
	} else if err := e.db.CompleteStepWithStatus(sr.ID, status, finalExitCode, durationMS, logPath); err != nil {
		return false, "", fmt.Errorf("complete step %s: %w", stepName, err)
	}
	e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(status), "", "", &durationMS)
	return skipRemaining, restartFrom, nil
}

// recordDeclinedRound persists an approve, skip, or abort resolution as a real
// decision instead of leaving no trace.
//
// Before this existed, those three resolutions wrote no finding-level state at
// all, so a round where the human read a blocking finding and said "ship it as
// is" was byte-identical to a round with no findings. Nothing downstream could
// tell the two apart, and the only durable statement of what the change must do
// stayed the user-intent prose - which is how a later step could re-derive and
// re-apply the very change the human had just declined.
//
// The decline is stored the way a partial selection already stores one: as the
// complement of selected_finding_ids. Writing an explicit empty array with the
// user_declined source is what makes "selected nothing" representable, since a
// NULL column means "no decision was recorded".
//
// Best effort by design. This is advisory prompt context for later steps, so a
// failed write degrades to today's behavior and must never fail the run.
// applyApprovalOverride is the single place both ActionApprove sites (the
// live wait in executeStep and the daemon-restart recovery path in Resume)
// route through before completing a step on approval. If step raised its gate
// over a live, re-checkable condition (ApprovalOverrideVerifier), this
// re-checks it once and, only when it is still unresolved, records the
// upcoming completion as an explicit override (db.SetStepOverrideReason)
// instead of a silent plain pass - see ApprovalOverrideVerifier's doc for the
// incident this exists to make impossible. It never blocks or changes the
// approval itself: a human's ActionApprove always proceeds, and a step that
// does not implement the interface (every step but CI today) is completely
// unaffected. A verification error fails closed - it is recorded as an
// unresolved condition, not silently treated as clear - but still never stops
// the approval, only what it gets recorded as.
//
// Persisting that override marker is itself fail-closed: every downstream
// surface (outcomeForRun, the run_completed CIOverrideReason delta, the TUI
// banner) derives override status solely from step_results.override_reason, so
// a swallowed write failure would complete the step as an ordinary clean pass -
// the exact false-green this feature exists to prevent. When the marker cannot
// be written this returns the error so the caller fails the run closed instead
// of recording that plain pass.
func (e *Executor) applyApprovalOverride(step Step, sctx *StepContext, stepResultID string) error {
	verifier, ok := step.(ApprovalOverrideVerifier)
	if !ok {
		return nil
	}
	unresolved, err := verifier.VerifyApprovalOverride(sctx)
	if err != nil {
		unresolved = fmt.Sprintf("could not verify: %v", err)
	}
	if unresolved == "" {
		return nil
	}
	if dbErr := e.db.SetStepOverrideReason(stepResultID, unresolved); dbErr != nil {
		return fmt.Errorf("record approval override reason for step %s: %w", step.Name(), dbErr)
	}
	return nil
}

func (e *Executor) recordDeclinedRound(roundID, findingsJSON string, stepName types.StepName, roundNum int) {
	if e == nil || e.db == nil || roundID == "" {
		return
	}
	if findingsCount(findingsJSON) == 0 {
		// Nothing was declined, so there is no decision to record.
		return
	}
	if err := e.db.SetStepRoundDeclined(roundID); err != nil {
		slog.Warn("failed to record declined findings", "step", stepName, "round", roundNum, "error", err)
	}
}

func roundInsertID(_ string, inserted *db.StepRound, err error) string {
	if err != nil || inserted == nil {
		return ""
	}
	return inserted.ID
}

type gateStepBoundaryAgent struct {
	inner agent.Agent
	phase types.StepName
}

func (a *gateStepBoundaryAgent) Name() string { return a.inner.Name() }

func (a *gateStepBoundaryAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	opts.Prompt = gateguidance.PromptBoundary(string(a.phase)) + opts.Prompt
	return a.inner.Run(ctx, opts)
}

func (a *gateStepBoundaryAgent) Close() error { return a.inner.Close() }

func (a *gateStepBoundaryAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *gateStepBoundaryAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *gateStepBoundaryAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *gateStepBoundaryAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

type lifecycleAgent struct {
	inner       agent.Agent
	onLifecycle func(agent.LifecycleEvent)
}

func (a *lifecycleAgent) Name() string {
	return a.inner.Name()
}

func (a *lifecycleAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	previous := opts.OnLifecycle
	opts.OnLifecycle = func(event agent.LifecycleEvent) {
		if previous != nil {
			previous(event)
		}
		if a.onLifecycle != nil {
			a.onLifecycle(event)
		}
	}
	return a.inner.Run(ctx, opts)
}

func (a *lifecycleAgent) Close() error {
	return a.inner.Close()
}

// SupportsSessionResume forwards the wrapped adapter's session capability so
// wrapping never hides it from the review loop's session manager.
func (a *lifecycleAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *lifecycleAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *lifecycleAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

const (
	maxStepActivityText          = 240
	stepActivityThrottleInterval = time.Second
)

func stepActivityFromLog(text string) string {
	end := len(text)
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	if end == 0 {
		return ""
	}
	start := strings.LastIndexByte(text[:end], '\n') + 1
	line := strings.TrimSpace(text[start:end])
	return "log: " + truncateActivity(line)
}

func truncateActivity(text string) string {
	if len(text) <= maxStepActivityText {
		return text
	}
	runeCount := 0
	for i := range text {
		if runeCount == maxStepActivityText {
			return text[:i] + "..."
		}
		runeCount++
	}
	return text
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// waitForApprovalOrReconcile blocks until a user action arrives, the parked
// gate's external source of truth makes it obsolete, or the context is
// cancelled. Reconciliation runs synchronously under a bounded child context,
// so no watcher goroutine can outlive approval, cancellation, or shutdown.
// The caller must set e.waiting and e.waitingStep before calling this method.
func (e *Executor) waitForApprovalOrReconcile(ctx context.Context, step Step, sctx *StepContext, immediate bool) (approvalResponse, bool, error) {
	defer func() {
		e.mu.Lock()
		e.waiting = false
		e.waitingStep = ""
		e.mu.Unlock()
		// Drain any stale response that arrived after context cancellation or
		// raced with an external reconciliation.
		select {
		case <-e.approvalCh:
		default:
		}
	}()

	if _, ok := step.(ApprovalGateReconciler); !ok {
		select {
		case response := <-e.approvalCh:
			return response, false, nil
		case <-ctx.Done():
			return approvalResponse{}, false, context.Cause(ctx)
		}
	}

	delay := e.gateReconcileInterval
	if immediate {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case response := <-e.approvalCh:
			return response, false, nil
		case <-ctx.Done():
			return approvalResponse{}, false, context.Cause(ctx)
		case <-timer.C:
			resolved, err := e.reconcileApprovalGate(ctx, step, sctx)
			if resolved {
				if e.claimGateReconciliation() {
					return approvalResponse{}, true, nil
				}
				return <-e.approvalCh, false, nil
			}
			if errors.Is(err, ErrFatalGateReconciliation) {
				return approvalResponse{}, false, err
			}
			if err != nil && ctx.Err() == nil {
				if sctx != nil && sctx.Log != nil {
					sctx.Log(fmt.Sprintf("warning: could not reconcile parked %s gate; preserving it: %v", step.Name(), err))
				} else {
					slog.Warn("could not reconcile parked approval gate; preserving it", "step", step.Name(), "error", err)
				}
			}
			timer.Reset(e.gateReconcileInterval)
		}
	}
}

func (e *Executor) claimGateReconciliation() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.waiting {
		return false
	}
	e.waiting = false
	e.waitingStep = ""
	return true
}

func (e *Executor) reconcileApprovalGate(ctx context.Context, step Step, sctx *StepContext) (bool, error) {
	reconciler, ok := step.(ApprovalGateReconciler)
	if !ok {
		return false, nil
	}
	timeout := e.gateReconcileTimeout
	if timeout <= 0 {
		timeout = defaultGateReconcileTimeout
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	copyCtx := *sctx
	copyCtx.Ctx = reconcileCtx
	return reconciler.ReconcileApprovalGate(&copyCtx)
}

// failRun marks a run as failed and returns the error.
// It accepts an optional context; if the context was cancelled with a cause,
// the cause message is used as the run's error (more informative than "context canceled").
func (e *Executor) failRun(run *db.Run, repo *db.Repo, err error, ctxs ...context.Context) error {
	errMsg := err.Error()
	for _, ctx := range ctxs {
		if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
			errMsg = cause.Error()
			break
		}
	}
	runStatus := types.RunFailed
	if errMsg == types.RunCancelReasonAbortedByUser || errMsg == types.RunCancelReasonSuperseded {
		runStatus = types.RunCancelled
	}
	verifiedHead, verified := e.reconcileTerminalRunHead(run)
	var dbErr error
	if verified {
		dbErr = e.db.UpdateRunErrorStatusWithVerifiedHead(run.ID, errMsg, runStatus, verifiedHead)
	} else {
		dbErr = e.db.UpdateRunErrorStatus(run.ID, errMsg, runStatus)
	}
	if dbErr != nil {
		slog.Error("failed to update run error status", "run", run.ID, "error", dbErr)
	} else if verified {
		run.HeadSHA = verifiedHead
	}
	run.Status = runStatus
	run.Error = &errMsg
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return err
}

func (e *Executor) completeRun(run *db.Run, repo *db.Repo) error {
	verifiedHead, verified := e.reconcileTerminalRunHead(run)
	var err error
	if verified {
		err = e.db.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCompleted, verifiedHead)
	} else {
		err = e.db.UpdateRunStatus(run.ID, types.RunCompleted)
	}
	if err != nil {
		return err
	}
	if verified {
		run.HeadSHA = verifiedHead
	}
	run.Status = types.RunCompleted
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return nil
}

func (e *Executor) reconcileTerminalRunHead(run *db.Run) (string, bool) {
	if run == nil || strings.TrimSpace(e.workDir) == "" {
		return "", false
	}
	recordedRun, err := e.db.GetRun(run.ID)
	if err != nil || recordedRun == nil {
		slog.Warn("failed to load run head before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	recorded := strings.TrimSpace(recordedRun.HeadSHA)
	if recorded == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed, err := git.HeadSHA(ctx, e.workDir)
	if err != nil {
		slog.Warn("failed to resolve worktree head before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	observed = strings.TrimSpace(observed)
	if observed == "" {
		return "", false
	}
	if observed == recorded {
		if !e.preserveUnpublishedTerminalHead(ctx, recordedRun, observed) {
			return "", false
		}
		return recorded, true
	}
	if _, err := git.Run(ctx, e.workDir, "merge-base", "--is-ancestor", recorded, observed); err != nil {
		slog.Warn("worktree head is not a verified descendant before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	if !e.preserveUnpublishedTerminalHead(ctx, recordedRun, observed) {
		return "", false
	}
	return observed, true
}

func (e *Executor) preserveUnpublishedTerminalHead(ctx context.Context, run *db.Run, head string) bool {
	if run == nil || head == "" {
		return false
	}
	published := ""
	if run.LastPushedSHA != nil {
		published = *run.LastPushedSHA
	}
	if published == "" {
		if run.SubmittedHeadSHA != nil {
			published = *run.SubmittedHeadSHA
		}
	}
	if head == published {
		return true
	}
	if err := custody.PreserveRecoveryHead(ctx, e.workDir, run.ID, head); err != nil {
		slog.Warn("failed to anchor unpublished terminal head", "run", run.ID, "head", head, "error", err)
		return false
	}
	return true
}

// --- event helpers ---

func (e *Executor) emitRunEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo) {
	status := string(run.Status)
	event := ipc.Event{
		Type:   eventType,
		RunID:  run.ID,
		RepoID: repo.ID,
		Status: &status,
		Branch: &run.Branch,
		Error:  run.Error,
		PRURL:  run.PRURL,
	}
	// A completed run may have passed with a CI approval override; the TUI
	// banner reads the reason off the delta (like PRURL) so it never needs a
	// snapshot to distinguish it from a genuinely green run. Derived from step
	// rows so both ActionApprove sites (live wait and Resume) are covered.
	// Gated on the terminal status, not the event type: errorRun emits the same
	// event for failed/cancelled runs, whose banner never reads it.
	if run.Status == types.RunCompleted {
		if reason := e.runOverrideReason(run.ID); reason != "" {
			event.CIOverrideReason = &reason
		}
	}
	e.onEvent(event)
}

// runOverrideReason returns the first step OverrideReason recorded for the run,
// deriving the run-level CI override reason the same way daemon.runToInfo does.
func (e *Executor) runOverrideReason(runID string) string {
	steps, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return ""
	}
	for _, s := range steps {
		if s.OverrideReason != nil && *s.OverrideReason != "" {
			return *s.OverrideReason
		}
	}
	return ""
}

func (e *Executor) emitCIReadinessEvent(run *db.Run, repo *db.Repo, ready, declaredNoCI bool) {
	declaredNoCI = ready && declaredNoCI
	e.onEvent(ipc.Event{
		Type:        ipc.EventCIReadinessChanged,
		RunID:       run.ID,
		RepoID:      repo.ID,
		CIReady:     &ready,
		CIReadyNoCI: &declaredNoCI,
	})
}

func (e *Executor) emitStepEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string) {
	e.emitStepEventWithFindings(eventType, run, repo, stepName, status, "")
}

func (e *Executor) emitStepEventWithFindings(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string) {
	e.emitStepEventWithFindingsAndError(eventType, run, repo, stepName, status, findings, "", nil)
}

func (e *Executor) emitStepEventWithFindingsAndError(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string, errMsg string, durationMS *int64) {
	event := ipc.Event{
		Type:       eventType,
		RunID:      run.ID,
		RepoID:     repo.ID,
		StepName:   &stepName,
		Status:     &status,
		DurationMS: durationMS,
	}
	stats := e.findingStatsForStep(run.ID, stepName)
	if stats.ReportedFindings > 0 || stats.FixedFindings > 0 {
		reported := stats.ReportedFindings
		fixed := stats.FixedFindings
		event.ReportedFindings = &reported
		event.FixedFindings = &fixed
	}
	if errMsg != "" {
		event.Error = &errMsg
	}
	if findings != "" {
		event.Findings = &findings
	}
	e.onEvent(event)
	if !shouldTrackStepTelemetry(eventType, status) {
		return
	}

	fields := telemetry.Fields{
		"event":  string(eventType),
		"step":   string(stepName),
		"status": status,
	}
	if agentName := e.telemetryAgentName(); agentName != "" {
		fields["agent"] = agentName
	}
	if durationMS != nil {
		fields["duration_ms"] = *durationMS
	}
	if findings != "" {
		fields["findings_count"] = findingsCount(findings)
	}
	telemetry.Track("step", fields)
}

func (e *Executor) findingStatsForStep(runID string, stepName types.StepName) db.StepStats {
	steps, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return db.StepStats{StepName: stepName}
	}
	for _, step := range steps {
		if step.StepName != stepName {
			continue
		}
		stats, err := e.db.StepFindingStats(step)
		if err != nil {
			return db.StepStats{StepName: stepName}
		}
		return stats
	}
	return db.StepStats{StepName: stepName}
}

func shouldTrackStepTelemetry(eventType ipc.EventType, status string) bool {
	if eventType != ipc.EventStepCompleted {
		return false
	}
	switch types.StepStatus(status) {
	case types.StepStatusAwaitingApproval, types.StepStatusFixReview, types.StepStatusFailed:
		return true
	default:
		return false
	}
}

func (e *Executor) emitLogChunk(run *db.Run, repo *db.Repo, stepName types.StepName, content string) {
	e.onEvent(ipc.Event{
		Type:     ipc.EventLogChunk,
		RunID:    run.ID,
		RepoID:   repo.ID,
		StepName: &stepName,
		Content:  &content,
	})
}

func (e *Executor) telemetryAgentName() string {
	if e.config == nil || e.config.Agent == "" {
		return ""
	}
	return string(e.config.Agent)
}

func (e *Executor) fixTelemetryFields(source string, stepName types.StepName, selectedCount int, attempt int) telemetry.Fields {
	fields := telemetry.Fields{
		"source":                  source,
		"step":                    string(stepName),
		"selected_findings_count": selectedCount,
	}
	if agentName := e.telemetryAgentName(); agentName != "" {
		fields["agent"] = agentName
	}
	if attempt > 0 {
		fields["attempt"] = attempt
	}
	return fields
}

func findingsCount(raw string) int {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return 0
	}
	return len(findings.Items)
}

func selectedFindingCount(raw string, ids []string) int {
	if len(ids) > 0 {
		return len(ids)
	}
	return findingsCount(raw)
}
