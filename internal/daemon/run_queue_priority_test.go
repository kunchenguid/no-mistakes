package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// queueHarness drives the admission queue directly, without a daemon or a
// database, so ordering, the ceiling and the running set are observable
// without timing. A "run" here is a queue entry whose spawn only records that
// it started; finish releases its slot exactly as an executor goroutine does.
type queueHarness struct {
	t         *testing.T
	m         *RunManager
	names     map[uint64]string
	sequences map[string]uint64
	started   []string
	cancelled []string
}

func newQueueHarness(t *testing.T, maxConcurrent int) *queueHarness {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(fmt.Sprintf("max_concurrent_runs: %d\n", maxConcurrent)), 0o644); err != nil {
		t.Fatal(err)
	}
	return &queueHarness{
		t:         t,
		m:         NewRunManager(nil, p, func() []pipeline.Step { return nil }),
		names:     map[uint64]string{},
		sequences: map[string]uint64{},
	}
}

// reserve takes an admission reservation, as a submission does before setup.
func (h *queueHarness) reserve(name string, priority int) {
	h.t.Helper()
	sequence := h.m.reserveAdmission(priority)
	h.names[sequence] = name
	h.sequences[name] = sequence
}

// admit completes setup for a reserved run, which makes it startable.
func (h *queueHarness) admit(name string) {
	h.t.Helper()
	sequence, ok := h.sequences[name]
	if !ok {
		h.t.Fatalf("admit %s: never reserved", name)
	}
	h.m.admitRun(sequence, queuedRun{
		runID:  name,
		branch: name,
		cancel: func(error) { h.cancelled = append(h.cancelled, name) },
		done:   make(chan struct{}),
		spawn:  func() { h.started = append(h.started, name) },
	})
	h.assertCeiling()
}

// submit reserves and completes setup in one step.
func (h *queueHarness) submit(name string, priority int) {
	h.t.Helper()
	h.reserve(name, priority)
	h.admit(name)
}

// finish ends a started run and frees its slot, as the executor goroutine's
// deferred cleanup does.
func (h *queueHarness) finish(name string) {
	h.t.Helper()
	h.m.mu.Lock()
	if _, ok := h.m.executors[name]; !ok {
		h.m.mu.Unlock()
		h.t.Fatalf("finish %s: not running", name)
	}
	delete(h.m.executors, name)
	delete(h.m.cancels, name)
	delete(h.m.dones, name)
	delete(h.m.ciWakes, name)
	h.m.mu.Unlock()
	h.m.pumpQueue()
	h.assertCeiling()
}

func (h *queueHarness) queueOrder() []string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	order := make([]string, 0, len(h.m.runQueue))
	for _, qe := range h.m.runQueue {
		order = append(order, h.names[qe.sequence])
	}
	return order
}

