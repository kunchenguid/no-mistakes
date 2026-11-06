package daemon

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #1691: updating no-mistakes must not fail runs already in progress.
// A scheduled restart lets the executing run finish on the running daemon,
// holds pushes that arrive meanwhile, and the restarted daemon runs them.

// heldStep executes until the test releases it, so "a run is executing when
// the restart is scheduled" is a state the test controls rather than a race.
type heldStep struct {
	name     types.StepName
	started  chan struct{}
	once     sync.Once
	released <-chan struct{}
}

func (s *heldStep) Name() types.StepName { return s.name }
func (s *heldStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.released:
		return &pipeline.StepOutcome{}, nil
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
}

func TestScheduledRestartLetsTheExecutingRunFinishAndRunsHeldPushesAfterward(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "restart-when-idle")
	gate := p.RepoDir(repo.ID)
	release := make(chan struct{})
	step := &heldStep{name: types.StepReview, started: make(chan struct{}), released: release}
	steps := func() []pipeline.Step { return []pipeline.Step{step} }

	first := make(chan error, 1)
	go func() { first <- RunWithOptions(p, d, steps) }()
	waitForDaemonReady(t, p)

	executing := submitBranchRun(t, p, gate, "feature-a", headSHA)
	<-step.started

	var scheduled ipc.RestartStatusResult
	if err := callDaemon(p, ipc.MethodScheduleRestart, &ipc.ScheduleRestartParams{}, &scheduled); err != nil {
		t.Fatalf("schedule restart: %v", err)
	}
	if !scheduled.Pending || scheduled.WaitingRuns != 1 {
		t.Fatalf("scheduled restart = %+v, want pending and waiting for the one executing run", scheduled)
	}

	// A push during the wait is held, not started on the old daemon.
	if held := submitBranchRun(t, p, gate, "feature-b", headSHA); held != "" {
		t.Fatalf("push during the wait started run %s on the old daemon, want it held", held)
	}
	if run, err := d.GetActiveRun(repo.ID, "feature-b"); err != nil || run != nil {
		t.Fatalf("held push has a run before the restart: %+v, %v", run, err)
	}
	// A launch other than a plain push is refused, so its caller retries.
	err := callDaemon(p, ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: gate, Ref: "refs/heads/feature-c", Old: zeroSHA, New: headSHA, LaunchNonce: "nonce-1",
	}, &ipc.PushReceivedResult{})
	if err == nil || !strings.Contains(err.Error(), "restarting for an update") {
		t.Fatalf("proof-mode launch during the wait = %v, want the restart refusal", err)
	}
	var status ipc.RestartStatusResult
	if err := callDaemon(p, ipc.MethodRestartStatus, &ipc.ScheduleRestartParams{}, &status); err != nil {
		t.Fatal(err)
	}
	if status.WaitingRuns != 1 || status.HeldPushes != 1 {
		t.Fatalf("restart status = %+v, want 1 waiting run and 1 held push", status)
	}
	select {
	case err := <-first:
		t.Fatalf("daemon stopped while a run was executing: %v", err)
	default:
	}

	close(release)
	if run := waitForRunTerminalState(t, d, executing); run.Status != types.RunCompleted {
		t.Fatalf("executing run ended %s (%v), want completed on the daemon it started on", run.Status, errText(run.Error))
	}
	select {
	case err := <-first:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("daemon stopped with %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not stop for the restart once the run finished")
	}

	// The restarted daemon starts the held push.
	second := make(chan error, 1)
	go func() { second <- RunWithOptions(p, d, steps) }()
	waitForDaemonReady(t, p)
	t.Cleanup(func() { stopParkedRunDaemon(t, p, second) })
	heldRun := waitForBranchRun(t, d, repo.ID, "feature-b")
	if run := waitForRunTerminalState(t, d, heldRun.ID); run.Status != types.RunCompleted {
		t.Fatalf("held push ended %s (%v), want completed on the restarted daemon", run.Status, errText(run.Error))
	}
	if _, err := os.Stat(p.HeldPushesFile()); !os.IsNotExist(err) {
		t.Fatalf("held pushes file should be removed once replayed, stat err = %v", err)
	}
}

