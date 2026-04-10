package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// EventFunc is called when a pipeline event occurs, for streaming to subscribers.
type EventFunc func(ipc.Event)

type approvalResponse struct {
	action     types.ApprovalAction
	findingIDs []string
}

// Executor runs pipeline steps sequentially and coordinates approval interactions.
type Executor struct {
	db     *db.DB
	paths  *paths.Paths
	config *config.Config
	agent  agent.Agent
	steps  []Step

	onEvent EventFunc

	mu          sync.Mutex
	approvalCh  chan approvalResponse // buffered channel for approval responses
	waiting     bool                  // true when blocked on approval
	waitingStep types.StepName        // which step is currently awaiting approval
}

// NewExecutor creates a pipeline executor.
func NewExecutor(database *db.DB, p *paths.Paths, cfg *config.Config, ag agent.Agent, steps []Step, onEvent EventFunc) *Executor {
	if onEvent == nil {
		onEvent = func(ipc.Event) {}
	}
	return &Executor{
		db:         database,
		paths:      p,
		config:     cfg,
		agent:      ag,
		steps:      steps,
		onEvent:    onEvent,
		approvalCh: make(chan approvalResponse, 1),
	}
}

// Respond sends a user approval action to the currently waiting step.
// The step parameter must match the step currently awaiting approval.
// Returns an error if no step is awaiting approval or if the step name doesn't match.
func (e *Executor) Respond(step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	e.mu.Lock()
	waiting := e.waiting
	waitingStep := e.waitingStep
	e.mu.Unlock()

	if !waiting {
		return fmt.Errorf("no step awaiting approval")
	}

	if step != waitingStep {
		return fmt.Errorf("step mismatch: responding to %q but %q is awaiting approval", step, waitingStep)
	}

	e.approvalCh <- approvalResponse{action: action, findingIDs: findingIDs}
	return nil
}

// Execute runs the pipeline steps sequentially for a given run.
// The workDir is the directory where steps execute (typically a git worktree).
// If the context is cancelled with a cause (via context.WithCancelCause),
// the cause message is preserved as the run's error in the DB.
func (e *Executor) Execute(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	// Mark run as running
	if err := e.db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	run.Status = types.RunRunning
	e.emitRunEvent(ipc.EventRunUpdated, run, repo)

	// Create log directory for this run
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}

	// Create step result records in DB
	stepRecords := make(map[types.StepName]*db.StepResult)
	for _, step := range e.steps {
		sr, err := e.db.InsertStepResult(run.ID, step.Name())
		if err != nil {
			return e.failRun(run, repo, fmt.Errorf("insert step result: %w", err))
		}
		stepRecords[step.Name()] = sr
	}

	// Execute steps sequentially
	for _, step := range e.steps {
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx))
		}

		sr := stepRecords[step.Name()]
		if err := e.executeStep(ctx, step, sr, run, repo, workDir, logDir); err != nil {
			return e.failRun(run, repo, err, ctx)
		}
	}

	// Mark run as completed
	if err := e.db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	run.Status = types.RunCompleted
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return nil
}