func (h *queueHarness) running() []string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	var ids []string
	for id := range h.m.executors {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (h *queueHarness) assertCeiling() {
	h.t.Helper()
	h.m.mu.Lock()
	running, limit := len(h.m.executors), h.m.maxConcurrentRuns
	h.m.mu.Unlock()
	if limit > 0 && running > limit {
		h.t.Fatalf("%d runs executing, above max_concurrent_runs %d", running, limit)
	}
}

func (h *queueHarness) expectStarted(want ...string) {
	h.t.Helper()
	if !slices.Equal(h.started, want) {
		h.t.Fatalf("started = %v, want %v", h.started, want)
	}
}

func (h *queueHarness) expectQueue(want ...string) {
	h.t.Helper()
	if got := h.queueOrder(); !slices.Equal(got, want) {
		h.t.Fatalf("queue = %v, want %v", got, want)
	}
}

// A higher priority is admitted first; equal priorities keep submission order.
func TestRunQueue_PriorityBeforeSubmissionOrder(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.reserve("ordinary-1", 0)
	h.reserve("p0-1", 10)
	h.reserve("ordinary-2", 0)
	h.reserve("red-main", 20)
	h.reserve("p0-2", 10)
	h.reserve("ordinary-3", 0)
	h.expectQueue("red-main", "p0-1", "p0-2", "ordinary-1", "ordinary-2", "ordinary-3")
}

// Without any priority the queue is exactly today's FIFO.
func TestRunQueue_OrdinaryRunsStayFIFO(t *testing.T) {
	h := newQueueHarness(t, 1)
	for i := 1; i <= 4; i++ {
		h.reserve(fmt.Sprintf("ordinary-%d", i), 0)
	}
	h.expectQueue("ordinary-1", "ordinary-2", "ordinary-3", "ordinary-4")
}

// Completing setup must not lose the reserved priority: a later reservation is
// still ordered against it.
func TestRunQueue_AdmittedRunKeepsItsReservedPriority(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit("running", 0)
	h.submit("p0", 10)
	h.reserve("ordinary", 0)
	h.reserve("later-p0", 10)
	h.reserve("red-main", 20)
	h.expectQueue("red-main", "p0", "later-p0", "ordinary")
	h.expectStarted("running")
}

// Priority reorders only the waiting runs: running work is never cancelled or
// displaced, the ceiling is never exceeded, and freed slots go to the highest
// priority first and then to submission order.
func TestRunQueue_PriorityStartsFirstWithoutTouchingRunningWork(t *testing.T) {
	h := newQueueHarness(t, 2)
	h.submit("running-a", 0)
	h.submit("running-b", 0)
	h.expectStarted("running-a", "running-b")

	h.submit("ordinary-1", 0)
	h.submit("ordinary-2", 0)
	h.submit("p0", 10)
	h.submit("red-main", 20)

	// The ceiling holds: nothing else starts while both slots are taken, and
	// no running run is cancelled to make room.
	h.expectStarted("running-a", "running-b")
	if got := h.running(); !slices.Equal(got, []string{"running-a", "running-b"}) {
		t.Fatalf("running = %v, want the two original runs", got)
	}
	if len(h.cancelled) != 0 {
		t.Fatalf("cancelled %v, want no running run cancelled", h.cancelled)
	}
	h.expectQueue("red-main", "p0", "ordinary-1", "ordinary-2")

	h.finish("running-a")
	h.expectStarted("running-a", "running-b", "red-main")
	h.finish("red-main")
	h.expectStarted("running-a", "running-b", "red-main", "p0")
	h.finish("running-b")
	h.expectStarted("running-a", "running-b", "red-main", "p0", "ordinary-1")
	h.finish("p0")
	h.expectStarted("running-a", "running-b", "red-main", "p0", "ordinary-1", "ordinary-2")
	h.expectQueue()
	if len(h.cancelled) != 0 {
		t.Fatalf("cancelled %v, want no run cancelled", h.cancelled)
	}
}

// A priority run that arrives while ordinary runs wait takes the next freed
// slot, but only that one: the ceiling stays the ceiling.
func TestRunQueue_PriorityTakesOnlyTheNextFreedSlot(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit("running", 0)
	h.submit("ordinary", 0)
	h.submit("red-main", 20)
	h.expectStarted("running")
	h.finish("running")
	h.expectStarted("running", "red-main")
	h.expectQueue("ordinary")
}

// The existing head-of-line rule is unchanged: under a cap, the queue head
// still in setup holds back later ready runs, so a priority run in setup is
// not overtaken by an ordinary ready run. Its reservation holds nothing once
// setup fails.
func TestRunQueue_PriorityRunInSetupKeepsTheHeadUntilItsSetupEnds(t *testing.T) {
	h := newQueueHarness(t, 1)
	h.submit("running", 0)
	h.submit("ordinary", 0)
	h.reserve("red-main", 20) // setup still in progress
	h.finish("running")
	h.expectStarted("running")
	h.admit("red-main")
	h.expectStarted("running", "red-main")

	h.reserve("failing-p0", 10) // its setup will fail
	h.finish("red-main")
	h.expectStarted("running", "red-main")
	h.m.releaseAdmission(h.sequences["failing-p0"])
	h.expectStarted("running", "red-main", "ordinary")
}

// Without a cap every ready run starts at once and a run still in setup
// blocks nobody, priority or not.
func TestRunQueue_UnlimitedCapIgnoresPriority(t *testing.T) {
	h := newQueueHarness(t, 0)
	h.reserve("red-main", 20) // setup still in progress
	h.submit("ordinary", 0)
	h.expectStarted("ordinary")
	h.admit("red-main")
	h.expectStarted("ordinary", "red-main")
}

// Recovered parked runs resume one by one, each taking a free slot as soon as
// it is admitted, so they must resume in priority order: an older ordinary run
// must not take the only slot ahead of a priority run.
func TestRunQueue_RecoveredRunsResumeByPersistedPriority(t *testing.T) {
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
	repo, head := setupTestGitRepo(t, p, d, "recovered-priority")
	// The ordinary run is older, so creation order alone would resume it first.
	ordinary := insertParkedQueueTestRun(t, p, d, repo, "feature/ordinary", head)
	urgent := insertParkedQueueTestRun(t, p, d, repo, "feature/red-main", head)
	if err := d.UpdateRunPriority(urgent.ID, 20); err != nil {
		t.Fatal(err)
	}
	plans := m.recoverableParkedRuns(context.Background())
	if len(plans) != 2 {
		t.Fatalf("recoverable plans = %d, want two parked runs", len(plans))
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
	if err := m.HandleRespond(ordinary.ID, types.StepDocument, types.ActionApprove, nil); err == nil {
		t.Fatal("older ordinary recovered run took the only slot ahead of the priority run")
	}
	approve(urgent.ID)
	expectQueuedBranch(t, started, "feature/red-main")
	if err := m.HandleCancel(urgent.ID); err != nil {
		t.Fatal(err)
	}
	approve(ordinary.ID)
	expectQueuedBranch(t, started, "feature/ordinary")
	if err := m.HandleCancel(ordinary.ID); err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalStatus(t, d, ordinary.ID)
}

func submitPriorityBranchRun(t *testing.T, p *paths.Paths, gateDir, branch, headSHA string, priority int) (string, error) {
	t.Helper()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:     gateDir,
		Ref:      "refs/heads/" + branch,
		Old:      "0000000000000000000000000000000000000000",
		New:      headSHA,
		Priority: priority,
	}, &result)
	return result.RunID, err
}

