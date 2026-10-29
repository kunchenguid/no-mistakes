package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// startTestDaemonWithMaxConcurrentRuns starts a daemon whose global config
// caps concurrent runs, mirroring startTestDaemonWithSteps.
func startTestDaemonWithMaxConcurrentRuns(t *testing.T, maxConcurrent int, sf StepFactory) (*paths.Paths, *db.DB) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + fmt.Sprintf("\nmax_concurrent_runs: %d\n", maxConcurrent)
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	runTestDaemon(t, p, d, sf, 15*time.Second)
	return p, d
}

func submitBranchRun(t *testing.T, p *paths.Paths, gateDir, branch, headSHA string) string {
	t.Helper()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: gateDir,
		Ref:  "refs/heads/" + branch,
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}
	return result.RunID
}

// waitForRunTerminalStatus waits for any terminal status (completed, failed,
// or cancelled): unlike waitForRunTerminalState, it also settles cancelled
// runs, which is how queue tests end their runs.
func waitForRunTerminalStatus(t *testing.T, d *db.DB, runID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status.Terminal() {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach terminal state", runID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func cancelRun(t *testing.T, p *paths.Paths, runID string) {
	t.Helper()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.CancelRunResult
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: runID}, &result); err != nil {
		t.Fatalf("cancel run %s: %v", runID, err)
	}
}

func runStatus(t *testing.T, d *db.DB, runID string) types.RunStatus {
	t.Helper()
	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatalf("run %s not found", runID)
	}
	return run.Status
}

// Queued runs start in submission order as slots free.
func TestMaxConcurrentRuns_QueuedRunsStartFIFO(t *testing.T) {
	started := make(chan string, 8)
	p, d := startTestDaemonWithMaxConcurrentRuns(t, 1, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "max-conc-fifo-repo")
	gateDir := p.RepoDir("max-conc-fifo-repo")

	firstID := submitBranchRun(t, p, gateDir, "feature/one", headSHA)
	waitForStartedBranch(t, started, "feature/one")

	queuedIDs := []string{
		submitBranchRun(t, p, gateDir, "feature/two", headSHA),
		submitBranchRun(t, p, gateDir, "feature/three", headSHA),
		submitBranchRun(t, p, gateDir, "feature/four", headSHA),
	}
	for _, id := range queuedIDs {
		if got := runStatus(t, d, id); got != types.RunPending {
			t.Fatalf("queued run %s status = %s, want pending", id, got)
		}
	}

	// No queued run may start while the slot is held.
	select {
	case got := <-started:
		t.Fatalf("queued run started while the slot was held: %s", got)
	case <-time.After(500 * time.Millisecond):
	}

	for i, want := range []string{"feature/two", "feature/three", "feature/four"} {
		if i == 0 {
			cancelRun(t, p, firstID)
		} else {
			cancelRun(t, p, queuedIDs[i-1])
		}
		waitForStartedBranch(t, started, want)
	}
	// The loop already cancelled every run but the last one as it started
	// the next; only feature/four is still executing.
	cancelRun(t, p, queuedIDs[2])
	waitForRunTerminalStatus(t, d, firstID)
	for _, id := range queuedIDs {
		waitForRunTerminalStatus(t, d, id)
	}
}

// Cancelling a queued run removes it cleanly: it never starts, and the queue
// drains around it.
func TestMaxConcurrentRuns_CancelQueuedRunNeverStarts(t *testing.T) {
	started := make(chan string, 4)
	p, d := startTestDaemonWithMaxConcurrentRuns(t, 1, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "max-conc-cancel-repo")
	gateDir := p.RepoDir("max-conc-cancel-repo")

	firstID := submitBranchRun(t, p, gateDir, "feature/one", headSHA)
	waitForStartedBranch(t, started, "feature/one")

	queuedID := submitBranchRun(t, p, gateDir, "feature/two", headSHA)
	if got := runStatus(t, d, queuedID); got != types.RunPending {
		t.Fatalf("queued run status = %s, want pending", got)
	}

	cancelRun(t, p, queuedID)
	if got := waitForRunTerminalStatus(t, d, queuedID); got.Status != types.RunCancelled {
		t.Fatalf("cancelled queued run status = %s, want cancelled", got.Status)
	}

	// Freeing the slot must not start the cancelled run.
	cancelRun(t, p, firstID)
	waitForRunTerminalStatus(t, d, firstID)
	select {
	case got := <-started:
		t.Fatalf("cancelled queued run started after the slot freed: %s", got)
	case <-time.After(time.Second):
	}
}

