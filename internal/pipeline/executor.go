package pipeline

import (
	"context"
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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// EventFunc is called when a pipeline event occurs, for streaming to subscribers.
type EventFunc func(ipc.Event)

type approvalResponse struct {
	actionID      string
	action        types.ApprovalAction
	findingIDs    []string
	instructions  map[string]string
	addedFindings []types.Finding
}

type userFixPayload struct {
	findingsJSON string
	findingIDs   []string
	hasOverrides bool
}

// buildUserFixPayload applies overrides to the complete finding set before
// selecting the repair batch. Merging first lets user-added findings avoid IDs
// already used by unselected findings while retaining per-finding instructions
// on the selected originals.
func buildUserFixPayload(raw string, response approvalResponse) userFixPayload {
	mergedAll := mergeUserOverridesJSON(raw, response.instructions, response.addedFindings)
	ids := append([]string(nil), response.findingIDs...)
	if len(response.addedFindings) > 0 {
		originalCount := 0
		if original, err := types.ParseFindingsJSON(raw); err == nil {
			originalCount = len(original.Items)
		}
		if merged, err := types.ParseFindingsJSON(mergedAll); err == nil && originalCount <= len(merged.Items) {
			for _, finding := range merged.Items[originalCount:] {
				ids = append(ids, finding.ID)
			}
		}
	}
	seen := make(map[string]bool, len(ids))
	uniqueIDs := ids[:0]
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniqueIDs = append(uniqueIDs, id)
	}
	ids = uniqueIDs
	return userFixPayload{
		findingsJSON: filterFindingsJSON(mergedAll, ids),
		findingIDs:   ids,
		hasOverrides: len(response.instructions) > 0 || len(response.addedFindings) > 0,
	}
}

// Executor runs pipeline steps sequentially and coordinates approval interactions.
type Executor struct {
	db     *db.DB
	paths  *paths.Paths
	config *config.Config
	agent  agent.Agent
	steps  []Step
	skips  map[types.StepName]bool

	onEvent EventFunc

	// sessions manages this run's durable review-loop agent sessions; shared
	// carries run-scoped step-to-step results. Both are created per Execute.
	sessions *RunSessions
	shared   *RunShared

	mu          sync.Mutex
	approvalCh  chan approvalResponse // buffered channel for durable approval responses
	waiting     bool                  // true when blocked on approval
	waitingStep types.StepName        // which step is currently awaiting approval
	waitingGate *db.ApprovalGate      // durable gate that the response must match
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

func (e *Executor) enabledStepAfter(current, target types.StepName) bool {
	afterCurrent := false
	for _, step := range e.steps {
		if afterCurrent && step.Name() == target {
			return !e.skips[target]
		}
		if step.Name() == current {
			afterCurrent = true
		}
	}
	return false
}

func (e *Executor) validateConfiguredSkips() error {
	if e.skips[types.StepVerify] && e.enabledStepAfter(types.StepVerify, types.StepPush) {
		return fmt.Errorf("cannot skip Verify while Push is enabled")
	}
	return nil
}

func (e *Executor) rejectUnsafeVerifySkip(step types.StepName, action types.ApprovalAction) error {
	if step == types.StepVerify && action == types.ActionSkip && e.enabledStepAfter(types.StepVerify, types.StepPush) {
		return fmt.Errorf("cannot skip Verify while Push is enabled")
	}
	return nil
}

func (e *Executor) requireAggregateVerifiedCandidate(runID string) error {
	verifyRequired := false
	for _, step := range e.steps {
		if step.Name() == types.StepPush {
			break
		}
		if step.Name() == types.StepVerify {
			verifyRequired = true
		}
	}
	if !verifyRequired {
		return nil
	}
	results, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return fmt.Errorf("load Verify result before Push: %w", err)
	}
	var verifyResult *db.StepResult
	for _, result := range results {
		if result.StepName == types.StepVerify {
			verifyResult = result
			break
		}
	}
	if verifyResult == nil || verifyResult.Status != types.StepStatusCompleted {
		return fmt.Errorf("Push requires a successful Verify result tied to the aggregate-verified candidate")
	}
	seal, err := e.db.LatestSeal(runID)
	if err != nil {
		return fmt.Errorf("load publication seal before Push: %w", err)
	}
	reviewed, err := e.db.LatestSealByReason(runID, "reviewed")
	if err != nil {
		return fmt.Errorf("load aggregate-verified seal before Push: %w", err)
	}
	if seal == nil || reviewed == nil || reviewed.SHA != seal.SHA {
		return fmt.Errorf("Push requires an aggregate-verified seal for the exact publication candidate")
	}
	return nil
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
	return e.RespondWithOverrides(step, action, findingIDs, nil, nil)
}

// RespondWithOverrides is like Respond but also carries per-finding user
// instructions and user-authored findings. Both are merged into the round's
// findings on a fix action before the fix agent runs.
func (e *Executor) RespondWithOverrides(step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.waiting || e.waitingGate == nil {
		return fmt.Errorf("no step awaiting approval")
	}
	if step != e.waitingStep {
		return fmt.Errorf("step mismatch: responding to %q but %q is awaiting approval", step, e.waitingStep)
	}
	if err := e.rejectUnsafeVerifySkip(step, action); err != nil {
		return err
	}
	response, input, err := approvalActionForGate(e.waitingGate, action, findingIDs, instructions, addedFindings)
	if err != nil {
		return err
	}
	persisted, err := e.db.InsertApprovalAction(input)
	if err != nil {
		return fmt.Errorf("persist approval action: %w", err)
	}
	response.actionID = persisted.ID
	e.waiting = false
	e.approvalCh <- response
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
	if err := e.validateConfiguredSkips(); err != nil {
		return e.failRun(run, repo, err, ctx)
	}

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

	// Execute steps sequentially
	// One run-wide provider-circuit state, shared by every routed step. A new
	// gate starts with all circuits closed.
	circuits := newProviderCircuits()

	for i, step := range e.steps {
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx))
		}

		sr := stepRecords[step.Name()]
		if e.skips[step.Name()] {
			if err := e.db.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				return e.failRun(run, repo, fmt.Errorf("skip step %s: %w", step.Name(), err), ctx)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, step.Name(), string(types.StepStatusSkipped), "", "", "", nil)
			// A skipped Lint still ends the pre-Verify mutator sequence: seal the
			// candidate the earlier mutators produced so Push has a sealed SHA.
			if step.Name() == types.StepLint {
				if sealErr := e.sealCandidate(ctx, run, workDir); sealErr != nil {
					return e.failRun(run, repo, sealErr, ctx)
				}
			}
			continue
		}
		skipRemaining, err := e.executeStep(ctx, step, sr, run, repo, workDir, logDir, stepExecutionState{}, circuits)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		// Seal the immutable publish candidate once the last pre-Verify content
		// mutator (Lint) has completed, so Verify and Push operate on a fixed SHA.
		if !skipRemaining && step.Name() == types.StepLint {
			if sealErr := e.sealCandidate(ctx, run, workDir); sealErr != nil {
				return e.failRun(run, repo, sealErr, ctx)
			}
		}
		if skipRemaining {
			// Mark all subsequent steps as skipped
			for _, remaining := range e.steps[i+1:] {
				rsr := stepRecords[remaining.Name()]
				if dbErr := e.db.CompleteStepWithStatus(rsr.ID, types.StepStatusSkipped, 0, 0, ""); dbErr != nil {
					slog.Warn("failed to finalize skipped step", "step", remaining.Name(), "error", dbErr)
				}
				e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, remaining.Name(), string(types.StepStatusSkipped), "", "", "", nil)
			}
			break
		}
	}

	return e.completeRun(ctx, run, repo, workDir)
}

func (e *Executor) initializeRunScopes(runID string) {
	sessionsEnabled := e.config != nil && e.config.SessionReuse
	e.sessions = NewRunSessions(e.db, runID, e.agent, sessionsEnabled)
	e.shared = &RunShared{}
}

func (e *Executor) restoreProviderCircuits(runID string) (*providerCircuits, error) {
	attempts, err := e.db.GetInvocationAttemptsByRun(runID)
	if err != nil {
		return nil, err
	}
	return providerCircuitsFromAttempts(attempts), nil
}

