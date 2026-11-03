package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Restart when idle (issue #1691).
//
// `no-mistakes update` replaces the executable file and then has to restart
// the daemon, which until now cancelled every run executing at that moment.
// A scheduled restart instead lets the running daemon - still executing the
// image it was started from - finish those runs, holds new pushes instead of
// starting them, and once nothing is executing stops cleanly and replaces its
// own process image with the executable file, now the new version. Runs parked
// at an approval gate do not hold the restart up: a clean stop already keeps
// them (#1438), so they resume on the new version. Held pushes are recorded in
// HeldPushesFile and started by the restarted daemon.

// ErrRestartForUpdate is returned by the daemon's serve loop when it stopped
// because a scheduled restart became due. RunRestartable answers it by
// executing the daemon's executable file again in place.
var ErrRestartForUpdate = errors.New("daemon stopped to restart through its executable for an update")

// errRestartPending refuses run launches other than a plain push while a
// restart is scheduled: they are not held, so the caller retries once the
// restarted daemon serves them.
var errRestartPending = errors.New("the daemon is restarting for an update; try again shortly")

const defaultRestartIdlePoll = 500 * time.Millisecond

type restartState struct {
	// pending is set once a restart is scheduled and never cleared: the
	// process that scheduled it exits into the new image.
	pending atomic.Bool
	// due is set when the daemon has decided to stop for the restart.
	due atomic.Bool
	// gate orders gate responses against the due mark: a response holds the
	// read side from its due check through its dispatch, and the poller holds
	// the write side while it marks the restart due and recounts.
	gate    sync.RWMutex
	once    sync.Once
	trigger func() // stops the daemon; set by the serve loop
	poll    time.Duration
	heldMu  sync.Mutex
}

// ScheduleRestartWhenIdle schedules a restart through the executable file for
// the first moment no run is executing. Calling it again changes nothing.
func (m *RunManager) ScheduleRestartWhenIdle() (*ipc.RestartStatusResult, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("restarting when idle is not supported on Windows; use `no-mistakes daemon restart` once no run is active")
	}
	if m.shuttingDown.Load() {
		return nil, errors.New("daemon is shutting down")
	}
	if m.restart.trigger == nil {
		return nil, errors.New("this daemon cannot restart itself")
	}
	m.restart.pending.Store(true)
	m.restart.once.Do(func() {
		slog.Info("restart scheduled: waiting for executing runs to finish", "waiting_runs", m.busyRunCount())
		go m.restartWhenIdle()
	})
	return m.RestartStatus(), nil
}

// RestartStatus reports whether a restart is scheduled and what it waits for.
func (m *RunManager) RestartStatus() *ipc.RestartStatusResult {
	status := &ipc.RestartStatusResult{Pending: m.restart.pending.Load()}
	if status.Pending {
		status.WaitingRuns = m.busyRunCount()
	}
	status.HeldPushes = countHeldPushes(m.paths.HeldPushesFile())
	return status
}

func (m *RunManager) restartPending() bool { return m.restart.pending.Load() }

// restartWhenIdle polls until no run is executing, then stops the daemon for
// the restart. Polling covers both ways a run stops executing - finishing and
// parking at a gate - without hooking each of them.
func (m *RunManager) restartWhenIdle() {
	poll := m.restart.poll
	if poll <= 0 {
		poll = defaultRestartIdlePoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if m.shuttingDown.Load() {
			return
		}
		if m.busyRunCount() == 0 && m.markRestartDue() {
			slog.Info("restarting for update: no run is executing")
			m.restart.trigger()
			return
		}
		<-ticker.C
	}
}

// markRestartDue marks the restart due and counts again, with the write side
// of the gate held: a gate response already dispatched is finished before the
// recount sees it, and one arriving after the mark is refused by
// beginGateResponse instead of resuming a run the stop would cancel. It
// reports whether the daemon is still idle; if not, the mark is rolled back.
func (m *RunManager) markRestartDue() bool {
	m.restart.gate.Lock()
	defer m.restart.gate.Unlock()
	m.restart.due.Store(true)
	if m.busyRunCount() == 0 {
		return true
	}
	m.restart.due.Store(false)
	return false
}