// executeStep runs a single step with approval coordination.
func (e *Executor) executeStep(ctx context.Context, step Step, sr *db.StepResult, run *db.Run, repo *db.Repo, workDir, logDir string) error {
	stepName := step.Name()
	logPath := filepath.Join(logDir, string(stepName)+".log")
	finalExitCode := 0

	// Mark step as running
	if err := e.db.StartStep(sr.ID); err != nil {
		return fmt.Errorf("start step %s: %w", stepName, err)
	}
	e.emitStepEvent(ipc.EventStepStarted, run, repo, stepName, string(types.StepStatusRunning))

	started := time.Now()

	// Open log file for persistent step logging
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("create step log file %s: %w", stepName, err)
	}
	defer logFile.Close()

	// Build step context with log callback that emits events and writes to file
	sctx := &StepContext{
		Ctx:     ctx,
		Run:     run,
		Repo:    repo,
		WorkDir: workDir,
		Agent:   e.agent,
		Config:  e.config,
		DB:      e.db,
		Log: func(text string) {
			e.emitLogChunk(run, repo, stepName, text)
			fmt.Fprintln(logFile, text)
		},
	}

	// Execute with possible fix loop
	for {
		outcome, err := step.Execute(sctx)
		if err != nil {
			e.db.FailStep(sr.ID, err.Error())
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", err.Error())
			return fmt.Errorf("step %s failed: %w", stepName, err)
		}

		outcome.Findings = normalizeFindingsJSON(outcome.Findings, string(stepName))
		finalExitCode = outcome.ExitCode

		if outcome.Findings != "" {
			e.db.SetStepFindings(sr.ID, outcome.Findings)
		} else {
			e.db.ClearStepFindings(sr.ID)
		}

		if !outcome.NeedsApproval {
			// Step completed without needing approval
			break
		}

		// Determine approval status: fix_review after a fix cycle, awaiting_approval otherwise
		approvalStatus := types.StepStatusAwaitingApproval
		var diffText string
		if sctx.Fixing {
			approvalStatus = types.StepStatusFixReview
			// Compute working tree diff to show what the agent changed
			if d, err := git.DiffHead(ctx, workDir); err == nil && d != "" {
				diffText = d
			}
		}

		// Step needs approval — wait for user action
		e.db.UpdateStepStatus(sr.ID, approvalStatus)
		e.emitStepEventWithFindingsAndDiff(ipc.EventStepCompleted, run, repo, stepName, string(approvalStatus), outcome.Findings, diffText)

		response, err := e.waitForApproval(ctx, stepName)
		if err != nil {
			e.db.FailStep(sr.ID, err.Error())
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", err.Error())
			return fmt.Errorf("step %s: waiting for approval: %w", stepName, err)
		}

		switch response.action {
		case types.ActionApprove:
			// Approved — break out of fix loop
			goto done

		case types.ActionSkip:
			// Skip — mark step skipped and return (not an error)
			durationMS := time.Since(started).Milliseconds()
			if err := e.db.CompleteStep(sr.ID, finalExitCode, durationMS, logPath); err != nil {
				return fmt.Errorf("complete step %s (skip): %w", stepName, err)
			}
			e.db.UpdateStepStatus(sr.ID, types.StepStatusSkipped)
			e.emitStepEvent(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusSkipped))
			return nil

		case types.ActionAbort:
			e.db.FailStep(sr.ID, "aborted by user")
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", "aborted by user")
			return fmt.Errorf("step %s: aborted by user", stepName)

		case types.ActionFix:
			// Fix — mark step as fixing, re-execute with previous findings
			e.db.UpdateStepStatus(sr.ID, types.StepStatusFixing)
			sctx.Fixing = true
			sctx.PreviousFindings = filterFindingsJSON(outcome.Findings, response.findingIDs)
			slog.Info("step fix requested, re-executing", "step", stepName)
			continue // loop back to step.Execute
		}
	}

done:
	// Mark step completed with timing
	durationMS := time.Since(started).Milliseconds()
	if err := e.db.CompleteStep(sr.ID, finalExitCode, durationMS, logPath); err != nil {
		return fmt.Errorf("complete step %s: %w", stepName, err)
	}
	e.emitStepEvent(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusCompleted))
	return nil
}

// waitForApproval blocks until a user action arrives or context is cancelled.
func (e *Executor) waitForApproval(ctx context.Context, stepName types.StepName) (approvalResponse, error) {
	e.mu.Lock()
	e.waiting = true
	e.waitingStep = stepName
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.waiting = false
		e.waitingStep = ""
		e.mu.Unlock()
	}()

	select {
	case response := <-e.approvalCh:
		return response, nil
	case <-ctx.Done():
		return approvalResponse{}, context.Cause(ctx)
	}
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
	e.db.UpdateRunError(run.ID, errMsg)
	run.Status = types.RunFailed
	run.Error = &errMsg
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return err
}

// --- event helpers ---

func (e *Executor) emitRunEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo) {
	status := string(run.Status)
	e.onEvent(ipc.Event{
		Type:   eventType,
		RunID:  run.ID,
		RepoID: repo.ID,
		Status: &status,
		Branch: &run.Branch,
		Error:  run.Error,
	})
}

func (e *Executor) emitStepEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string) {
	e.emitStepEventWithFindings(eventType, run, repo, stepName, status, "")
}

func (e *Executor) emitStepEventWithFindings(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string) {
	e.emitStepEventWithFindingsAndDiff(eventType, run, repo, stepName, status, findings, "")
}

func (e *Executor) emitStepEventWithFindingsAndDiff(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string, diff string) {
	e.emitStepEventWithFindingsDiffAndError(eventType, run, repo, stepName, status, findings, diff, "")
}

func (e *Executor) emitStepEventWithFindingsDiffAndError(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string, diff string, errMsg string) {
	event := ipc.Event{
		Type:     eventType,
		RunID:    run.ID,
		RepoID:   repo.ID,
		StepName: &stepName,
		Status:   &status,
	}
	if errMsg != "" {
		event.Error = &errMsg
	}
	if findings != "" {
		event.Findings = &findings
	}
	if diff != "" {
		event.Diff = &diff
	}
	e.onEvent(event)
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