func (e *Executor) requireResolvedBlockingRepairs(runID string, stepName types.StepName) error {
	unresolved, err := e.db.HasUnresolvedBlockingRepair(runID)
	if err != nil {
		return fmt.Errorf("check unresolved repairs before approval: %w", err)
	}
	if unresolved {
		return fmt.Errorf("%s cannot be approved while a blocking finding lineage remains unresolved", stepName)
	}
	return nil
}

func (e *Executor) completeRun(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	if err := e.maybeActivateCanary(ctx, run, workDir); err != nil {
		return e.failRun(run, repo, err, ctx)
	}
	if err := e.db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	run.Status = types.RunCompleted
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	e.recordCanaryCohort(ctx, run, workDir)
	return nil
}

type stepExecutionState struct {
	fixing                    bool
	previousFindings          string
	roundNum                  int
	executionMS               int64
	currentRoundID            string
	approvalActionID          string
	consentedSourceFindings   string
	consentedRepairFindings   string
	consentedRepairChecksJSON string
	consentedIDs              []string
	aggregateVerified         bool
}

type recoveredGate struct {
	index       int
	step        Step
	stepResult  *db.StepResult
	durableGate *db.ApprovalGate
	findings    string
	round       int
	lastRoundID string
}

type recoveredAppliedFix struct {
	gate                recoveredGate
	action              *db.ApprovalAction
	payload             userFixPayload
	aggregateRound      *db.StepRound
	openNonRepairRounds []*db.StepRound
}

func ValidateRecoveredRun(database *db.DB, run *db.Run, steps []Step) error {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		return fmt.Errorf("run is not a recoverable parked run")
	}
	_, err := (&Executor{db: database, steps: steps}).recoveredGate(run.ID)
	return err
}

// ValidateRecoveredPrefix proves that an active, non-parked run consists of a
// completed/skipped prefix followed only by pending steps. An all-terminal
// sequence is valid: the daemon may have crashed after the final step but
// before the run itself was marked completed.
func ValidateRecoveredPrefix(database *db.DB, run *db.Run, steps []Step) error {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince != nil {
		return fmt.Errorf("run is not a recoverable completed prefix")
	}
	executor := &Executor{db: database, steps: steps}
	if appliedFix, err := executor.recoveredAppliedFix(run.ID); err != nil {
		return err
	} else if appliedFix != nil {
		return nil
	}
	start, err := executor.recoveredPrefixStart(run.ID)
	if err != nil {
		return err
	}
	for index := range start {
		if steps[index].Name() != types.StepPush {
			continue
		}
		if err := executor.requireAggregateVerifiedCandidate(run.ID); err != nil {
			return fmt.Errorf("recovered published prefix is unsafe: %w", err)
		}
		seal, err := database.LatestSeal(run.ID)
		if err != nil {
			return fmt.Errorf("load recovered publication seal: %w", err)
		}
		if seal == nil || seal.SHA != run.HeadSHA {
			return fmt.Errorf("recovered published prefix is not pinned to the run head")
		}
	}
	return nil
}

func (e *Executor) recoveredPrefixStart(runID string) (int, error) {
	results, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return 0, fmt.Errorf("get recovered steps: %w", err)
	}
	if len(results) != len(e.steps) {
		return 0, fmt.Errorf("recovered run has %d step records for %d steps", len(results), len(e.steps))
	}
	firstPending := len(results)
	for index, result := range results {
		if result.StepName != e.steps[index].Name() {
			return 0, fmt.Errorf("recovered step %d is %q, want %q", index, result.StepName, e.steps[index].Name())
		}
		switch result.Status {
		case types.StepStatusCompleted, types.StepStatusSkipped:
			if firstPending != len(results) {
				return 0, fmt.Errorf("recovered step %s is %s after pending step", result.StepName, result.Status)
			}
			if result.CompletedAt == nil || result.ExitCode == nil || result.DurationMS == nil || result.LogPath == nil || result.Error != nil || result.AgentPID != nil {
				return 0, fmt.Errorf("recovered terminal step %s is incomplete", result.StepName)
			}
			if result.Status == types.StepStatusCompleted && result.StartedAt == nil {
				return 0, fmt.Errorf("recovered completed step %s was never started", result.StepName)
			}
		case types.StepStatusPending:
			if firstPending == len(results) {
				firstPending = index
			}
			if result.StartedAt != nil || result.CompletedAt != nil || result.ExitCode != nil || result.DurationMS != nil || result.LogPath != nil || result.FindingsJSON != nil || result.Error != nil || result.AgentPID != nil || result.AutoFixLimit != nil {
				return 0, fmt.Errorf("recovered pending step %s contains execution state", result.StepName)
			}
		default:
			return 0, fmt.Errorf("recovered step %s has non-reconstructable status %s", result.StepName, result.Status)
		}
	}
	return firstPending, nil
}

func (e *Executor) recoveredAppliedFix(runID string) (*recoveredAppliedFix, error) {
	results, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get recovered steps: %w", err)
	}
	if len(results) != len(e.steps) {
		return nil, fmt.Errorf("recovered run has %d step records for %d steps", len(results), len(e.steps))
	}
	hasFixing := false
	for _, result := range results {
		if result.Status == types.StepStatusFixing {
			hasFixing = true
			break
		}
	}
	if !hasFixing {
		return nil, nil
	}
	fixIndex := -1
	for index, result := range results {
		if result.StepName != e.steps[index].Name() {
			return nil, fmt.Errorf("recovered step %d is %q, want %q", index, result.StepName, e.steps[index].Name())
		}
		if result.Status == types.StepStatusFixing {
			if fixIndex != -1 {
				return nil, fmt.Errorf("recovered run has multiple applied approval fixes")
			}
			fixIndex = index
			continue
		}
		if fixIndex == -1 {
			if result.Status != types.StepStatusCompleted && result.Status != types.StepStatusSkipped {
				return nil, fmt.Errorf("recovered step %s is %s before applied approval fix", result.StepName, result.Status)
			}
			if result.CompletedAt == nil || result.ExitCode == nil || result.DurationMS == nil || result.LogPath == nil || result.Error != nil || result.AgentPID != nil {
				return nil, fmt.Errorf("recovered terminal step %s is incomplete", result.StepName)
			}
			if result.Status == types.StepStatusCompleted && result.StartedAt == nil {
				return nil, fmt.Errorf("recovered completed step %s was never started", result.StepName)
			}
			continue
		}
		if result.Status != types.StepStatusPending {
			return nil, fmt.Errorf("recovered step %s is %s after applied approval fix", result.StepName, result.Status)
		}
		if result.StartedAt != nil || result.CompletedAt != nil || result.ExitCode != nil || result.DurationMS != nil || result.LogPath != nil || result.FindingsJSON != nil || result.Error != nil || result.AgentPID != nil || result.AutoFixLimit != nil {
			return nil, fmt.Errorf("recovered pending step %s contains execution state", result.StepName)
		}
	}
	stepResult := results[fixIndex]
	if stepResult.StartedAt == nil || stepResult.DurationMS == nil || stepResult.AgentPID != nil {
		return nil, fmt.Errorf("recovered applied approval fix is incomplete")
	}
	gate, err := e.db.GetCurrentApprovalGate(stepResult.ID)
	if err != nil {
		return nil, fmt.Errorf("load applied approval fix gate: %w", err)
	}
	if gate == nil || gate.RunID != runID || gate.StepResultID != stepResult.ID {
		return nil, fmt.Errorf("recovered applied approval fix has no current gate")
	}
	action, err := e.db.GetApprovalAction(gate.ID)
	if err != nil {
		return nil, fmt.Errorf("load applied approval fix action: %w", err)
	}
	if action == nil || action.Action != types.ActionFix || action.AppliedAt == nil || action.RunID != runID || action.StepResultID != stepResult.ID || action.StepRoundID != gate.SourceRoundID {
		return nil, fmt.Errorf("recovered applied approval fix has no durable fix action")
	}
	response, err := approvalResponseFromRecord(action)
	if err != nil {
		return nil, fmt.Errorf("decode applied approval fix: %w", err)
	}
	payload := buildUserFixPayload(gate.FindingsJSON, response)
	round, err := e.db.GetStepRound(gate.SourceRoundID)
	if err != nil {
		return nil, fmt.Errorf("load applied approval fix source round: %w", err)
	}
	if round == nil || round.StepResultID != stepResult.ID || round.State != db.StepRoundCompleted {
		return nil, fmt.Errorf("recovered applied approval fix source round is invalid")
	}
	selection := marshalFindingIDs(payload.findingIDs)
	if selection == "" || round.SelectedFindingIDs == nil || *round.SelectedFindingIDs != selection || round.SelectionSource == nil || *round.SelectionSource != db.RoundSelectionSourceUser {
		return nil, fmt.Errorf("recovered applied approval fix selection is incomplete")
	}
	if payload.hasOverrides && (round.UserFindingsJSON == nil || *round.UserFindingsJSON != payload.findingsJSON) {
		return nil, fmt.Errorf("recovered applied approval fix findings are incomplete")
	}
	allRounds, err := e.db.GetAllRoundsByStep(stepResult.ID)
	if err != nil {
		return nil, fmt.Errorf("load applied approval fix child rounds: %w", err)
	}
	repairs, err := e.db.GetFindingRepairsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("load applied approval fix repairs: %w", err)
	}
	repairRounds := make(map[string]bool, len(repairs))
	for _, repair := range repairs {
		if repair.StepResultID == stepResult.ID {
			repairRounds[repair.StepRoundID] = true
		}
	}
	latestRound := round.Round
	var aggregateRound *db.StepRound
	var openNonRepairRounds []*db.StepRound
	for _, child := range allRounds {
		if child.Round <= round.Round {
			continue
		}
		if child.Round > latestRound {
			latestRound = child.Round
		}
		switch child.State {
		case db.StepRoundCompleted:
			if repairRounds[child.ID] {
				continue
			}
			attempts, attemptErr := e.db.GetInvocationAttemptsByRound(child.ID)
			if attemptErr != nil {
				return nil, fmt.Errorf("load completed applied-fix child attempts: %w", attemptErr)
			}
			if hasSucceededAggregateAttempt(attempts) {
				aggregateRound = child
			}
		case db.StepRoundReserved:
			if repairRounds[child.ID] {
				continue
			}
			openNonRepairRounds = append(openNonRepairRounds, child)
			attempts, attemptErr := e.db.GetInvocationAttemptsByRound(child.ID)
			if attemptErr != nil {
				return nil, fmt.Errorf("load open applied-fix child attempts: %w", attemptErr)
			}
			if hasSucceededAggregateAttempt(attempts) {
				aggregateRound = child
			}
		case db.StepRoundFailed, db.StepRoundCancelled:
		default:
			return nil, fmt.Errorf("recovered applied approval fix has child round %d in unknown state %q", child.Round, child.State)
		}
	}
	return &recoveredAppliedFix{
		gate: recoveredGate{
			index:       fixIndex,
			step:        e.steps[fixIndex],
			stepResult:  stepResult,
			durableGate: gate,
			findings:    gate.FindingsJSON,
			round:       latestRound,
			lastRoundID: round.ID,
		},
		action:              action,
		payload:             payload,
		aggregateRound:      aggregateRound,
		openNonRepairRounds: openNonRepairRounds,
	}, nil
}
func hasSucceededAggregateAttempt(attempts []*db.InvocationAttempt) bool {
	for _, attempt := range attempts {
		if attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeSucceeded {
			continue
		}
		switch attempt.Start.Purpose {
		case types.PurposeNormalAggregateVerification, types.PurposeEscalatedAggregateVerification:
			return true
		}
	}
	return false
}

