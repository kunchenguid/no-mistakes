package tui

import (
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// eventMsg wraps an IPC event received from the daemon.
type eventMsg struct {
	event          ipc.Event
	subscriptionID uint64
}

// errMsg wraps an error from async operations.
type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

type subscriptionErrMsg struct {
	err            error
	subscriptionID uint64
}

// rerunStartedMsg switches the TUI onto a newly created rerun.
type rerunStartedMsg struct {
	run       *ipc.RunInfo
	requestID uint64
}

type rerunErrMsg struct {
	err       error
	requestID uint64
}

type spinnerTickMsg struct{}

type syncRefreshedMsg struct{ state branchsync.State }
type syncAppliedMsg struct{ state branchsync.State }

// runReconciledMsg carries an authoritative get_run snapshot requested after a
// stream gap or a resubscribe.
type runReconciledMsg struct {
	run            *ipc.RunInfo
	err            error
	subscriptionID uint64
}

// stepDiffMsg carries a fix-review gate's working-tree diff, read on demand
// because it is derived state that the event stream deliberately does not
// carry.
type stepDiffMsg struct {
	step           types.StepName
	diff           string
	err            error
	subscriptionID uint64
}

// resubscribeMsg fires after the reconnect delay for a dropped stream.
type resubscribeMsg struct{ subscriptionID uint64 }

// connectedMsg signals that the event subscription is ready.
type connectedMsg struct {
	events         <-chan ipc.Event
	cancelSub      func()
	subscriptionID uint64
}