// End to end through the push IPC: priority runs submitted after an ordinary
// one start first, the run already executing is left alone, and each run's
// priority is persisted and reported by get_run.
func TestMaxConcurrentRuns_PushedPriorityStartsAheadOfOrdinaryRuns(t *testing.T) {
	started := make(chan string, 8)
	p, d := startTestDaemonWithMaxConcurrentRuns(t, 1, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "max-conc-priority-repo")
	gateDir := p.RepoDir("max-conc-priority-repo")

	heldID := submitBranchRun(t, p, gateDir, "feature/held", headSHA)
	waitForStartedBranch(t, started, "feature/held")

	ids := map[string]string{}
	for _, sub := range []struct {
		branch   string
		priority int
	}{{"feature/ordinary", 0}, {"feature/p0", 10}, {"feature/red-main", 20}} {
		id, err := submitPriorityBranchRun(t, p, gateDir, sub.branch, headSHA, sub.priority)
		if err != nil {
			t.Fatal(err)
		}
		ids[sub.branch] = id
		run, err := d.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunPending || run.Priority != sub.priority {
			t.Fatalf("%s: status %s priority %d, want pending with priority %d", sub.branch, run.Status, run.Priority, sub.priority)
		}
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			t.Fatal(err)
		}
		var got ipc.GetRunResult
		err = client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: id}, &got)
		client.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got.Run == nil || got.Run.Priority != sub.priority {
			t.Fatalf("%s: get_run priority = %+v, want %d", sub.branch, got.Run, sub.priority)
		}
	}

	// The executing run is neither displaced nor cancelled by the arrivals.
	expectNoQueuedBranch(t, started)
	if got := runStatus(t, d, heldID); got != types.RunRunning {
		t.Fatalf("held run status = %s, want running", got)
	}

	cancelRun(t, p, heldID)
	expectQueuedBranch(t, started, "feature/red-main")
	expectNoQueuedBranch(t, started)
	cancelRun(t, p, ids["feature/red-main"])
	expectQueuedBranch(t, started, "feature/p0")
	expectNoQueuedBranch(t, started)
	cancelRun(t, p, ids["feature/p0"])
	expectQueuedBranch(t, started, "feature/ordinary")
	cancelRun(t, p, ids["feature/ordinary"])
	for _, id := range append([]string{heldID}, ids["feature/ordinary"], ids["feature/p0"], ids["feature/red-main"]) {
		waitForRunTerminalStatus(t, d, id)
	}
}