// An explicit zero keeps today's unlimited behavior.
func TestMaxConcurrentRuns_ZeroMeansUnlimited(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithMaxConcurrentRuns(t, 0, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "max-conc-zero-repo")
	gateDir := p.RepoDir("max-conc-zero-repo")

	firstID := submitBranchRun(t, p, gateDir, "feature/one", headSHA)
	waitForStartedBranch(t, started, "feature/one")
	secondID := submitBranchRun(t, p, gateDir, "feature/two", headSHA)
	waitForStartedBranch(t, started, "feature/two")
	if got := runStatus(t, d, secondID); got != types.RunRunning {
		t.Fatalf("second run status = %s, want running", got)
	}
	cancelRun(t, p, firstID)
	cancelRun(t, p, secondID)
	waitForRunTerminalStatus(t, d, firstID)
	waitForRunTerminalStatus(t, d, secondID)
}

// A second submitted run waits in pending while the cap is held, then starts
// once the first run finishes.
func TestMaxConcurrentRuns_SecondRunWaitsForSlot(t *testing.T) {
	started := make(chan string, 4)
	p, d := startTestDaemonWithMaxConcurrentRuns(t, 1, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "max-conc-wait-repo")
	gateDir := p.RepoDir("max-conc-wait-repo")

	firstID := submitBranchRun(t, p, gateDir, "feature/one", headSHA)
	waitForStartedBranch(t, started, "feature/one")

	secondID := submitBranchRun(t, p, gateDir, "feature/two", headSHA)

	select {
	case got := <-started:
		t.Fatalf("second run started while the slot was held: %s", got)
	case <-time.After(500 * time.Millisecond):
	}
	if got := runStatus(t, d, secondID); got != types.RunPending {
		t.Fatalf("queued run status = %s, want pending", got)
	}

	cancelRun(t, p, firstID)
	waitForStartedBranch(t, started, "feature/two")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := runStatus(t, d, secondID); got == types.RunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued run status = %s, want running after the slot freed", runStatus(t, d, secondID))
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelRun(t, p, secondID)
	if got := waitForRunTerminalStatus(t, d, secondID); got.Status != types.RunCancelled {
		t.Fatalf("second run status = %s, want cancelled", got.Status)
	}
}

func expectQueuedBranch(t *testing.T, started <-chan string, want string) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("started branch = %s, want %s", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("branch %s did not start", want)
	}
}