func (e *Executor) recoveredFixRepairsResolved(recovered *recoveredAppliedFix) (bool, error) {
	for _, findingID := range recovered.payload.findingIDs {
		lineageID := fmt.Sprintf("approval:%s:%s", recovered.action.ID, findingID)
		repairs, err := e.db.GetFindingRepairsByLineage(lineageID)
		if err != nil {
			return false, fmt.Errorf("load recovered Verify repair lineage: %w", err)
		}
		if len(repairs) == 0 || repairs[len(repairs)-1].Status != db.RepairStatusResolved {
			return false, nil
		}
	}
	return len(recovered.payload.findingIDs) > 0, nil
}

func (e *Executor) aggregateSealMatches(runID, headSHA string) (bool, error) {
	seal, err := e.db.LatestSeal(runID)
	if err != nil {
		return false, fmt.Errorf("load recovered aggregate publication seal: %w", err)
	}
	reviewed, err := e.db.LatestSealByReason(runID, "reviewed")
	if err != nil {
		return false, fmt.Errorf("load recovered aggregate-reviewed seal: %w", err)
	}
	return seal != nil && reviewed != nil && seal.SHA == headSHA && reviewed.SHA == headSHA, nil
}

func (e *Executor) reconcileRecoveredAppliedFix(recovered *recoveredAppliedFix, headSHA string) (bool, error) {
	aggregateVerified := false
	for _, round := range recovered.openNonRepairRounds {
		attempts, err := e.db.GetInvocationAttemptsByRound(round.ID)
		if err != nil {
			return false, fmt.Errorf("load recovered open child attempts: %w", err)
		}
		hasActive := false
		for _, attempt := range attempts {
			if attempt.Terminal != nil {
				continue
			}
			hasActive = true
			if err := e.db.FinishInvocationAttempt(attempt.ID, types.InvocationAttemptTerminal{
				Outcome: types.InvocationOutcomeInterrupted,
			}); err != nil {
				return false, fmt.Errorf("interrupt recovered open child attempt: %w", err)
			}
		}
		reconstructableAggregate := recovered.aggregateRound != nil &&
			recovered.aggregateRound.ID == round.ID &&
			!hasActive &&
			hasSucceededAggregateAttempt(attempts)
		if reconstructableAggregate {
			repairsResolved, err := e.recoveredFixRepairsResolved(recovered)
			if err != nil {
				return false, err
			}
			sealMatches, err := e.aggregateSealMatches(recovered.action.RunID, headSHA)
			if err != nil {
				return false, err
			}
			if repairsResolved && sealMatches {
				if err := e.db.CompleteReservedStepRound(round.ID, nil, nil, round.DurationMS); err != nil {
					return false, fmt.Errorf("complete recovered aggregate Verify round: %w", err)
				}
				aggregateVerified = true
				continue
			}
		}
		if err := e.db.TerminateReservedStepRound(round.ID, db.StepRoundCancelled, round.DurationMS); err != nil {
			return false, fmt.Errorf("cancel unreconstructable recovered child round: %w", err)
		}
	}
	if recovered.aggregateRound != nil && recovered.aggregateRound.State == db.StepRoundCompleted {
		attempts, err := e.db.GetInvocationAttemptsByRound(recovered.aggregateRound.ID)
		if err != nil {
			return false, fmt.Errorf("load recovered aggregate Verify attempts: %w", err)
		}
		for _, attempt := range attempts {
			if attempt.Terminal == nil {
				return false, fmt.Errorf("completed recovered aggregate Verify round has an active invocation")
			}
		}
		repairsResolved, err := e.recoveredFixRepairsResolved(recovered)
		if err != nil {
			return false, err
		}
		sealMatches, err := e.aggregateSealMatches(recovered.action.RunID, headSHA)
		if err != nil {
			return false, err
		}
		if !repairsResolved || !sealMatches {
			return false, fmt.Errorf("completed recovered aggregate Verify round lacks matching durable repair and seal evidence")
		}
		aggregateVerified = true
	}
	if aggregateVerified {
		if err := e.db.ClearStepFindings(recovered.gate.stepResult.ID); err != nil {
			return false, fmt.Errorf("clear aggregate-verified recovered findings: %w", err)
		}
	}
	return aggregateVerified, nil
}

func (e *Executor) resumeRecoveredAppliedFix(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, recovered *recoveredAppliedFix) error {
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("resolve recovered worktree head: %w", err)
	}
	if headSHA != run.HeadSHA {
		return fmt.Errorf("recovered worktree head does not match run head")
	}
	aggregateVerified, err := e.reconcileRecoveredAppliedFix(recovered, headSHA)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("reconcile recovered applied fix: %w", err), ctx)
	}
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}
	e.initializeRunScopes(run.ID)
	circuits, err := e.restoreProviderCircuits(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("restore provider circuits: %w", err), ctx)
	}
	duration := recoveredStepDuration(recovered.gate.stepResult)
	e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, recovered.gate.step.Name(), string(types.StepStatusFixing), "", "", "", &duration)
	skipRemaining, err := e.executeStep(ctx, recovered.gate.step, recovered.gate.stepResult, run, repo, workDir, logDir, stepExecutionState{
		fixing:                    true,
		previousFindings:          recovered.payload.findingsJSON,
		roundNum:                  recovered.gate.round,
		executionMS:               duration,
		currentRoundID:            recovered.gate.lastRoundID,
		approvalActionID:          recovered.action.ID,
		consentedSourceFindings:   recovered.gate.findings,
		consentedRepairFindings:   recovered.payload.findingsJSON,
		consentedRepairChecksJSON: recovered.gate.durableGate.RepairChecksJSON,
		consentedIDs:              recovered.payload.findingIDs,
		aggregateVerified:         aggregateVerified,
	}, circuits)
	if err != nil {
		return e.failRun(run, repo, err, ctx)
	}
	if skipRemaining {
		return e.skipRecoveredRemainder(ctx, run, repo, workDir, recovered.gate.index+1)
	}
	if recovered.gate.step.Name() == types.StepLint {
		if err := e.sealCandidate(ctx, run, workDir); err != nil {
			return e.failRun(run, repo, err, ctx)
		}
	}
	return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, recovered.gate.index+1, circuits)
}