// An out-of-range priority is refused before any run is created or any
// active run is superseded.
func TestMaxConcurrentRuns_PushedPriorityOutOfRangeIsRefused(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithMaxConcurrentRuns(t, 1, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})
	repo, headSHA := setupTestGitRepo(t, p, d, "max-conc-priority-range")
	gateDir := p.RepoDir("max-conc-priority-range")
	activeID := submitBranchRun(t, p, gateDir, "feature/active", headSHA)
	waitForStartedBranch(t, started, "feature/active")
	for _, priority := range []int{-1, types.RunPriorityMax + 1} {
		if _, err := submitPriorityBranchRun(t, p, gateDir, "feature/active", headSHA, priority); err == nil {
			t.Fatalf("priority %d accepted, want refusal", priority)
		}
	}
	if got := runStatus(t, d, activeID); got != types.RunRunning {
		t.Fatalf("active run status = %s after refused submissions, want running", got)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d after refused submissions, want only the active run", len(runs))
	}
	cancelRun(t, p, activeID)
	waitForRunTerminalStatus(t, d, activeID)
}

// A rerun re-validates the same work, so it keeps the selected run's priority;
// a caller-named priority replaces it.
func TestRerunInheritsPriorityUnlessTheCallerNamesOne(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "priority-rerun-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:     p.RepoDir("priority-rerun-repo"),
		Ref:      "refs/heads/main",
		Old:      "0000000000000000000000000000000000000000",
		New:      headSHA,
		Priority: 20,
	}, &first); err != nil {
		t.Fatal(err)
	}
	if got := waitForRunTerminalState(t, d, first.RunID); got.Priority != 20 {
		t.Fatalf("first run priority = %d, want 20", got.Priority)
	}

	var inherited ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "priority-rerun-repo", Branch: "main", PreviousRunID: first.RunID}, &inherited); err != nil {
		t.Fatal(err)
	}
	if got := waitForRunTerminalState(t, d, inherited.RunID); got.Priority != 20 {
		t.Fatalf("rerun priority = %d, want inherited 20", got.Priority)
	}

	var named ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "priority-rerun-repo", Branch: "main", PreviousRunID: first.RunID, Priority: 10}, &named); err != nil {
		t.Fatal(err)
	}
	if got := waitForRunTerminalState(t, d, named.RunID); got.Priority != 10 {
		t.Fatalf("rerun priority = %d, want the named 10", got.Priority)
	}
}

// An explicit priority 0 on a rerun is a request for an ordinary run, not
// "unspecified": it must not inherit the selected run's urgency.
func TestRerunExplicitOrdinaryPriorityDoesNotInherit(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "priority-rerun-zero-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:     p.RepoDir("priority-rerun-zero-repo"),
		Ref:      "refs/heads/main",
		Old:      "0000000000000000000000000000000000000000",
		New:      headSHA,
		Priority: 20,
	}, &first); err != nil {
		t.Fatal(err)
	}
	if got := waitForRunTerminalState(t, d, first.RunID); got.Priority != 20 {
		t.Fatalf("first run priority = %d, want 20", got.Priority)
	}

	// Raw params so the request reads as the CLI's explicit --priority 0.
	explicit := json.RawMessage(fmt.Sprintf(`{"repo_id":"priority-rerun-zero-repo","branch":"main","previous_run_id":%q,"priority":0,"priority_set":true}`, first.RunID))
	var ordinary ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, explicit, &ordinary); err != nil {
		t.Fatal(err)
	}
	if got := waitForRunTerminalState(t, d, ordinary.RunID); got.Priority != 0 {
		t.Fatalf("rerun with explicit --priority 0 got priority %d, want 0", got.Priority)
	}
}
