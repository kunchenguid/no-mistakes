package cli

import (
	"context"
	"fmt"
	"sync/atomic"
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
	defaultGetRunTimeout   = 30 * time.Second
)

// driveGetRunTimeoutNS is the per-attempt get_run read deadline in
// nanoseconds. A live daemon that takes longer than this to answer is slow,
// not dead: Reconcile classifies that timeout, probes health, and retries.
// Tests shorten it so a fake slow daemon can be proven without waiting 30s.
var driveGetRunTimeoutNS atomic.Int64

func init() {
	driveGetRunTimeoutNS.Store(int64(defaultGetRunTimeout))
}

func getRunCallTimeout() time.Duration {
	d := time.Duration(driveGetRunTimeoutNS.Load())
	if d <= 0 {
		return defaultGetRunTimeout
	}
	return d
}

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
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := s.getRun(ctx, runID)
		if err == nil {
			return run, nil
		}
		if !ipc.IsCallTimeout(err) {
			return nil, err
		}
		if probeErr := s.probeHealth(ctx); probeErr != nil {
			return nil, fmt.Errorf("get_run timed out and daemon health probe failed: %w", probeErr)
		}
		// Health succeeded: the daemon is live and the missed deadline was a
		// slow reply. Retry until the caller's wait/context budget expires.
	}
}

func (s *ipcRunStateSource) getRun(ctx context.Context, runID string) (*ipc.RunInfo, error) {
	client, err := ipc.Dial(s.socketPath)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	timeout := getRunCallTimeout()
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
	if err := client.CallWithContext(ctx, ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result, timeout); err != nil {
		return nil, err
	}
	return result.Run, nil
}

func (s *ipcRunStateSource) probeHealth(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := ipc.Dial(s.socketPath)
	if err != nil {
		return err
	}
	defer client.Close()

	timeout := ipc.DefaultDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err()
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	var result ipc.HealthResult
	if err := client.CallWithContext(ctx, ipc.MethodHealth, &ipc.HealthParams{}, &result, timeout); err != nil {
		return err
	}
	if result.Status != "ok" {
		return fmt.Errorf("daemon health status %q", result.Status)
	}
	return nil
}

// runReconciler is the sole owner of event-driven run-state refresh policy.
// It always subscribes before its first full read, refreshes only for
// state-bearing events, reconnects a dropped stream before reconciling, and
// uses one slow heartbeat as a lost-event backstop. Event payloads are wakeup
// hints only: authoritative state always comes from get_run, which makes
// duplicate and delayed events harmless. A get_run that misses its read
// deadline is classified as a slow reply: a passing health probe retries
// instead of treating a live daemon as I/O failure.
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
	// The event taxonomy has one owner (ipc.ClassOf), so the daemon's overflow
	// policy and this reconciliation policy cannot drift apart. Anything
	// classified as a state transition, plus the daemon's own stream-gap
	// signal, forces one authoritative read; an unrecognised type fails safe
	// to state there and reconciles here rather than being ignored.
	switch ipc.ClassOf(event.Type) {
	case ipc.ClassState, ipc.ClassControl:
		return true
	default:
		return false
	}
}
