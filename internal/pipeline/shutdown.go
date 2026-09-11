package pipeline

import (
	"context"
	"errors"
)

// ErrDaemonShutdown cancels all daemon work; only a persisted approval wait
// may turn it into a recoverable suspension. Active steps still fail normally.
var ErrDaemonShutdown = errors.New("daemon shutting down")

// ErrRunSuspended tells the manager to retain a parked run's recovery assets
// while releasing its process resources. It is not a completed or failed run.
var ErrRunSuspended = errors.New("parked run suspended for daemon shutdown")

func (e *Executor) cancelApprovalWait(ctx context.Context) (approvalResponse, bool, error) {
	// Respond claims the gate before sending. As with reconciliation, an
	// accepted operator decision wins even if shutdown becomes ready too.
	if !e.claimGateResolution() {
		return <-e.approvalCh, false, nil
	}
	if errors.Is(context.Cause(ctx), ErrDaemonShutdown) {
		return approvalResponse{}, false, ErrRunSuspended
	}
	return approvalResponse{}, false, context.Cause(ctx)
}