func TestScheduledRestartDoesNotWaitForARunParkedAtAGate(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "restart-parked")
	first := make(chan error, 1)
	go func() {
		first <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockApprovalStep{name: types.StepDocument}}
		})
	}()
	waitForDaemonReady(t, p)

	runID := submitBranchRun(t, p, p.RepoDir(repo.ID), "feature-a", headSHA)
	waitForParked(t, d, runID)

	var scheduled ipc.RestartStatusResult
	if err := callDaemon(p, ipc.MethodScheduleRestart, &ipc.ScheduleRestartParams{}, &scheduled); err != nil {
		t.Fatalf("schedule restart: %v", err)
	}
	if scheduled.WaitingRuns != 0 {
		t.Fatalf("scheduled restart waits for %d runs, want 0: a parked run survives a clean stop", scheduled.WaitingRuns)
	}
	select {
	case err := <-first:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("daemon stopped with %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a run parked at a gate held the restart up")
	}
	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		t.Fatalf("parked run after the restart stop = %s (awaiting since %v), want still parked for resume", run.Status, run.AwaitingAgentSince)
	}
}

const zeroSHA = "0000000000000000000000000000000000000000"

func callDaemon(p *paths.Paths, method string, params, result any) error {
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call(method, params, result)
}

func waitForBranchRun(t *testing.T, d *db.DB, repoID, branch string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := d.GetRunsByRepo(repoID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.Branch == branch {
				return run
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no run for branch %s appeared", branch)
	return nil
}

func waitForParked(t *testing.T, d *db.DB, runID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.AwaitingAgentSince != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never parked at its gate", runID)
}

func errText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A push held while a replay is running belongs to the next restart: the
// replay must never delete it along with the pushes it just handed over.
func TestReplayHeldPushesKeepsAPushHeldWhileReplaying(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "replay-keeps-held")
	m := NewRunManager(d, p, nil)
	m.restart.pending.Store(true)

	// With a restart pending, replaying a held push holds it again, exactly
	// as a push arriving mid-replay would be held.
	params := &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/feature-a", Old: zeroSHA, New: headSHA}
	if err := m.holdPush(params); err != nil {
		t.Fatal(err)
	}
	if got := countHeldPushes(p.HeldPushesFile()); got != 1 {
		t.Fatalf("held pushes before replay = %d, want 1", got)
	}

	m.replayHeldPushes(t.Context())

	if got := countHeldPushes(p.HeldPushesFile()); got != 1 {
		t.Fatalf("held pushes after replay = %d, want the push held during replay kept", got)
	}
	if _, err := os.Stat(p.HeldPushesFile() + ".replay"); !os.IsNotExist(err) {
		t.Fatalf("claimed replay file should be removed, stat err = %v", err)
	}
}

// A held push that fails only because the daemon is stopping must survive for
// the next start; dropping it would lose a push the hook never retries.
func TestReplayHeldPushFileKeepsTheFileWhenTheDaemonIsStopping(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "replay-stopping")
	m := NewRunManager(d, p, nil)
	m.shuttingDown.Store(true)

	params := &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/feature-a", Old: zeroSHA, New: headSHA}
	if err := m.holdPush(params); err != nil {
		t.Fatal(err)
	}
	replayPath := p.HeldPushesFile() + ".replay"
	if err := os.Rename(p.HeldPushesFile(), replayPath); err != nil {
		t.Fatal(err)
	}

	if m.replayHeldPushFile(t.Context(), replayPath) {
		t.Fatal("replay should stop when the daemon is stopping")
	}
	if got := countHeldPushes(replayPath); got != 1 {
		t.Fatalf("held pushes kept = %d, want 1", got)
	}
}

// A gate response that passed its due check must finish dispatching before the
// restart is marked due and recounted; otherwise the stop cancels the run the
// response just resumed.
func TestRestartDueWaitsForAGateResponseInFlight(t *testing.T) {
	p, d := newParkedRunEnv(t)
	m := NewRunManager(d, p, nil)

	release, err := m.beginGateResponse()
	if err != nil {
		t.Fatal(err)
	}
	marked := make(chan bool, 1)
	go func() { marked <- m.markRestartDue() }()

	select {
	case <-marked:
		t.Fatal("restart was marked due while a gate response was still dispatching")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case idle := <-marked:
		if !idle {
			t.Fatal("idle daemon should be marked due once the response finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart never marked due after the response finished")
	}
	if _, err := m.beginGateResponse(); !errors.Is(err, errRestartPending) {
		t.Fatalf("response after the due mark = %v, want the restart refusal", err)
	}
}

// Greptile on #1697: while the pushes an earlier daemon held are replaying, a
// fresh push is held behind them, so an older held push can never replace the
// run of a newer push to the same branch.
func TestFreshPushDuringReplayIsHeldBehindTheBacklog(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "replay-order")
	m := NewRunManager(d, p, nil)
	gate := p.RepoDir(repo.ID)
	older := &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature-a", Old: zeroSHA, New: zeroSHA[:39] + "1"}
	if err := m.holdPush(older); err != nil {
		t.Fatal(err)
	}
	m.restart.replaying.Store(heldPushesWaiting(p.HeldPushesFile()))
	if !m.restart.replaying.Load() {
		t.Fatal("held pushes on disk should mark the replay started")
	}

	newer := &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature-a", Old: zeroSHA, New: headSHA}
	runID, err := m.HandlePushReceived(t.Context(), newer)
	if err != nil || runID != "" {
		t.Fatalf("fresh push during replay = %q, %v; want it held", runID, err)
	}
	data, err := os.ReadFile(p.HeldPushesFile())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], older.New) || !strings.Contains(lines[1], headSHA) {
		t.Fatalf("held pushes = %q, want the older push first and the fresh push behind it", lines)
	}
	if run, err := d.GetActiveRun(repo.ID, "feature-a"); err != nil || run != nil {
		t.Fatalf("fresh push started a run ahead of the replay: %+v, %v", run, err)
	}
}