// ResumeRecoveredPrefix continues a previously validated completed/skipped
// prefix at its first pending step, or finalizes an all-terminal run. It never
// replays a terminal step.
func (e *Executor) ResumeRecoveredPrefix(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	if repo == nil {
		return fmt.Errorf("recovered run has no repository")
	}
	if err := ValidateRecoveredPrefix(e.db, run, e.steps); err != nil {
		return err
	}
	appliedFix, err := e.recoveredAppliedFix(run.ID)
	if err != nil {
		return err
	}
	if appliedFix != nil {
		return e.resumeRecoveredAppliedFix(ctx, run, repo, workDir, appliedFix)
	}
	start, err := e.recoveredPrefixStart(run.ID)
	if err != nil {
		return err
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("resolve recovered worktree head: %w", err)
	}
	if headSHA != run.HeadSHA {
		return fmt.Errorf("recovered worktree head does not match run head")
	}
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}
	e.initializeRunScopes(run.ID)
	circuits, err := e.restoreProviderCircuits(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("restore provider circuits: %w", err), ctx)
	}
	return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, start, circuits)
}

// Resume restores a run that was durably parked at an approval gate when the
// daemon stopped. It only accepts a fully recorded gate and otherwise returns
// an error so startup recovery can fail the run rather than guessing.
func (e *Executor) Resume(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
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
	circuits, err := e.restoreProviderCircuits(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("restore provider circuits: %w", err), ctx)
	}

	e.emitStepEventWithFindingsDiffAndError(
		ipc.EventStepCompleted,
		run,
		repo,
		gate.step.Name(),
		string(gate.stepResult.Status),
		gate.findings,
		"",
		"",
		gate.stepResult.DurationMS,
	)
	parkStart := time.Unix(*run.AwaitingAgentSince, 0)
	persistedAction, err := e.db.GetPendingApprovalAction(gate.durableGate.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("load pending approval action: %w", err), ctx)
	}
	var response approvalResponse
	if persistedAction != nil {
		response, err = approvalResponseFromRecord(persistedAction)
	} else {
		e.mu.Lock()
		e.waiting = true
		e.waitingStep = gate.step.Name()
		e.waitingGate = gate.durableGate
		e.mu.Unlock()
		response, err = e.waitForApproval(ctx, gate.step.Name())
	}
	if err != nil {
		if persistedAction == nil {
			_ = e.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds())
		}
		duration := recoveredStepDuration(gate.stepResult)
		if dbErr := e.db.FailStep(gate.stepResult.ID, err.Error(), duration); dbErr != nil {
			slog.Warn("failed to mark recovered step as failed in db", "step", gate.step.Name(), "error", dbErr)
		}
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", "", err.Error(), &duration)
		return e.failRun(run, repo, fmt.Errorf("step %s: waiting for approval: %w", gate.step.Name(), err), ctx)
	}
	duration := recoveredStepDuration(gate.stepResult)
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
		if err := e.requireResolvedBlockingRepairs(run.ID, gate.step.Name()); err != nil {
			if rejectErr := e.db.RejectApproval(db.RejectApprovalInput{
				ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
				ExitCode: recoveredExitCode(gate.stepResult), DurationMS: duration, LogPath: recoveredLogPath(gate.stepResult), Error: err.Error(),
			}); rejectErr != nil {
				return e.failRun(run, repo, fmt.Errorf("%w; finalize rejected approval: %v", err, rejectErr), ctx)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", "", err.Error(), &duration)
			return err
		}
		if err := e.db.ApplyApprovalTerminal(db.ApplyApprovalTerminalInput{
			ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
			Status: types.StepStatusCompleted, ExitCode: recoveredExitCode(gate.stepResult), DurationMS: duration, LogPath: recoveredLogPath(gate.stepResult),
		}); err != nil {
			return e.failRun(run, repo, fmt.Errorf("apply recovered approved step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusCompleted), "", "", "", &duration)
		if gate.step.Name() == types.StepLint {
			if err := e.sealCandidate(ctx, run, workDir); err != nil {
				return e.failRun(run, repo, err, ctx)
			}
		}
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, circuits)
	case types.ActionSkip:
		if err := e.db.ApplyApprovalTerminal(db.ApplyApprovalTerminalInput{
			ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
			Status: types.StepStatusSkipped, ExitCode: recoveredExitCode(gate.stepResult), DurationMS: duration, LogPath: recoveredLogPath(gate.stepResult),
		}); err != nil {
			return e.failRun(run, repo, fmt.Errorf("apply recovered skipped step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusSkipped), "", "", "", &duration)
		if gate.step.Name() == types.StepLint {
			if err := e.sealCandidate(ctx, run, workDir); err != nil {
				return e.failRun(run, repo, err, ctx)
			}
		}
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, circuits)
	case types.ActionAbort:
		if err := e.db.ApplyApprovalTerminal(db.ApplyApprovalTerminalInput{
			ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
			Status: types.StepStatusFailed, ExitCode: recoveredExitCode(gate.stepResult), DurationMS: duration, LogPath: recoveredLogPath(gate.stepResult), Error: "aborted by user",
		}); err != nil {
			return e.failRun(run, repo, fmt.Errorf("apply recovered aborted step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", "", "aborted by user", &duration)
		return e.failRun(run, repo, fmt.Errorf("step %s: aborted by user", gate.step.Name()), ctx)
	case types.ActionFix:
		telemetry.Track("fix", e.fixTelemetryFields("user", gate.step.Name(), selectedFindingCount(gate.findings, response.findingIDs), 0))
		payload := buildUserFixPayload(gate.findings, response)
		selection := marshalFindingIDs(payload.findingIDs)
		var selectedIDs *string
		if selection != "" {
			selectedIDs = &selection
		}
		var userFindings *string
		if payload.hasOverrides {
			userFindings = &payload.findingsJSON
		}
		if err := e.db.ApplyApprovalFix(db.ApplyApprovalFixInput{
			ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
			SelectedIDsJSON: selectedIDs, UserFindingsJSON: userFindings,
		}); err != nil {
			return e.failRun(run, repo, fmt.Errorf("apply recovered user fix for step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFixing), "", "", "", nil)
		skipRemaining, err := e.executeStep(ctx, gate.step, gate.stepResult, run, repo, workDir, logDir, stepExecutionState{
			fixing:                    true,
			previousFindings:          payload.findingsJSON,
			roundNum:                  gate.round,
			executionMS:               duration,
			currentRoundID:            gate.lastRoundID,
			approvalActionID:          response.actionID,
			consentedSourceFindings:   gate.findings,
			consentedRepairFindings:   payload.findingsJSON,
			consentedRepairChecksJSON: gate.durableGate.RepairChecksJSON,
			consentedIDs:              payload.findingIDs,
		}, circuits)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(ctx, run, repo, workDir, gate.index+1)
		}
		if gate.step.Name() == types.StepLint {
			if err := e.sealCandidate(ctx, run, workDir); err != nil {
				return e.failRun(run, repo, err, ctx)
			}
		}
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, circuits)
	default:
		return e.failRun(run, repo, fmt.Errorf("step %s: unsupported approval action %q", gate.step.Name(), response.action), ctx)
	}
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
			source, sourceErr := e.recoveredGateSourceRound(result.StepName, rounds, *result.FindingsJSON)
			if sourceErr != nil {
				return nil, sourceErr
			}
			if source == nil {
				return nil, fmt.Errorf("recovered approval gate findings are incomplete")
			}
			durableGate, gateErr := e.db.GetCurrentApprovalGate(result.ID)
			if gateErr != nil {
				return nil, fmt.Errorf("load durable recovered approval gate: %w", gateErr)
			}
			if durableGate == nil ||
				durableGate.RunID != runID ||
				durableGate.StepResultID != result.ID ||
				durableGate.SourceRoundID != source.ID ||
				durableGate.Status != result.Status ||
				durableGate.FindingsJSON != *result.FindingsJSON ||
				durableGate.DurationMS != *result.DurationMS {
				return nil, fmt.Errorf("recovered approval gate does not match its durable snapshot")
			}
			gate = &recoveredGate{
				index:       index,
				step:        e.steps[index],
				stepResult:  result,
				durableGate: durableGate,
				findings:    durableGate.FindingsJSON,
				round:       rounds[len(rounds)-1].Round,
				lastRoundID: durableGate.SourceRoundID,
			}
			continue
		}
		if gate == nil {
			if result.Status != types.StepStatusCompleted && result.Status != types.StepStatusSkipped {
				return nil, fmt.Errorf("recovered step %s is %s before approval gate", result.StepName, result.Status)
			}
			continue
		}
		if result.Status != types.StepStatusPending {
			return nil, fmt.Errorf("recovered step %s is %s after approval gate", result.StepName, result.Status)
		}
	}
	if gate == nil {
		return nil, fmt.Errorf("recovered run has no approval gate")
	}
	return gate, nil
}

func (e *Executor) recoveredGateSourceRound(stepName types.StepName, rounds []*db.StepRound, findings string) (*db.StepRound, error) {
	if len(rounds) == 0 {
		return nil, nil
	}
	current, err := types.ParseFindingsJSON(findings)
	if err != nil {
		return nil, fmt.Errorf("parse recovered approval gate findings: %w", err)
	}
	if len(current.Items) == 0 {
		return rounds[len(rounds)-1], nil
	}

	owned := make([]bool, len(current.Items))
	roundByID := make(map[string]*db.StepRound, len(rounds))
	var source *db.StepRound
	advanceSource := func(candidate *db.StepRound) {
		if candidate != nil && (source == nil || candidate.Round > source.Round) {
			source = candidate
		}
	}
	for _, round := range rounds {
		roundByID[round.ID] = round
		if round.FindingsJSON == nil {
			continue
		}
		produced, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err != nil {
			return nil, fmt.Errorf("parse recovered round %d findings: %w", round.Round, err)
		}
		for currentIndex, finding := range current.Items {
			for _, producedFinding := range produced.Items {
				if producedFinding == finding {
					owned[currentIndex] = true
					advanceSource(round)
					break
				}
			}
		}
	}
	for _, isOwned := range owned {
		if !isOwned {
			return nil, nil
		}
	}

	stepResult, err := e.db.GetStepResult(rounds[0].StepResultID)
	if err != nil {
		return nil, fmt.Errorf("load recovered step result: %w", err)
	}
	repairs, err := e.db.GetFindingRepairsByRun(stepResult.RunID)
	if err != nil {
		return nil, fmt.Errorf("load recovered finding repairs: %w", err)
	}
	for _, repair := range repairs {
		if repair.StepResultID != stepResult.ID {
			continue
		}
		repairRound := roundByID[repair.StepRoundID]
		if repairRound == nil {
			return nil, fmt.Errorf("recovered finding repair references an unknown round")
		}
		for _, finding := range current.Items {
			if repair.Severity == finding.Severity &&
				repair.Action == finding.Action &&
				repair.Description == finding.Description &&
				repair.File == finding.File &&
				repair.Line == finding.Line {
				advanceSource(repairRound)
				break
			}
		}
	}
	if stepName == types.StepReview {
		for index := len(rounds) - 1; index >= 0; index-- {
			round := rounds[index]
			attempts, err := e.db.GetInvocationAttemptsByRound(round.ID)
			if err != nil {
				return nil, fmt.Errorf("load recovered review attempts: %w", err)
			}
			for _, attempt := range attempts {
				if attempt.Start.Purpose == types.PurposeInitialReview && attempt.Terminal != nil && attempt.Terminal.Outcome == types.InvocationOutcomeSucceeded {
					return round, nil
				}
			}
		}
	}
	return source, nil
}

func (e *Executor) executeRecoveredRemainder(ctx context.Context, run *db.Run, repo *db.Repo, workDir, logDir string, start int, circuits *providerCircuits) error {
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("get recovered steps: %w", err), ctx)
	}
	for index := start; index < len(e.steps); index++ {
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx), ctx)
		}
		if index >= len(results) || results[index].StepName != e.steps[index].Name() || results[index].Status != types.StepStatusPending {
			return e.failRun(run, repo, fmt.Errorf("recovered step plan changed at %d", index), ctx)
		}
		skipRemaining, err := e.executeStep(ctx, e.steps[index], results[index], run, repo, workDir, logDir, stepExecutionState{}, circuits)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(ctx, run, repo, workDir, index+1)
		}
		if e.steps[index].Name() == types.StepLint {
			if err := e.sealCandidate(ctx, run, workDir); err != nil {
				return e.failRun(run, repo, err, ctx)
			}
		}
	}
	return e.completeRun(ctx, run, repo, workDir)
}

