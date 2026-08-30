package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// publicationControl is the daemon's complete authority over publication
// policy and effects. Runtime owns only goroutine lifecycle and delegates all
// protocol/state decisions to this service.
type publicationControl interface {
	Start(context.Context, publication.ParsedRequest) (publication.Result, error)
	Authorize(context.Context, publication.Authorization) (publication.Result, error)
	Status(context.Context, string) (publication.Result, error)
	RecoverEffect(context.Context, string, publication.EffectKind) (publication.Result, error)
}

// publicationExecutorPlan is the already-composed use of the existing
// pipeline Executor for one publication. WorkDir is contextual only; protected
// defense steps replace it with fresh CandidatePort views in their adapter.
type publicationExecutorPlan struct {
	Executor *pipeline.Executor
	WorkDir  string
	Cleanup  func()
}

type publicationExecutorFactory func(context.Context, string, *db.Run, *db.Repo) (*publicationExecutorPlan, error)

type publicationRuntimeOptions struct {
	DB              *db.DB
	Runs            *RunManager
	Manager         publicationControl
	Identity        publication.PublisherBinding
	ExecutorFactory publicationExecutorFactory
}

// publicationRuntime is the daemon-owned adapter between strict publication
// RPC and the one existing pipeline Executor. It contains no step traversal,
// retry policy, counter, or state machine of its own.
type publicationRuntime struct {
	db       *db.DB
	runs     *RunManager
	manager  publicationControl
	identity publication.PublisherBinding
	factory  publicationExecutorFactory
}

func newPublicationRuntime(options publicationRuntimeOptions) (*publicationRuntime, error) {
	if options.DB == nil {
		return nil, fmt.Errorf("publication runtime database is required")
	}
	if options.Runs == nil {
		return nil, fmt.Errorf("publication runtime RunManager is required")
	}
	if options.Manager == nil {
		return nil, fmt.Errorf("publication runtime manager is required")
	}
	if !validPublicationRPCIdentity(publicationIPCIdentity(options.Identity)) {
		return nil, fmt.Errorf("publication runtime daemon identity is invalid")
	}
	if options.ExecutorFactory == nil {
		return nil, fmt.Errorf("publication runtime executor factory is required")
	}
	runtime := &publicationRuntime{
		db: options.DB, runs: options.Runs, manager: options.Manager, identity: options.Identity, factory: options.ExecutorFactory,
	}
	// Startup recovery receives only the narrow recovery interface. Its only
	// mutating-effect operation is Manager.RecoverEffect, which observes first;
	// the runtime itself exposes no alternate Push/PR execution route.
	options.Runs.publicationRecovery = runtime
	return runtime, nil
}

func (r *publicationRuntime) Start(ctx context.Context, request publication.ParsedRequest) (publication.Result, error) {
	result, err := r.manager.Start(ctx, request)
	if err != nil {
		return publication.Result{}, err
	}
	if publicationResultRunnable(result) {
		if _, err := r.ensureExecutor(ctx, result); err != nil {
			return publication.Result{}, err
		}
	}
	return result, nil
}

// Authorize persists exactly one Manager decision. It deliberately does not
// start, resume, or signal an Executor: the already parked adapter observes the
// durable decision through Manager.WaitForAuthorization.
func (r *publicationRuntime) Authorize(ctx context.Context, authorization publication.Authorization) (publication.Result, error) {
	return r.manager.Authorize(ctx, authorization)
}

// Status is a pure projection. In particular it never repairs a missing
// goroutine or advances a step as a side effect of observation.
func (r *publicationRuntime) Status(ctx context.Context, publicationID string) (publication.Result, error) {
	return r.manager.Status(ctx, publicationID)
}

// ResumePublication puts the same Executor back at the first permissible
// durable boundary. Executor.ResumePublication owns validation and invokes its
// existing remainder traversal; this method only makes the goroutine unique.
func (r *publicationRuntime) ResumePublication(ctx context.Context, publicationID string) (publication.Result, error) {
	result, err := r.manager.Status(ctx, publicationID)
	if err != nil {
		return publication.Result{}, err
	}
	// Recovery is authorized by the durable active Run, not by the public
	// projection alone. In particular, READY/FAILED may describe a terminal CI
	// journal whose still-running CI step must be completed/failed by the one
	// Executor without observing CI again. ensureExecutor itself refuses a
	// terminal Run and validates the exact publisher/publication binding.
	if _, err := r.ensureExecutor(ctx, result); err != nil {
		return publication.Result{}, err
	}
	return result, nil
}

// RecoverEffect first performs Manager's read-only exact reconciliation. Only
// a conclusive continuation state is then handed back to the Executor. Unknown,
// failed, denied, and drift outcomes remain parked fail-closed and are never
// translated into a replay.
func (r *publicationRuntime) RecoverEffect(ctx context.Context, publicationID string, kind publication.EffectKind) (publication.Result, error) {
	result, err := r.manager.RecoverEffect(ctx, publicationID, kind)
	if err != nil {
		return publication.Result{}, err
	}
	if publicationResultRunnable(result) {
		if _, err := r.ensureExecutor(ctx, result); err != nil {
			return publication.Result{}, err
		}
	}
	return result, nil
}

func publicationResultRunnable(result publication.Result) bool {
	switch result.Status {
	case publication.StatusChecking, publication.StatusReadyForPush, publication.StatusReadyForPR, publication.StatusCIObserving:
		return true
	default:
		return false
	}
}