// beginGateResponse refuses a gate response in the moment between the daemon
// deciding to restart and its stop: the run stays parked and resumes on the
// restarted daemon, where the response can be sent again. A response that is
// let through holds off the due mark until the returned release is called, so
// the poller's recount cannot run between the check and the dispatch.
func (m *RunManager) beginGateResponse() (release func(), err error) {
	m.restart.gate.RLock()
	if m.restart.due.Load() {
		m.restart.gate.RUnlock()
		return nil, errRestartPending
	}
	return m.restart.gate.RUnlock, nil
}

// busyRunCount counts runs a restart must wait for: every executing run that
// is not parked at an approval gate, and every queued submission except
// recovered parked runs waiting for a slot.
func (m *RunManager) busyRunCount() int {
	m.mu.Lock()
	ids := make([]string, 0, len(m.executors))
	for id := range m.executors {
		ids = append(ids, id)
	}
	busy := 0
	for _, qe := range m.runQueue {
		if qe.recovered == nil {
			busy++
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		run, err := m.db.GetRun(id)
		if err == nil && run != nil && run.Status == types.RunRunning && run.AwaitingAgentSince != nil {
			continue
		}
		busy++
	}
	return busy
}

// holdPush records a push received while a restart is scheduled, for the
// restarted daemon to start on the new version.
func (m *RunManager) holdPush(params *ipc.PushReceivedParams) error {
	line, err := json.Marshal(params)
	if err != nil {
		return err
	}
	m.restart.heldMu.Lock()
	defer m.restart.heldMu.Unlock()
	f, err := os.OpenFile(m.paths.HeldPushesFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	slog.Info("push held until the daemon restarts for an update", "ref", params.Ref, "new", params.New, "gate", params.Gate)
	return nil
}

// replayHeldPushes starts the pushes an earlier daemon held while waiting to
// restart, in the order they arrived. The file is removed only after every
// push was handed over, so a crash part-way replays the rest next time; a
// push replayed twice is superseded like any repeated push to a branch.
func (m *RunManager) replayHeldPushes(ctx context.Context) {
	path := m.paths.HeldPushesFile()
	replayPath := path + ".replay"
	m.restart.heldMu.Lock()
	// A replay file left by a crash is replayed first; pushes held meanwhile
	// are appended to it so none is dropped.
	if _, statErr := os.Stat(replayPath); errors.Is(statErr, fs.ErrNotExist) {
		if err := os.Rename(path, replayPath); err != nil {
			m.restart.heldMu.Unlock()
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Error("failed to claim held pushes; they were not started", "path", path, "error", err)
			}
			return
		}
	} else if data, err := os.ReadFile(path); err == nil {
		if f, err := os.OpenFile(replayPath, os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, werr := f.Write(data)
			_ = f.Close()
			if werr == nil {
				_ = os.Remove(path)
			}
		}
	}
	m.restart.heldMu.Unlock()
	path = replayPath
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("failed to read held pushes; they were not started", "path", path, "error", err)
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), ipc.MaxFrameBytes)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var params ipc.PushReceivedParams
		if err := json.Unmarshal(line, &params); err != nil {
			slog.Error("skipping unreadable held push", "error", err)
			continue
		}
		runID, err := m.HandlePushReceived(ctx, &params)
		if err != nil {
			slog.Error("held push could not be started after restart", "ref", params.Ref, "new", params.New, "error", err)
			continue
		}
		slog.Info("started held push after restart", "ref", params.Ref, "new", params.New, "run_id", runID)
	}
	if err := scanner.Err(); err != nil {
		slog.Error("failed to read held pushes; the rest were not started", "path", path, "error", err)
		return
	}
	if err := os.Remove(path); err != nil {
		slog.Error("failed to remove replayed held pushes", "path", path, "error", fmt.Errorf("remove: %w", err))
	}
}

func countHeldPushes(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			count++
		}
	}
	return count
}