func (e *Executor) skipRecoveredRemainder(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, start int) error {
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
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, e.steps[index].Name(), string(types.StepStatusSkipped), "", "", "", nil)
	}
	return e.completeRun(ctx, run, repo, workDir)
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

// sealCandidate records an immutable publish candidate after the last pre-Verify
// content mutator: it requires a clean worktree, captures the exact HEAD, and
// appends a new seal so Verify and Push operate on a fixed SHA. A dirty worktree
// fails closed - an earlier mutator that left uncommitted changes must be fixed
// at its source rather than swept into the published candidate.
func (e *Executor) sealCandidate(ctx context.Context, run *db.Run, workDir string) error {
	status, err := git.Run(ctx, workDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("seal candidate: read worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("seal candidate: worktree is dirty, refusing to seal the publish candidate")
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("seal candidate: resolve HEAD: %w", err)
	}
	if _, err := e.db.CreateSeal(run.ID, head, "pre_verify"); err != nil {
		return err
	}
	slog.Info("sealed publish candidate", "run", run.ID, "sha", head)
	return nil
}

// executeStep runs a single step with approval coordination.
// Returns (skipRemaining, error).
func (e *Executor) executeStep(ctx context.Context, step Step, sr *db.StepResult, run *db.Run, repo *db.Repo, workDir, logDir string, state stepExecutionState, circuits *providerCircuits) (bool, error) {
	stepName := step.Name()
	logPath := filepath.Join(logDir, string(stepName)+".log")
	finalExitCode := 0
	if stepName == types.StepPush {
		if err := e.requireAggregateVerifiedCandidate(run.ID); err != nil {
			return false, err
		}
	}

	if !state.fixing {
		if err := e.db.StartStep(sr.ID); err != nil {
			return false, fmt.Errorf("start step %s: %w", stepName, err)
		}
		e.emitStepEvent(ipc.EventStepStarted, run, repo, stepName, string(types.StepStatusRunning))
	}

	// Track execution-only time, excluding approval wait periods.
	phaseStart := time.Now()
	executionMS := state.executionMS
	var durationOverrideMS int64 // sum of step-reported overrides (demo mode)

	// Open log file for persistent step logging
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, fmt.Errorf("create step log file %s: %w", stepName, err)
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
		default:
			if dbErr := e.db.TouchStepActivity(sr.ID, text); dbErr != nil {
				slog.Warn("failed to touch step activity in db", "step", stepName, "error", dbErr)
			}
		}
		writeLog(text)
	}
	// roundNum is shared with the perf wrapper's closure below and advances
	// immediately before each round is reserved, so telemetry sees that round.
	roundNum := state.roundNum

	stepAgent := e.agent
	if stepAgent != nil {
		stepAgent = &lifecycleAgent{inner: stepAgent, onLifecycle: onAgentLifecycle}
		stepAgent = &perfRecordingAgent{
			inner:    stepAgent,
			db:       e.db,
			runID:    run.ID,
			stepName: stepName,
			round:    func() int { return roundNum },
		}
	}
	routingCfg := config.DefaultRoutingConfig()
	if e.config != nil && !e.config.Routing.IsZero() {
		routingCfg = e.config.Routing
	}
	invoker := newRoutingInvoker(routingCfg, e.db, circuits)
	invoker.newAgent = func(name types.AgentName, executable string) (agent.Agent, error) {
		if stepAgent != nil {
			// Test seam: route every Candidate launch to the injected recording
			// agent instead of spawning a real native binary.
			return stepAgent, nil
		}
		native, err := agent.New(name, executable, nil)
		if err != nil {
			return nil, err
		}
		steered := agent.WithSteering(native)
		return &lifecycleAgent{inner: steered, onLifecycle: onAgentLifecycle}, nil
	}
	sctx := &StepContext{
		Ctx:              ctx,
		Run:              run,
		Repo:             repo,
		WorkDir:          workDir,
		Agent:            stepAgent,
		Invoker:          invoker,
		Config:           e.config,
		DB:               e.db,
		StepResultID:     sr.ID,
		UserIntent:       userIntent,
		IntentSource:     userIntentSource,
		Sessions:         e.sessions,
		Shared:           e.shared,
		Fixing:           state.fixing,
		PreviousFindings: state.previousFindings,
		Log:              writeLog,
		LogChunk:         writeLogChunk,
		LogFile: func(text string) {
			fmt.Fprintln(logFile, text)
			touchLogActivity(text, true)
		},
	}

	nextTrigger := "initial"
	if sctx.Fixing {
		nextTrigger = "auto_fix"
	}
	skipRemaining := false
	stepSkipped := false
	approvalTerminal := false
	approvalTerminalDuration := int64(0)
	currentRoundID := state.currentRoundID
	postRepairRereview := false
	var recoveredOutcome *StepOutcome
	if state.consentedRepairFindings != "" && !state.aggregateVerified {
		if stepName == types.StepReview && e.routingActive() {
			remaining, err := e.repairConsentedReviewAtGate(ctx, sctx, run, sr, state.consentedSourceFindings, state.consentedRepairFindings, state.consentedIDs, currentRoundID, &roundNum)
			if err != nil {
				return false, err
			}
			if remaining == "" {
				sctx.Fixing = false
				postRepairRereview = true
				nextTrigger = "auto_fix"
			} else {
				sctx.Fixing = true
				recoveredOutcome = &StepOutcome{NeedsApproval: true, Findings: remaining}
			}
		} else if stepName != types.StepReview {
			recoveredChecks, err := decodeDurableRepairChecks(stepName, state.consentedRepairChecksJSON, workDir, sctx.Env)
			if err != nil {
				return false, err
			}
			reserveRepairRound := func(trigger string) (*db.StepRound, error) {
				roundNum++
				return e.db.ReserveStepRound(sr.ID, roundNum, trigger)
			}
			result, err := e.repairConsentedStepFindings(
				ctx, sctx, run, sr, stepName, state.approvalActionID,
				state.consentedRepairFindings, state.consentedIDs, recoveredChecks, reserveRepairRound,
			)
			if err != nil {
				return false, fmt.Errorf("repair recovered user-consented %s findings: %w", stepName, err)
			}
			if !result.Owned || !result.Resolved {
				return false, fmt.Errorf("recovered user-consented %s repair did not independently resolve every selected blocking finding", stepName)
			}
			remaining, err := removeFindingsByID(state.consentedSourceFindings, result.ResolvedIDs)
			if err != nil {
				return false, fmt.Errorf("remove recovered resolved %s findings: %w", stepName, err)
			}
			if remaining == "" {
				if err := e.db.ClearStepFindings(sr.ID); err != nil {
					return false, fmt.Errorf("clear recovered resolved %s findings: %w", stepName, err)
				}
				sctx.Fixing = false
				if stepName == types.StepVerify {
					if err := e.sealCandidate(ctx, run, workDir); err != nil {
						return false, fmt.Errorf("reseal before recovered aggregate Verify: %w", err)
					}
					postRepairRereview = true
					nextTrigger = "auto_fix"
				} else {
					recoveredOutcome = &StepOutcome{RepairChecks: recoveredChecks}
				}
			} else {
				if err := e.db.SetStepFindings(sr.ID, remaining); err != nil {
					return false, fmt.Errorf("persist recovered remaining %s findings: %w", stepName, err)
				}
				sctx.Fixing = true
				recoveredOutcome = &StepOutcome{NeedsApproval: true, Findings: remaining, RepairChecks: recoveredChecks}
			}
		}
	}

	if state.aggregateVerified {
		goto done
	}

	// Execute with possible fix loop.
	for {
		roundNum++
		currentRound, reserveErr := e.db.ReserveStepRound(sr.ID, roundNum, nextTrigger)
		if reserveErr != nil {
			durationMS := executionMS + time.Since(phaseStart).Milliseconds()
			if dbErr := e.db.FailStep(sr.ID, reserveErr.Error(), durationMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", reserveErr.Error(), &durationMS)
			return false, fmt.Errorf("reserve step %s round: %w", stepName, reserveErr)
		}
		currentRoundID = currentRound.ID
		sctx.CurrentRound = currentRound
		sctx.InvocationScope = types.InvocationScope{
			Kind:         types.InvocationScopePipeline,
			RunID:        run.ID,
			StepResultID: sr.ID,
			StepRoundID:  currentRound.ID,
		}
		outcome := recoveredOutcome
		var err error
		if outcome == nil {
			outcome, err = step.Execute(sctx)
		} else {
			recoveredOutcome = nil
		}
		roundDuration := time.Since(phaseStart).Milliseconds()
		if err != nil {
			durationMS := executionMS + roundDuration
			roundState := db.StepRoundFailed
			if ctx.Err() != nil {
				roundState = db.StepRoundCancelled
			}
			if dbErr := e.db.TerminateReservedStepRound(currentRoundID, roundState, roundDuration); dbErr != nil {
				slog.Warn("failed to terminate step round", "step", stepName, "round", roundNum, "error", dbErr)
			}
			// Persist the failure reason to the step's own log file. The error
			// often carries the only detail of why the step failed (e.g. git
			// stderr from a rejected push); without this the step log shows the
			// work starting but never why it stopped.
			fmt.Fprintf(logFile, "\nerror: %s\n", err.Error())
			touchLogActivity("error: "+err.Error(), true)
			if dbErr := e.db.FailStep(sr.ID, err.Error(), durationMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", err.Error(), &durationMS)
			return false, fmt.Errorf("step %s failed: %w", stepName, err)
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

		// Append this execution's outcome to the round reserved before launch.
		var findingsPtr *string
		if outcome.Findings != "" {
			findingsPtr = &outcome.Findings
		}
		var fixSummaryPtr *string
		if outcome.FixSummary != "" {
			s := outcome.FixSummary
			fixSummaryPtr = &s
		}
		if dbErr := e.db.CompleteReservedStepRound(currentRoundID, findingsPtr, fixSummaryPtr, roundDuration); dbErr != nil {
			durationMS := executionMS + roundDuration
			fmt.Fprintf(logFile, "\nerror: complete step round: %s\n", dbErr.Error())
			if failErr := e.db.FailStep(sr.ID, dbErr.Error(), durationMS); failErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", failErr)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", dbErr.Error(), &durationMS)
			return false, fmt.Errorf("complete step %s round: %w", stepName, dbErr)
		}
		// Give every returned routed initial-review finding a durable run-wide
		// root lineage tied to the Candidate attempt that surfaced it. Lineage
		// is load-bearing for repair and approval, so routed review cannot
		// continue when these facts are not durable.
		if err := e.recordReviewLineages(run, stepName, currentRoundID, outcome.Findings); err != nil {
			return false, fmt.Errorf("record initial review lineages: %w", err)
		}

		// Route this step's blocking findings through the common repair
		// coordinator before the approval gate: a fresh fixer, the step's
		// deterministic checks, then a fresh strong verifier, escalating through
		// the routed cascade. Runs only on the initial round. When the
		// coordinator owns a non-review step's repair, the legacy per-step
		// auto-fix loop is skipped for that step.
		coordinatorResult := repairResult{}
		if !sctx.Fixing && !postRepairRereview {
			reserveRepairRound := func(trigger string) (*db.StepRound, error) {
				roundNum++
				return e.db.ReserveStepRound(sr.ID, roundNum, trigger)
			}
			var repairErr error
			if stepName == types.StepReview {
				coordinatorResult, repairErr = e.maybeRepairReviewFinding(ctx, sctx, run, sr, repo.DefaultBranch, currentRoundID, outcome.Findings, reserveRepairRound)
			} else {
				coordinatorResult, repairErr = e.maybeRepairStepFindings(ctx, sctx, run, sr, stepName, outcome.Findings, outcome.RepairChecks, reserveRepairRound)
			}
			if repairErr != nil {
				return false, fmt.Errorf("repair %s findings: %w", stepName, repairErr)
			}
			if len(coordinatorResult.NewFindings) > 0 || len(coordinatorResult.ResolvedIDs) > 0 {
				updated, err := mergeRepairFindingsJSON(outcome.Findings, coordinatorResult.NewFindings)
				if err != nil {
					return false, fmt.Errorf("merge verifier-created %s findings: %w", stepName, err)
				}
				updated, err = removeFindingsByID(updated, coordinatorResult.ResolvedIDs)
				if err != nil {
					return false, fmt.Errorf("remove resolved %s findings: %w", stepName, err)
				}
				if updated == "" {
					if err := e.db.ClearStepFindings(sr.ID); err != nil {
						return false, fmt.Errorf("clear resolved %s findings: %w", stepName, err)
					}
				} else if err := e.db.SetStepFindings(sr.ID, updated); err != nil {
					return false, fmt.Errorf("persist current %s findings: %w", stepName, err)
				}
				outcome.Findings = updated
			}
			if coordinatorResult.Owned {
				outcome.NeedsApproval = !coordinatorResult.Resolved ||
					hasBlockingFindingsJSON(outcome.Findings) ||
					hasAskUserFindingsJSON(outcome.Findings)
			}
		}
		if stepName == types.StepReview && coordinatorResult.Owned && coordinatorResult.Resolved &&
			!hasBlockingFindingsJSON(outcome.Findings) && !hasAskUserFindingsJSON(outcome.Findings) {
			// A targeted verifier can resolve the repaired lineage, but it does
			// not replace the reviewer's full adversarial pass over the complete
			// branch. Run exactly one full rereview in the durable reviewer
			// session, without re-entering automatic repair from that rereview.
			postRepairRereview = true
			nextTrigger = "auto_fix"
			continue
		}

		// A targeted verifier can resolve the repaired Verify finding, but it
		// cannot certify the complete candidate for publication. Seal the
		// repaired HEAD, then re-enter Verify once through its full aggregate
		// invocation. The post-repair flag prevents the aggregate result from
		// recursively entering automatic repair in the same pass.
		if coordinatorResult.Owned && coordinatorResult.Resolved && stepName == types.StepVerify {
			if sealErr := e.sealCandidate(ctx, run, workDir); sealErr != nil {
				return false, fmt.Errorf("reseal before aggregate Verify after repair: %w", sealErr)
			}
			postRepairRereview = true
			nextTrigger = "auto_fix"
			continue
		}

		// If the step produced a PR URL, propagate it to the run and emit an update.
		if outcome.PRURL != "" {
			run.PRURL = &outcome.PRURL
			e.emitRunEvent(ipc.EventRunUpdated, run, repo)
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

	approval:
		// Determine approval status: fix_review after a fix cycle, awaiting_approval otherwise
		approvalStatus := types.StepStatusAwaitingApproval
		var diffText string
		if sctx.Fixing {
			approvalStatus = types.StepStatusFixReview
			// Compute working tree diff to show what the agent changed
			if d, err := git.DiffHead(ctx, workDir); err != nil {
				slog.Warn("failed to compute diff for fix review", "error", err)
			} else if d != "" {
				diffText = d
			}
		}

		rounds, sourceErr := e.db.GetRoundsByStep(sr.ID)
		if sourceErr != nil {
			return false, fmt.Errorf("load %s rounds before approval: %w", stepName, sourceErr)
		}
		sourceRound, sourceErr := e.recoveredGateSourceRound(stepName, rounds, outcome.Findings)
		if sourceErr != nil {
			return false, fmt.Errorf("validate %s approval findings: %w", stepName, sourceErr)
		}
		if sourceRound == nil {
			return false, fmt.Errorf("%s approval findings are corrupt or not attributable to a completed round", stepName)
		}
		repairChecksJSON, checkErr := encodeDurableRepairChecks(ctx, stepName, outcome.RepairChecks, run, repo, workDir)
		if checkErr != nil {
			return false, fmt.Errorf("persist %s approval repair checks: %w", stepName, checkErr)
		}
		gate, parkErr := e.db.ParkApprovalGate(db.ParkApprovalGateInput{
			RunID:            run.ID,
			StepResultID:     sr.ID,
			SourceRoundID:    sourceRound.ID,
			Status:           approvalStatus,
			FindingsJSON:     outcome.Findings,
			RepairChecksJSON: repairChecksJSON,
			DurationMS:       executionMS,
		})
		if parkErr != nil {
			return false, fmt.Errorf("park %s approval gate: %w", stepName, parkErr)
		}
		currentRoundID = sourceRound.ID
		parkStart := time.Now()
		e.mu.Lock()
		e.waiting = true
		e.waitingStep = stepName
		e.waitingGate = gate
		e.mu.Unlock()
		e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(approvalStatus), outcome.Findings, diffText, "", &executionMS)

		response, err := e.waitForApproval(ctx, stepName)
		if err != nil {
			if dbErr := e.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds()); dbErr != nil {
				err = fmt.Errorf("%w; clear approval park: %v", err, dbErr)
			}
			if dbErr := e.db.FailStep(sr.ID, err.Error(), executionMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", err.Error(), &executionMS)
			return false, fmt.Errorf("step %s: waiting for approval: %w", stepName, err)
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
			if err := e.requireResolvedBlockingRepairs(run.ID, stepName); err != nil {
				if rejectErr := e.db.RejectApproval(db.RejectApprovalInput{
					ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
					ExitCode: finalExitCode, DurationMS: executionMS, LogPath: logPath, Error: err.Error(),
				}); rejectErr != nil {
					return false, fmt.Errorf("%w; finalize rejected approval: %v", err, rejectErr)
				}
				e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", err.Error(), &executionMS)
				return false, err
			}
			approvalDuration := executionMS
			if durationOverrideMS > 0 {
				approvalDuration = durationOverrideMS
			}
			if err := e.db.ApplyApprovalTerminal(db.ApplyApprovalTerminalInput{
				ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
				Status: types.StepStatusCompleted, ExitCode: finalExitCode, DurationMS: approvalDuration, LogPath: logPath,
			}); err != nil {
				return false, fmt.Errorf("apply approved %s step: %w", stepName, err)
			}
			approvalTerminal = true
			approvalTerminalDuration = approvalDuration
			goto done

		case types.ActionSkip:
			if err := e.db.ApplyApprovalTerminal(db.ApplyApprovalTerminalInput{
				ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
				Status: types.StepStatusSkipped, ExitCode: finalExitCode, DurationMS: executionMS, LogPath: logPath,
			}); err != nil {
				return false, fmt.Errorf("apply skipped %s step: %w", stepName, err)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusSkipped), "", "", "", &executionMS)
			return false, nil

		case types.ActionAbort:
			if err := e.db.ApplyApprovalTerminal(db.ApplyApprovalTerminalInput{
				ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
				Status: types.StepStatusFailed, ExitCode: finalExitCode, DurationMS: executionMS, LogPath: logPath, Error: "aborted by user",
			}); err != nil {
				return false, fmt.Errorf("apply aborted %s step: %w", stepName, err)
			}
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "", "aborted by user", &executionMS)
			return false, fmt.Errorf("step %s: aborted by user", stepName)

		case types.ActionFix:
			telemetry.Track("fix", e.fixTelemetryFields("user", stepName, selectedFindingCount(outcome.Findings, response.findingIDs), 0))
			// Fix - mark step as fixing, resume execution timer, re-execute.
			phaseStart = time.Now()
			selectedCount := selectedFindingCount(outcome.Findings, response.findingIDs)
			writeLog(fmt.Sprintf("user-fix round starting after round %d (%d %s selected)", roundNum, selectedCount, pluralize(selectedCount, "finding", "findings")))
			payload := buildUserFixPayload(outcome.Findings, response)
			selection := marshalFindingIDs(payload.findingIDs)
			var selectedIDs *string
			if selection != "" {
				selectedIDs = &selection
			}
			var userFindings *string
			if payload.hasOverrides {
				userFindings = &payload.findingsJSON
			}
			if err := e.db.ApplyApprovalFix(db.ApplyApprovalFixInput{
				ActionID: response.actionID, ParkedMS: time.Since(parkStart).Milliseconds(),
				SelectedIDsJSON: selectedIDs, UserFindingsJSON: userFindings,
			}); err != nil {
				return false, fmt.Errorf("apply user fix for step %s: %w", stepName, err)
			}
			// Routed consent: the human (or unattended consent) authorized a fix,
			// so repair the explicitly selected findings through the
			// intent-sensitive cascade. This is the only path that may fix an
			// ask-user finding.
			if stepName == types.StepReview && e.routingActive() {
				remainingFindings, err := e.repairConsentedReviewAtGate(ctx, sctx, run, sr, outcome.Findings, payload.findingsJSON, payload.findingIDs, currentRoundID, &roundNum)
				if err != nil {
					return false, err
				}
				outcome.Findings = remainingFindings
				e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFixing), "", "", "", nil)
				if remainingFindings == "" {
					sctx.Fixing = false
					postRepairRereview = true
					nextTrigger = "auto_fix"
					executionMS += time.Since(phaseStart).Milliseconds()
					phaseStart = time.Now()
					continue
				}
				sctx.Fixing = true
				executionMS += time.Since(phaseStart).Milliseconds()
				goto approval
			}
			if stepName != types.StepReview {
				reserveRepairRound := func(trigger string) (*db.StepRound, error) {
					roundNum++
					return e.db.ReserveStepRound(sr.ID, roundNum, trigger)
				}
				result, err := e.repairConsentedStepFindings(
					ctx,
					sctx,
					run,
					sr,
					stepName,
					response.actionID,
					payload.findingsJSON,
					payload.findingIDs,
					outcome.RepairChecks,
					reserveRepairRound,
				)
				if err != nil {
					return false, fmt.Errorf("repair user-consented %s findings: %w", stepName, err)
				}
				if !result.Owned || !result.Resolved {
					return false, fmt.Errorf("user-consented %s repair did not independently resolve every selected blocking finding", stepName)
				}
				updated, err := mergeRepairFindingsJSON(outcome.Findings, result.NewFindings)
				if err != nil {
					return false, fmt.Errorf("merge consented %s verifier findings: %w", stepName, err)
				}
				updated, err = removeFindingsByID(updated, result.ResolvedIDs)
				if err != nil {
					return false, fmt.Errorf("remove resolved consented %s findings: %w", stepName, err)
				}
				outcome.Findings = updated
				if updated == "" {
					if err := e.db.ClearStepFindings(sr.ID); err != nil {
						return false, fmt.Errorf("clear resolved consented %s findings: %w", stepName, err)
					}
				} else if err := e.db.SetStepFindings(sr.ID, updated); err != nil {
					return false, fmt.Errorf("persist remaining consented %s findings: %w", stepName, err)
				}
				if hasBlockingFindingsJSON(updated) || hasAskUserFindingsJSON(updated) {
					sctx.Fixing = true
					goto approval
				}
				sctx.Fixing = false
				executionMS += time.Since(phaseStart).Milliseconds()
				phaseStart = time.Now()
				if stepName == types.StepVerify {
					if err := e.sealCandidate(ctx, run, workDir); err != nil {
						return false, fmt.Errorf("reseal before aggregate Verify after consented repair: %w", err)
					}
					postRepairRereview = true
					nextTrigger = "auto_fix"
					continue
				}
				goto done
			}
			sctx.Fixing = true
			sctx.PreviousFindings = payload.findingsJSON
			nextTrigger = "auto_fix"
			e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFixing), "", "", "", nil)
			slog.Info("step fix requested, re-executing", "step", stepName)
			continue // loop back to step.Execute
		}
	}

