package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

const (
	// driveHeartbeatInterval is deliberately slow. Normal wakeups come from
	// run events; this full-state read exists only to recover a lost event.
	driveHeartbeatInterval = 30 * time.Second
	// Reconnects are bounded so a dead daemon becomes an actionable AXI error
	// rather than an infinite silent wait.
	driveReconnectInterval = 500 * time.Millisecond
	driveReconnectTimeout  = 30 * time.Second
)

type runStateSource interface {
	Subscribe(runID string) (<-chan ipc.Event, func(), error)
	Reconcile(ctx context.Context, runID string) (*ipc.RunInfo, error)
}

type ipcRunStateSource struct {
	socketPath string
}

func (s *ipcRunStateSource) Subscribe(runID string) (<-chan ipc.Event, func(), error) {
	return ipc.Subscribe(s.socketPath, &ipc.SubscribeParams{RunID: runID})
}

func (s *ipcRunStateSource) Reconcile(ctx context.Context, runID string) (*ipc.RunInfo, error) {
	client, err := ipc.Dial(s.socketPath)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	timeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ctx.Err()
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	var result ipc.GetRunResult
	if err := client.CallWithTimeout(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result, timeout); err != nil {
		return nil, err
	}
	return result.Run, nil
}

// runReconciler is the sole owner of event-driven run-state refresh policy.
// It always subscribes before its first full read, refreshes only for
// state-bearing events, reconnects a dropped stream before reconciling, and
// uses one slow heartbeat as a lost-event backstop. Event payloads are wakeup
// hints only: authoritative state always comes from get_run, which makes
// duplicate and delayed events harmless.
type runReconciler struct {
	runID             string
	source            runStateSource
	heartbeatInterval time.Duration
	reconnectInterval time.Duration
	reconnectTimeout  time.Duration

	events         <-chan ipc.Event
	cancelSub      func()
	started        bool
	lastRun        *ipc.RunInfo
	lastReconciled time.Time
}

func newRunReconciler(source runStateSource, runID string) *runReconciler {
	return &runReconciler{
		runID:             runID,
		source:            source,
		heartbeatInterval: driveHeartbeatInterval,
		reconnectInterval: driveReconnectInterval,
		reconnectTimeout:  driveReconnectTimeout,
	}
}

// Next blocks until a state reconciliation is warranted and returns the
// authoritative run snapshot.
func (r *runReconciler) Next(ctx context.Context) (*ipc.RunInfo, error) {
	if !r.started {
		if err := r.connect(ctx); err != nil {
			return nil, err
		}
		r.started = true
		return r.reconcile(ctx)
	}

	heartbeatAfter := r.heartbeatInterval
	if !r.lastReconciled.IsZero() {
		heartbeatAfter -= time.Since(r.lastReconciled)
		if heartbeatAfter < 0 {
			heartbeatAfter = 0
		}
	}
	heartbeat := time.NewTimer(heartbeatAfter)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-heartbeat.C:
			return r.reconcile(ctx)
		case event, ok := <-r.events:
			if !ok {
				r.clearSubscription()
				if err := r.connect(ctx); err != nil {
					return nil, err
				}
				return r.reconcile(ctx)
			}
			if event.RunID != "" && event.RunID != r.runID {
				continue
			}
			if event.Type == ipc.EventLogChunk && r.lastRun != nil {
				// Log events can make CI ready without changing database state.
				// Wake the driver to inspect the log while preserving the full-read
				// budget and the heartbeat deadline.
				return r.lastRun, nil
			}
			if !stateReconcileEvent(event, r.runID) {
				continue
			}
			// Coalesce a burst of duplicate transitions into one database read.
			for {
				select {
				case queued, open := <-r.events:
					if !open {
						r.clearSubscription()
						if err := r.connect(ctx); err != nil {
							return nil, err
						}
						return r.reconcile(ctx)
					}
					_ = queued // every queued event is covered by the full read below
				default:
					return r.reconcile(ctx)
				}
			}
		}
	}
}

func (r *runReconciler) reconcile(ctx context.Context) (*ipc.RunInfo, error) {
	run, err := r.source.Reconcile(ctx, r.runID)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", r.runID, err)
	}
	r.lastRun = run
	r.lastReconciled = time.Now()
	return run, nil
}

func (r *runReconciler) connect(ctx context.Context) error {
	started := time.Now()
	var lastErr error
	for {
		events, cancel, err := r.source.Subscribe(r.runID)
		if err == nil {
			r.events = events
			r.cancelSub = cancel
			return nil
		}
		lastErr = err
		remaining := r.reconnectTimeout - time.Since(started)
		if r.reconnectTimeout <= 0 || remaining <= 0 {
			return fmt.Errorf("subscribe to run %s events after reconnect: %w", r.runID, lastErr)
		}
		wait := r.reconnectInterval
		if wait <= 0 || wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *runReconciler) Close() {
	r.clearSubscription()
}

func (r *runReconciler) clearSubscription() {
	if r.cancelSub != nil {
		r.cancelSub()
	}
	r.events = nil
	r.cancelSub = nil
}

func stateReconcileEvent(event ipc.Event, runID string) bool {
	if event.RunID != "" && event.RunID != runID {
		return false
	}
	switch event.Type {
	case ipc.EventRunCreated, ipc.EventRunUpdated, ipc.EventRunCompleted,
		ipc.EventStepStarted, ipc.EventStepCompleted:
		return true
	default:
		return false
	}
}