func expectNoQueuedBranch(t *testing.T, started <-chan string) {
	t.Helper()
	select {
	case got := <-started:
		t.Fatalf("branch started before its admission slot: %s", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMaxConcurrentRuns_ConcurrentSubmissionsPreserveAdmissionOrder(t *testing.T) {
	for _, freeDuringSetup := range []bool{false, true} {
		t.Run(fmt.Sprintf("free_during_setup_%t", freeDuringSetup), func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			content, err := os.ReadFile(p.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), append(content, []byte("max_concurrent_runs: 1\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			started := make(chan string, 4)
			setupEntered := make(chan struct{})
			releaseSetup := make(chan struct{})
			var calls atomic.Int32
			var releaseOnce sync.Once
			m := NewRunManager(d, p, func() []pipeline.Step {
				if calls.Add(1) == 2 {
					close(setupEntered)
					<-releaseSetup
				}
				return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
			})
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseSetup) })
				m.Shutdown()
			})
			repo, head := setupTestGitRepo(t, p, d, "concurrent-admission")
			submit := func(branch string) (string, error) {
				return m.startRun(context.Background(), repo, branch, head, head, "push", nil, "", "", false, "", nil)
			}
			held, err := submit("feature/held")
			if err != nil {
				t.Fatal(err)
			}
			expectQueuedBranch(t, started, "feature/held")
			type submitted struct {
				id  string
				err error
			}
			firstResult := make(chan submitted, 1)
			go func() {
				id, err := submit("feature/first")
				firstResult <- submitted{id, err}
			}()
			select {
			case <-setupEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("first queued submission never reached setup barrier")
			}
			if err := m.HandleCancel(""); err == nil {
				t.Fatal("empty run ID cancelled an unfinished submission")
			}
			second, err := submit("feature/second")
			if err != nil {
				t.Fatal(err)
			}
			third, err := submit("feature/third")
			if err != nil {
				t.Fatal(err)
			}
			if freeDuringSetup {
				if err := m.HandleCancel(held); err != nil {
					t.Fatal(err)
				}
				waitForRunTerminalStatus(t, d, held)
			}
			expectNoQueuedBranch(t, started)
			for _, id := range []string{second, third} {
				if got := runStatus(t, d, id); got != types.RunPending {
					t.Fatalf("waiting run status = %s, want pending", got)
				}
			}
			releaseOnce.Do(func() { close(releaseSetup) })
			var first submitted
			select {
			case first = <-firstResult:
			case <-time.After(10 * time.Second):
				t.Fatal("first submission did not finish setup")
			}
			if first.err != nil {
				t.Fatal(first.err)
			}
			if !freeDuringSetup {
				if got := runStatus(t, d, first.id); got != types.RunPending {
					t.Fatalf("first waiting run status = %s, want pending", got)
				}
				if err := m.HandleCancel(held); err != nil {
					t.Fatal(err)
				}
			}
			for i, id := range []string{first.id, second, third} {
				expectQueuedBranch(t, started, []string{"feature/first", "feature/second", "feature/third"}[i])
				expectNoQueuedBranch(t, started)
				if err := m.HandleCancel(id); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMaxConcurrentRuns_SetupFailureReleasesAdmissionBeforeSlowCleanup(t *testing.T) {
	p, d := newParkedRunEnv(t)
	content, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(content, []byte("max_concurrent_runs: 1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, head := setupTestGitRepo(t, p, d, "failed-setup-admission")
	failedRepo := *repo
	failedRepo.WorkingPath = filepath.Join(t.TempDir(), "missing-checkout")
	started := make(chan string, 4)
	m := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})
	cleanupEntered := make(chan procreap.Worktree, 1)
	cleanupExited := make(chan struct{})
	cleanupBarrier, releaseCleanup := context.WithCancel(context.Background())
	var firstSweep atomic.Bool
	previousSweep := sweepRunWorktrees
	sweepRunWorktrees = func(ctx context.Context, root string, worktrees []procreap.Worktree, reason string) {
		if !firstSweep.CompareAndSwap(false, true) {
			return
		}
		defer close(cleanupExited)
		cleanupEntered <- worktrees[0]
		select {
		case <-cleanupBarrier.Done():
		case <-ctx.Done():
		}
	}
	failedDone := make(chan struct{})
	var setupErr error
	t.Cleanup(func() {
		releaseCleanup()
		m.Shutdown()
		select {
		case <-failedDone:
		case <-time.After(15 * time.Second):
			t.Error("failed setup cleanup did not finish")
		}
		sweepRunWorktrees = previousSweep
	})
	go func() {
		defer close(failedDone)
		_, setupErr = m.startRun(context.Background(), &failedRepo, "feature/failed", head, head, "push", nil, "", "", false, "", nil)
	}()
	var failedWorktree procreap.Worktree
	select {
	case failedWorktree = <-cleanupEntered:
	case <-failedDone:
		t.Fatalf("setup exited before reaching cleanup: %v", setupErr)
	case <-time.After(15 * time.Second):
		t.Fatal("failed setup did not reach cleanup")
	}
	const unique = "unique setup bytes\n"
	if err := os.WriteFile(filepath.Join(failedWorktree.Dir, "unique.txt"), []byte(unique), 0o644); err != nil {
		t.Fatal(err)
	}
	submit := func(branch string) string {
		t.Helper()
		id, err := m.startRun(context.Background(), repo, branch, head, head, "push", nil, "", "", false, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	secondID := submit("feature/second")
	expectQueuedBranch(t, started, "feature/second")
	select {
	case <-cleanupExited:
		t.Fatal("setup cleanup stopped blocking before the ready run started")
	default:
	}
	thirdID := submit("feature/third")
	expectNoQueuedBranch(t, started)
	if got := runStatus(t, d, thirdID); got != types.RunPending {
		t.Fatalf("third run status = %s, want pending", got)
	}
	releaseCleanup()
	select {
	case <-failedDone:
	case <-time.After(15 * time.Second):
		t.Fatal("failed setup did not finish after cleanup was released")
	}
	if setupErr == nil || !strings.Contains(setupErr.Error(), "configure worktree git identity") {
		t.Fatalf("setup error = %v, want identity failure", setupErr)
	}
	expectNoQueuedBranch(t, started)
	if got, err := os.ReadFile(filepath.Join(failedWorktree.Dir, "unique.txt")); err != nil || string(got) != unique {
		t.Fatalf("failed setup cleanup changed unique work: %q, %v", got, err)
	}
	if got, err := gitpkg.RunRaw(context.Background(), p.RepoDir(repo.ID), "show", gitpkg.DirtyWorkRef(failedWorktree.RunID)+"^3:unique.txt"); err != nil || string(got) != unique {
		t.Fatalf("failed setup snapshot lost unique bytes: %q, %v", got, err)
	}
	if got := runStatus(t, d, failedWorktree.RunID); got != types.RunFailed {
		t.Fatalf("setup run status = %s, want failed", got)
	}
	if err := m.HandleCancel(secondID); err != nil {
		t.Fatal(err)
	}
	expectQueuedBranch(t, started, "feature/third")
	if err := m.HandleCancel(thirdID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("admitted runs did not finish after cancellation")
	}
	expectNoQueuedBranch(t, started)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.runQueue) != 0 || len(m.executors) != 0 || len(m.cancels) != 0 || len(m.dones) != 0 {
		t.Fatal("setup failure left admission or execution accounting behind")
	}
}

func TestMaxConcurrentRuns_ConfigErrorRetainsCap(t *testing.T) {
	for _, failure := range []string{"invalid", "missing", "unreadable_replacement"} {
		t.Run(failure, func(t *testing.T) {
			started := make(chan string, 4)
			p, d := startTestDaemonWithMaxConcurrentRuns(t, 1, func() []pipeline.Step {
				return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
			})
			_, head := setupTestGitRepo(t, p, d, "unreadable-cap")
			gateDir := p.RepoDir("unreadable-cap")
			first := submitBranchRun(t, p, gateDir, "feature/first", head)
			expectQueuedBranch(t, started, "feature/first")
			second := submitBranchRun(t, p, gateDir, "feature/second", head)
			third := submitBranchRun(t, p, gateDir, "feature/third", head)
			if err := os.Rename(p.ConfigFile(), p.ConfigFile()+".old"); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "invalid":
				if err := os.WriteFile(p.ConfigFile(), []byte("agent: [invalid\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "unreadable_replacement":
				// A directory cannot be read as a config file, even under root or
				// on platforms that do not enforce Unix file permissions.
				if err := os.Mkdir(p.ConfigFile(), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cancelRun(t, p, first)
			expectQueuedBranch(t, started, "feature/second")
			expectNoQueuedBranch(t, started)
			if got := runStatus(t, d, third); got != types.RunPending {
				t.Fatalf("third run status = %s, want pending under retained cap", got)
			}
			cancelRun(t, p, second)
			expectQueuedBranch(t, started, "feature/third")
			cancelRun(t, p, third)
			waitForRunTerminalStatus(t, d, third)
		})
	}
}

// The cap reload contract distinguishes an initially absent config from losing
// a configured cap, while still allowing successful edits to change the cap.
func TestMaxConcurrentRuns_CapReloadsWithoutRestart(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	m := NewRunManager(nil, p, nil)
	for _, change := range []struct {
		name    string
		content string
		want    int
	}{
		{name: "initially absent", want: 0},
		{name: "configured", content: "max_concurrent_runs: 1\n", want: 1},
		{name: "missing after load", want: 1},
		{name: "unparsable after load", content: "agent: [invalid\n", want: 1},
		{name: "increased", content: "max_concurrent_runs: 2\n", want: 2},
		{name: "explicit unlimited", content: "max_concurrent_runs: 0\n", want: 0},
		{name: "restored cap", content: "max_concurrent_runs: 1\n", want: 1},
		{name: "unset in valid config", content: "agent: claude\n", want: 0},
	} {
		t.Run(change.name, func(t *testing.T) {
			if change.content == "" {
				if err := os.Remove(p.ConfigFile()); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(p.ConfigFile(), []byte(change.content), 0o644); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			got := m.executionCapLocked()
			m.mu.Unlock()
			if got != change.want {
				t.Fatalf("execution cap = %d, want %d", got, change.want)
			}
		})
	}
}

// The subscription contract requires every persisted cancellation to publish
// its terminal state before closing. An abrupt close or stale revision would
// leave a subscriber displaying the queued run until it reconnects.
func TestMaxConcurrentRuns_QueuedCancellationPublishesTerminalEvent(t *testing.T) {
	for _, action := range []string{"cancel", "supersede", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			content, err := os.ReadFile(p.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), append(content, []byte("max_concurrent_runs: 1\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			started := make(chan string, 4)
			m := NewRunManager(d, p, func() []pipeline.Step {
				return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
			})
			t.Cleanup(m.Shutdown)
			repo, head := setupTestGitRepo(t, p, d, "cancel-event")
			submit := func(branch string) string {
				t.Helper()
				id, err := m.startRun(context.Background(), repo, branch, head, head, "push", nil, "", "", false, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			submit("feature/held")
			expectQueuedBranch(t, started, "feature/held")
			queued := submit("feature/queued")
			if got := runStatus(t, d, queued); got != types.RunPending {
				t.Fatalf("queued run status = %s, want pending", got)
			}
			sub := subscribeDrained(t, m, queued)
			defer sub.Close()
			before := m.StateRev(queued)
			wantCause := types.RunCancelReasonAbortedByUser
			switch action {
			case "cancel":
				if err := m.HandleCancel(queued); err != nil {
					t.Fatal(err)
				}
			case "supersede":
				wantCause = types.RunCancelReasonSuperseded
				submit("feature/queued")
			case "shutdown":
				wantCause = "daemon shutting down"
				m.Shutdown()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			event, ok := sub.Next(ctx)
			if !ok || event.Type != ipc.EventRunCompleted {
				t.Fatalf("terminal event = %#v, ok = %t, want run_completed", event, ok)
			}
			if event.StateRev <= before || event.StateRev != m.StateRev(queued) {
				t.Fatalf("terminal revision = %d, before = %d, current = %d", event.StateRev, before, m.StateRev(queued))
			}
			if event.RunID != queued || event.RepoID != repo.ID || event.Branch == nil || *event.Branch != "feature/queued" ||
				event.Status == nil || *event.Status != string(types.RunCancelled) || event.Error == nil || *event.Error != wantCause {
				t.Fatalf("terminal event does not describe the cancelled run: %#v", event)
			}
			run, err := d.GetRun(queued)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunCancelled || run.Error == nil || *run.Error != wantCause {
				t.Fatalf("terminal event preceded persisted cancellation: %#v", run)
			}
			if event, ok := sub.Next(ctx); ok || ctx.Err() != nil {
				t.Fatalf("subscription did not close after completion: event = %#v, ok = %t, error = %v", event, ok, ctx.Err())
			}
		})
	}
}

func TestMaxConcurrentRuns_RecoveredParkedRunsShareCap(t *testing.T) {
	p, d := newParkedRunEnv(t)
	content, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(content, []byte("max_concurrent_runs: 1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 4)
	m := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{
			&mockApprovalStep{name: types.StepDocument},
			&notifyBlockStep{name: types.StepLint, started: started},
		}
	})
	t.Cleanup(m.Shutdown)
	repo, head := setupTestGitRepo(t, p, d, "recovered-cap")
	var ids []string
	for _, branch := range []string{"feature/first", "feature/second"} {
		run := insertParkedQueueTestRun(t, p, d, repo, branch, head)
		ids = append(ids, run.ID)
	}
	plans := m.recoverableParkedRuns(context.Background())
	if len(plans) != 2 {
		t.Fatalf("recoverable plans = %d, want two parked runs", len(plans))
	}
	m.resumeRecoveredRuns(plans)
	m.Shutdown()
	for _, id := range ids {
		run, err := d.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
			t.Fatalf("parked run lost on shutdown: status %s, awaiting %v", run.Status, run.AwaitingAgentSince)
		}
		if _, err := os.Stat(p.WorktreeDir(repo.ID, id)); err != nil {
			t.Fatalf("parked worktree lost on shutdown: %v", err)
		}
	}
	m = NewRunManager(d, p, m.steps)
	t.Cleanup(m.Shutdown)
	plans = m.recoverableParkedRuns(context.Background())
	if len(plans) != 2 {
		t.Fatalf("recoverable plans after shutdown = %d, want two", len(plans))
	}
	m.resumeRecoveredRuns(plans)
	approve := func(id string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := m.HandleRespond(id, types.StepDocument, types.ActionApprove, nil)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("recovered run never accepted approval: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := m.HandleRespond(ids[1], types.StepDocument, types.ActionApprove, nil); err == nil {
		t.Fatal("queued recovered run accepted approval while the first held its slot")
	}
	approve(ids[0])
	expectQueuedBranch(t, started, "feature/first")
	if err := m.HandleRespond(ids[1], types.StepDocument, types.ActionApprove, nil); err == nil {
		t.Fatal("second recovered run resumed before the first executor finished")
	}
	expectNoQueuedBranch(t, started)
	if err := m.HandleCancel(ids[0]); err != nil {
		t.Fatal(err)
	}
	approve(ids[1])
	expectQueuedBranch(t, started, "feature/second")
	if err := m.HandleCancel(ids[1]); err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalStatus(t, d, ids[1])
}

func insertParkedQueueTestRun(t *testing.T, p *paths.Paths, d *db.DB, repo *db.Repo, branch, head string) *db.Run {
	t.Helper()
	run, err := d.InsertRun(repo.ID, branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID), head); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"approval","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"needs approval"}`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, head, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepResult(run.ID, types.StepLint); err != nil {
		t.Fatal(err)
	}

	return run
}

func TestMaxConcurrentRuns_CancelRecoveredRunPreservesUnpublishedHead(t *testing.T) {
	for _, action := range []string{"cancel", "supersede", "conflicting_anchor"} {
		t.Run(action, func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			content, err := os.ReadFile(p.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), append(content, []byte("max_concurrent_runs: 1\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			m := NewRunManager(d, p, func() []pipeline.Step {
				return []pipeline.Step{&mockApprovalStep{name: types.StepDocument}, &mockPassStep{name: types.StepLint}}
			})
			t.Cleanup(m.Shutdown)
			repo, submitted := setupTestGitRepo(t, p, d, "cancel-recovered")
			insertParkedQueueTestRun(t, p, d, repo, "feature/held", submitted)
			run := insertParkedQueueTestRun(t, p, d, repo, "feature/queued", submitted)
			gateDir := p.RepoDir(repo.ID)
			gitCmd(t, gateDir, "update-ref", "refs/heads/"+run.Branch, submitted)
			workDir := p.WorktreeDir(repo.ID, run.ID)
			const fixContent = "unpublished pipeline fix\n"
			if err := os.WriteFile(filepath.Join(workDir, "pipeline-fix.txt"), []byte(fixContent), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, workDir, "add", "pipeline-fix.txt")
			gitCmd(t, workDir, "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "-m", "unpublished pipeline fix")
			fixed := gitOutput(t, workDir, "rev-parse", "HEAD")
			if err := d.UpdateRunHeadSHA(run.ID, fixed); err != nil {
				t.Fatal(err)
			}
			plans := m.recoverableParkedRuns(context.Background())
			if len(plans) != 2 {
				t.Fatalf("recoverable plans = %d, want two", len(plans))
			}
			m.resumeRecoveredRuns(plans)
			if err := m.HandleRespond(run.ID, types.StepDocument, types.ActionApprove, nil); err == nil {
				t.Fatal("second recovered run executed instead of queuing")
			}
			// Ensure closing the park must retain positive elapsed time, rather
			// than only clearing its awaiting-agent marker.
			time.Sleep(10 * time.Millisecond)
			wantCause := types.RunCancelReasonAbortedByUser
			if action == "supersede" {
				wantCause = types.RunCancelReasonSuperseded
				if _, err := m.startRun(context.Background(), repo, run.Branch, submitted, submitted, "push", nil, "", "", false, "", nil); err != nil {
					t.Fatal(err)
				}
			} else {
				if action == "conflicting_anchor" {
					gitCmd(t, gateDir, "update-ref", custody.RecoveryRef(run.ID), submitted)
				}
				if err := m.HandleCancel(run.ID); err != nil {
					t.Fatal(err)
				}
			}
			terminal, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Status != types.RunCancelled || terminal.Error == nil || *terminal.Error != wantCause {
				t.Fatalf("cancelled run = %#v, want cancelled with cause %s", terminal, wantCause)
			}
			// A terminal run no longer awaits an operator. Queued recovery must
			// close the park even though no executor ran its normal gate cleanup.
			if terminal.AwaitingAgentSince != nil {
				t.Errorf("cancelled queued recovered run still awaits an operator since %d", *terminal.AwaitingAgentSince)
			}
			if terminal.ParkedMS < 10 {
				t.Errorf("cancelled queued recovered run parked duration = %d ms, want at least 10 ms", terminal.ParkedMS)
			}
			if action == "conflicting_anchor" {
				if terminal.TerminalHeadVerifiedAt != nil {
					t.Fatal("conflicting anchor was recorded as verified")
				}
				if got := gitOutput(t, workDir, "rev-parse", "HEAD"); got != fixed {
					t.Fatalf("unpreserved worktree head = %s, want %s", got, fixed)
				}
				if got := gitOutput(t, gateDir, "rev-parse", custody.RecoveryRef(run.ID)); got != submitted {
					t.Fatalf("conflicting anchor changed to %s, want %s", got, submitted)
				}
				return
			}
			if terminal.TerminalHeadVerifiedAt == nil || terminal.HeadSHA != fixed {
				t.Fatalf("terminal head = %s, verified = %v, want verified %s", terminal.HeadSHA, terminal.TerminalHeadVerifiedAt, fixed)
			}
			if _, err := os.Stat(workDir); err != nil {
				t.Fatalf("cancelled checkout was removed without a stability proof: %v", err)
			}
			if got := gitOutput(t, gateDir, "rev-parse", custody.RecoveryRef(run.ID)); got != fixed {
				t.Fatalf("recovery anchor = %s, want %s", got, fixed)
			}
			if got := gitOutput(t, gateDir, "show", custody.RecoveryRef(run.ID)+":pipeline-fix.txt"); got != "unpublished pipeline fix" {
				t.Fatalf("preserved content = %q", got)
			}
			if got := gitOutput(t, gateDir, "rev-parse", "refs/heads/"+run.Branch); got != submitted {
				t.Fatalf("preservation changed gate head to %s, want %s", got, submitted)
			}
			if got, err := resolveRerunHead(context.Background(), gateDir, run.Branch, terminal); err != nil || got != fixed {
				t.Fatalf("rerun head = %s, error = %v, want %s", got, err, fixed)
			}
		})
	}
}