func (r *publicationRuntime) ensureExecutor(ctx context.Context, result publication.Result) (bool, error) {
	publicationRow, err := r.db.GetPublication(result.PublicationID)
	if err != nil {
		return false, fmt.Errorf("load publication binding: %w", err)
	}
	if publicationRow == nil || publicationRow.RunID != result.RunID || publicationRow.HeadSHA != result.HeadSHA {
		return false, fmt.Errorf("publication result does not match its durable binding")
	}
	parsed, err := publication.ParseRequest(publicationRow.CanonicalRequest)
	if err != nil {
		return false, fmt.Errorf("parse durable publication identity binding: %w", err)
	}
	if parsed.Request.Publisher != r.identity {
		return false, fmt.Errorf("durable publication publisher does not match this daemon binary")
	}
	run, err := r.db.GetRun(result.RunID)
	if err != nil {
		return false, fmt.Errorf("load publication run: %w", err)
	}
	if run == nil || run.Kind != types.RunKindFactoryPublicationV1 || run.RepoID != publicationRow.RepoID || run.HeadSHA != publicationRow.HeadSHA {
		return false, fmt.Errorf("publication result does not match an active factory-publication-v1 Run")
	}
	if run.Status.Terminal() {
		return false, nil
	}
	repo, err := r.db.GetRepo(run.RepoID)
	if err != nil {
		return false, fmt.Errorf("load publication repository: %w", err)
	}
	if repo == nil {
		return false, fmt.Errorf("publication repository %s is not registered", run.RepoID)
	}
	return r.runs.launchPublicationExecutor(ctx, publicationRow.PublicationID, run, repo, r.factory)
}

// registerHandlers binds the public machine surface to this exact runtime and
// exact daemon binary identity. The generic AXI handler set remains separate.
func (r *publicationRuntime) registerHandlers(server *ipc.Server, identity ipc.PublicationIdentity, guard publicationMutationGuard) {
	registerPublicationHandlers(server, r, identity, guard)
}

func publicationIPCIdentity(binding publication.PublisherBinding) ipc.PublicationIdentity {
	return ipc.PublicationIdentity{
		ExecutablePath: binding.ExecutablePath, ExecutableSHA256: binding.ExecutableSHA256,
		BuildSHA: binding.BuildSHA, Protocol: binding.Protocol,
	}
}

// launchPublicationExecutor is RunManager's one publication goroutine gate.
// The common cancel/done/waitgroup maps make Shutdown wait for it exactly like
// an ordinary run, while the typed executor map preserves existing response
// routing without adding a second scheduler.
func (m *RunManager) launchPublicationExecutor(ctx context.Context, publicationID string, run *db.Run, repo *db.Repo, factory publicationExecutorFactory) (bool, error) {
	if m.shuttingDown.Load() {
		return false, fmt.Errorf("daemon is shutting down")
	}
	if run == nil || repo == nil || factory == nil {
		return false, fmt.Errorf("publication executor launch is incomplete")
	}

	m.mu.Lock()
	if _, active := m.executors[run.ID]; active {
		m.mu.Unlock()
		return false, nil
	}
	// Keep creation inside the same critical section as registration. The
	// factory performs local composition only; this prevents concurrent
	// idempotent Start calls from constructing competing executors/adapters.
	plan, err := factory(ctx, publicationID, run, repo)
	if err != nil {
		terminalErr := m.db.UpdateRunErrorStatus(run.ID, err.Error(), types.RunFailed)
		m.mu.Unlock()
		if terminalErr != nil {
			return false, errors.Join(fmt.Errorf("compose publication executor: %w", err), fmt.Errorf("terminalize publication after composition failure: %w", terminalErr))
		}
		return false, fmt.Errorf("compose publication executor: %w", err)
	}
	if plan == nil || plan.Executor == nil {
		terminalErr := m.db.UpdateRunErrorStatus(run.ID, "publication executor factory returned no executor", types.RunFailed)
		m.mu.Unlock()
		if terminalErr != nil {
			return false, errors.Join(errors.New("publication executor factory returned no executor"), fmt.Errorf("terminalize publication after composition failure: %w", terminalErr))
		}
		return false, fmt.Errorf("publication executor factory returned no executor")
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	m.executors[run.ID] = plan.Executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				message := fmt.Sprintf("internal publication panic: %v", recovered)
				_ = m.db.UpdateRunErrorStatus(run.ID, message, types.RunFailed)
				slog.Error("panic in publication executor", "run_id", run.ID, "panic", recovered)
			}
			cancel(nil)
			if plan.Cleanup != nil {
				plan.Cleanup()
			}
			m.closeSubscribers(run.ID)
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()
		}()

		if err := plan.Executor.ResumePublication(runCtx, run, repo, plan.WorkDir); err != nil {
			// ResumePublication routes every error after boundary validation
			// through Executor.failRun. An early composition/shape error is still
			// made terminal here so an idempotent Start cannot spin forever.
			fresh, loadErr := m.db.GetRun(run.ID)
			if loadErr == nil && fresh != nil && !fresh.Status.Terminal() {
				_ = m.db.UpdateRunErrorStatus(run.ID, err.Error(), types.RunFailed)
			}
			slog.Error("publication executor failed", "publication_id", publicationID, "run_id", run.ID, "error", err)
		}
	}()
	return true, nil
}

func (m *RunManager) publicationExecutorActive(runID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, active := m.executors[runID]
	return active
}

var _ publicationRPCService = (*publicationRuntime)(nil)
var _ publicationRecoveryService = (*publicationRuntime)(nil)