// Greptile on #1697: a plain push that passed the first check and was then
// refused because a restart was scheduled meanwhile is held, not dropped: the
// hook never retries it.
func TestPushRefusedByALateRestartIsHeld(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "late-restart")
	m := NewRunManager(d, p, nil)
	m.restart.beforeStartRun = func() { m.restart.pending.Store(true) }

	params := &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/feature-a", Old: zeroSHA, New: headSHA}
	runID, err := m.HandlePushReceived(t.Context(), params)
	if err != nil || runID != "" {
		t.Fatalf("push refused by a late restart = %q, %v; want it held", runID, err)
	}
	if got := countHeldPushes(p.HeldPushesFile()); got != 1 {
		t.Fatalf("held pushes = %d, want the refused push held", got)
	}
}

// Kun's triage on #1697: a second restart scheduled while held pushes replay
// must not re-hold the unreplayed older pushes behind a fresh push held during
// the replay, or the next daemon starts the older push last and it replaces
// the newer run on the same branch. The unreplayed rest stays in the replay
// file, which the next daemon replays first.
func TestRestartDuringReplayKeepsTheUnreplayedPushesAheadOfNewerOnes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule func(m *RunManager)
	}{
		{"scheduled before the push is handed over", func(m *RunManager) { m.restart.pending.Store(true) }},
		{"scheduled while the push waits for its branch", func(m *RunManager) {
			m.restart.beforeStartRun = func() { m.restart.pending.Store(true) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			repo, headSHA := setupTestGitRepo(t, p, d, "second-restart")
			m := NewRunManager(d, p, nil)
			gate := p.RepoDir(repo.ID)
			older := &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature-a", Old: zeroSHA, New: headSHA}
			olderOther := &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature-b", Old: zeroSHA, New: headSHA}
			fresh := &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature-a", Old: headSHA, New: zeroSHA[:39] + "f"}

			// An earlier daemon's backlog is claimed for replay, and a fresh
			// push arrived during the replay and was held behind it.
			replayPath := p.HeldPushesFile() + ".replay"
			for _, push := range []*ipc.PushReceivedParams{older, olderOther} {
				if err := m.holdPush(push); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(p.HeldPushesFile(), replayPath); err != nil {
				t.Fatal(err)
			}
			if err := m.holdPush(fresh); err != nil {
				t.Fatal(err)
			}

			tc.schedule(m)
			if m.replayHeldPushFile(t.Context(), replayPath) {
				t.Fatal("replay should stop once another restart is scheduled")
			}

			replay, err := os.ReadFile(replayPath)
			if err != nil {
				t.Fatalf("unreplayed pushes should stay in the replay file: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(replay)), "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], "feature-a") || !strings.Contains(lines[1], "feature-b") {
				t.Fatalf("replay file = %q, want both unreplayed pushes in arrival order", lines)
			}
			held, err := os.ReadFile(p.HeldPushesFile())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Split(strings.TrimSpace(string(held)), "\n"); len(got) != 1 || !strings.Contains(got[0], fresh.New) {
				t.Fatalf("held pushes = %q, want only the fresh push, behind the replay file", got)
			}
			if run, err := d.GetActiveRun(repo.ID, "feature-a"); err != nil || run != nil {
				t.Fatalf("deferred replay started a run: %+v, %v", run, err)
			}
		})
	}
}
