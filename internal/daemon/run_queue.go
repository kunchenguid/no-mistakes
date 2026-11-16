package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

type queuedRun struct {
	sequence  uint64
	recovered *db.Run
	runID     string
	repoID    string
	branch    string
	executor  *pipeline.Executor
	cancel    context.CancelCauseFunc
	done      chan struct{}
	agent     agent.Agent
	spawn     func()
}

// slotFreeLocked reports whether a new execution may start under the given
// cap. A non-positive cap means unlimited. Callers must hold m.mu: the
// executors count and the queue change only under it, so check-and-register
// stays atomic.
func (m *RunManager) slotFreeLocked(maxConc int) bool {
	return maxConc <= 0 || len(m.executors) < maxConc
}

// executionCapLocked reloads the cap while m.mu is held. Read the file directly:
// LoadGlobal treats a missing file as defaults, which would clear a loaded cap
// during config replacement. The initial zero still means unlimited.
func (m *RunManager) executionCapLocked() int {
	data, err := os.ReadFile(m.paths.ConfigFile())
	if err == nil {
		var global *config.GlobalConfig
		global, err = config.LoadGlobalFromBytes(data)
		if err == nil {
			m.maxConcurrentRuns = global.MaxConcurrentRuns
		}
	}
	if err != nil {
		slog.Warn("failed to load global config for execution cap; retaining previous limit", "error", err)
	}
	return m.maxConcurrentRuns
}

func (m *RunManager) reserveAdmission() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executionCapLocked()
	m.nextAdmission++
	sequence := m.nextAdmission
	m.runQueue = append(m.runQueue, queuedRun{sequence: sequence})
	return sequence
}

func (m *RunManager) releaseAdmission(sequence uint64) {
	m.mu.Lock()
	for i, qe := range m.runQueue {
		if qe.sequence == sequence && qe.spawn == nil {
			m.runQueue = slices.Delete(m.runQueue, i, i+1)
			m.mu.Unlock()
			m.pumpQueue()
			return
		}
	}
	m.mu.Unlock()
}

func (m *RunManager) admitRun(sequence uint64, qe queuedRun) {
	m.mu.Lock()
	if m.shuttingDown.Load() {
		m.mu.Unlock()
		m.shutdownQueuedRun(qe)
		return
	}
	for i := range m.runQueue {
		if m.runQueue[i].sequence == sequence {
			qe.sequence = sequence
			m.runQueue[i] = qe
			break
		}
	}
	m.mu.Unlock()
	m.pumpQueue()
}

func (m *RunManager) pumpQueue() {
	m.mu.Lock()
	defer m.mu.Unlock()
	maxConc := m.executionCapLocked()
	for !m.shuttingDown.Load() && len(m.runQueue) > 0 && m.slotFreeLocked(maxConc) {
		i := 0
		if maxConc <= 0 {
			for i < len(m.runQueue) && m.runQueue[i].spawn == nil {
				i++
			}
		}
		if i == len(m.runQueue) || m.runQueue[i].spawn == nil {
			return
		}
		next := m.runQueue[i]
		m.runQueue = slices.Delete(m.runQueue, i, i+1)
		m.executors[next.runID] = next.executor
		m.cancels[next.runID] = next.cancel
		m.dones[next.runID] = next.done
		m.ciWakes[next.runID] = make(chan struct{}, 1)
		slog.Info("starting admitted run", "run_id", next.runID, "branch", next.branch)
		next.spawn()
	}
}

func (m *RunManager) shutdownQueuedRun(qe queuedRun) {
	if qe.recovered != nil {
		qe.cancel(pipeline.ErrDaemonShutdown)
		_ = qe.agent.Close()
		return
	}
	if err := m.cancelQueuedRun(qe, "daemon shutting down"); err != nil {
		slog.Error("failed to cancel queued run on shutdown", "run_id", qe.runID, "error", err)
	}
}

// extractQueuedLocked removes and returns the queued run with the given id,
// preserving the order of the rest. Callers must hold m.mu.
func (m *RunManager) extractQueuedLocked(runID string) (queuedRun, bool) {
	for i, qe := range m.runQueue {
		if qe.spawn != nil && qe.runID == runID {
			copy(m.runQueue[i:], m.runQueue[i+1:])
			m.runQueue[len(m.runQueue)-1] = queuedRun{}
			m.runQueue = m.runQueue[:len(m.runQueue)-1]
			return qe, true
		}
	}
	return queuedRun{}, false
}

// extractQueuedForBranchLocked removes and returns every queued run for the
// given repo and branch, in queue order. Callers must hold m.mu.
func (m *RunManager) extractQueuedForBranchLocked(repoID, branch string) []queuedRun {
	var out []queuedRun
	kept := m.runQueue[:0]
	for _, qe := range m.runQueue {
		if qe.spawn != nil && qe.repoID == repoID && qe.branch == branch {
			out = append(out, qe)
		} else {
			kept = append(kept, qe)
		}
	}
	for i := len(kept); i < len(m.runQueue); i++ {
		m.runQueue[i] = queuedRun{}
	}
	m.runQueue = kept
	return out
}

// cancelQueuedRun records a queued run terminal without starting or resuming
// its executor and releases its setup resources. A recovered run's head must
// be preserved before its worktree is removed. It frees no execution slot in
// this daemon lifetime, so no pump follows.
func (m *RunManager) cancelQueuedRun(qe queuedRun, cause string) error {
	run, err := m.db.GetRun(qe.runID)
	if err == nil {
		err = m.cancelInactiveRun(run, cause)
	}
	qe.cancel(nil)
	qe.agent.Close()
	return err
}

func (m *RunManager) cancelInactiveRun(run *db.Run, cause string) error {
	if run == nil {
		return fmt.Errorf("no active run to cancel")
	}
	if run.Status != types.RunPending && run.Status != types.RunRunning {
		return nil
	}
	workDir := worktrees.RecordedDir(m.paths, run.WorktreePath(), run.RepoID, run.ID)
	gateDir := m.paths.RepoDir(run.RepoID)
	var head string
	var verified bool
	if run.Status == types.RunRunning {
		head, verified = preserveRunHead(m.db, workDir, run)
	}
	var err error
	if verified {
		err = m.db.UpdateRunErrorStatusWithVerifiedHead(run.ID, cause, types.RunCancelled, head)
	} else {
		err = m.db.UpdateRunErrorStatus(run.ID, cause, types.RunCancelled)
	}
	if err != nil {
		return fmt.Errorf("mark run cancelled: %w", err)
	}
	if run.AwaitingAgentSince != nil {
		parkStart := time.Unix(*run.AwaitingAgentSince, 0)
		if err := m.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkStart).Milliseconds()); err != nil {
			return fmt.Errorf("complete cancelled run awaiting-agent state: %w", err)
		}
	}
	status := string(types.RunCancelled)
	m.broadcast(ipc.Event{
		Type: ipc.EventRunCompleted, RunID: run.ID, RepoID: run.RepoID,
		Branch: &run.Branch, Status: &status, Error: &cause, PRURL: run.PRURL,
	})
	if run.Status == types.RunPending || verified {
		m.removeRunWorktree(run.RepoID, run.ID, gateDir, workDir, "inactive_run_cancelled")
	}
	m.closeSubscribers(run.ID)
	slog.Info("cancelled inactive run", "run_id", run.ID, "branch", run.Branch)
	return nil
}
