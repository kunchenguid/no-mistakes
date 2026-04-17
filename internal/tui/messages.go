package tui

import "github.com/kunchenguid/no-mistakes/internal/ipc"

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

// connectedMsg signals that the event subscription is ready.
type connectedMsg struct {
	events         <-chan ipc.Event
	cancelSub      func()
	subscriptionID uint64
}