done:
	// Mark step completed with execution-only timing.
	durationMS := executionMS + time.Since(phaseStart).Milliseconds()
	if approvalTerminal {
		durationMS = approvalTerminalDuration
	}
	if durationOverrideMS > 0 {
		durationMS = durationOverrideMS
	}
	status := types.StepStatusCompleted
	if stepSkipped {
		status = types.StepStatusSkipped
	}
	if !approvalTerminal {
		if err := e.db.CompleteStepWithStatus(sr.ID, status, finalExitCode, durationMS, logPath); err != nil {
			return false, fmt.Errorf("complete step %s: %w", stepName, err)
		}
	}
	e.emitStepEventWithFindingsDiffAndError(ipc.EventStepCompleted, run, repo, stepName, string(status), "", "", "", &durationMS)
	return skipRemaining, nil
}

func roundInsertID(_ string, inserted *db.StepRound, err error) string {
	if err != nil || inserted == nil {
		return ""
	}
	return inserted.ID
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

// waitForApproval blocks until a user action arrives or context is cancelled.
// The caller must set e.waiting and e.waitingStep before calling this method.
func (e *Executor) waitForApproval(ctx context.Context, stepName types.StepName) (approvalResponse, error) {
	defer func() {
		e.mu.Lock()
		e.waiting = false
		e.waitingStep = ""
		e.waitingGate = nil
		e.mu.Unlock()
		// Drain any stale response that arrived after context cancellation
		select {
		case <-e.approvalCh:
		default:
		}
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
	runStatus := types.RunFailed
	if errMsg == types.RunCancelReasonAbortedByUser || errMsg == types.RunCancelReasonSuperseded {
		runStatus = types.RunCancelled
	}
	if dbErr := e.db.UpdateRunErrorStatus(run.ID, errMsg, runStatus); dbErr != nil {
		slog.Error("failed to update run error status", "run", run.ID, "error", dbErr)
	}
	run.Status = runStatus
	run.Error = &errMsg
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return err
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
	e.onEvent(event)
}

func (e *Executor) emitStepEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string) {
	e.emitStepEventWithFindings(eventType, run, repo, stepName, status, "")
}

func (e *Executor) emitStepEventWithFindings(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string) {
	e.emitStepEventWithFindingsAndDiff(eventType, run, repo, stepName, status, findings, "")
}

func (e *Executor) emitStepEventWithFindingsAndDiff(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string, diff string) {
	e.emitStepEventWithFindingsDiffAndError(eventType, run, repo, stepName, status, findings, diff, "", nil)
}

func (e *Executor) emitStepEventWithFindingsDiffAndError(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string, diff string, errMsg string, durationMS *int64) {
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
	if diff != "" {
		event.Diff = &diff
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
	// Model selection is the routing contract; there is no single agent to
	// name in run telemetry.
	return ""
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

// recordReviewLineages gives every returned initial-review finding a durable,
// run-wide root lineage tied to the routed Candidate attempt that surfaced it.
func (e *Executor) recordReviewLineages(run *db.Run, stepName types.StepName, roundID, findingsJSON string) error {
	if stepName != types.StepReview || findingsJSON == "" {
		return nil
	}
	if run == nil || roundID == "" {
		return fmt.Errorf("routed initial review findings require a run and round")
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return fmt.Errorf("parse initial review findings for lineage: %w", err)
	}
	if len(findings.Items) == 0 {
		return nil
	}
	attempts, err := e.db.GetInvocationAttemptsByRound(roundID)
	if err != nil {
		return fmt.Errorf("load initial review attempts for lineage: %w", err)
	}
	var reviewAttempt *db.InvocationAttempt
	sawRoutedAttempt := false
	for _, attempt := range attempts {
		if attempt.Start.Purpose != types.PurposeInitialReview || attempt.Start.Candidate.IsZero() {
			continue
		}
		sawRoutedAttempt = true
		if attempt.Terminal != nil && attempt.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			reviewAttempt = attempt
			break
		}
	}
	if reviewAttempt == nil {
		if !sawRoutedAttempt {
			return nil
		}
		return fmt.Errorf("routed initial review findings have no succeeded producing Candidate attempt")
	}
	displayIDs := make([]string, 0, len(findings.Items))
	for _, finding := range findings.Items {
		displayIDs = append(displayIDs, finding.ID)
	}
	lineages, err := e.db.CreateFindingLineages(run.ID, reviewAttempt.ID, displayIDs)
	if err != nil {
		return fmt.Errorf("create initial review lineages: %w", err)
	}
	if len(lineages) != len(displayIDs) {
		return fmt.Errorf("created %d initial review lineages for %d findings", len(lineages), len(displayIDs))
	}
	return nil
}
