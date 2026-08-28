package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// wedgedCustodyAbortFixture builds the issue #824 record on real state: a
// registered operator worktree, a real local gate whose branch sits at a
// LATER run's head, and a terminal run row with no push binding whose recorded
// pipeline head is in no object store at all. Aborting that run cannot cancel
// anything, so its response is the only place left to name a settlement.
func wedgedCustodyAbortFixture(t *testing.T) (string, *paths.Paths, string) {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)

	root := t.TempDir()
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	cliGit(t, local, "checkout", "-b", "feature/wedged")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "feature")
	submitted := cliGit(t, local, "rev-parse", "HEAD")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, filepath.Join(root, "remote.git"), "main")
	if err != nil {
		t.Fatal(err)
	}

	gate := p.RepoDir(repo.ID)
	cliGit(t, root, "init", "--bare", gate)
	cliGit(t, local, "push", gate, "refs/heads/feature/wedged:refs/heads/feature/wedged")
	// A later run pushed its own head onto the gate branch and was cancelled,
	// so the gate no longer names this run's head either.
	pipelineClone := filepath.Join(root, "later-run")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", gate, pipelineClone)
	cliGit(t, pipelineClone, "config", "user.name", "Test")
	cliGit(t, pipelineClone, "config", "user.email", "test@example.com")
	cliGit(t, pipelineClone, "checkout", "feature/wedged")
	if err := os.WriteFile(filepath.Join(pipelineClone, "later.txt"), []byte("later run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipelineClone, "add", "later.txt")
	cliGit(t, pipelineClone, "commit", "-m", "no-mistakes(review): later run fix")
	cliGit(t, pipelineClone, "push", "origin", "HEAD:refs/heads/feature/wedged")

	run, err := database.InsertRun(repo.ID, "feature/wedged", submitted, submitted)
	if err != nil {
		t.Fatal(err)
	}
	// The recorded pipeline head is gone from every reachable object store.
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	chdir(t, local)
	return run.ID, p, local
}

func assertNamesKeepLocalSettlement(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{"aborted: false", "run_status: failed", "already terminal"} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal-run abort output missing %q:\n%s", want, out)
		}
	}
	// The prescribed action is the FIRST help entry; the standing branch-sync
	// guidance that follows it names `--recover` in general prose, so only the
	// prescribed entry can be asserted. On this record the plain recovery is
	// the command that always refuses, and it must never be prescribed here.
	prescribed := ""
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "help[") {
			_, rest, _ := strings.Cut(trimmed, ": ")
			prescribed, _, _ = strings.Cut(rest, ",")
			break
		}
	}
	if prescribed != "Run `no-mistakes axi sync --recover --keep-local`" {
		t.Errorf("terminal-run abort prescribed %q, want the keep-local settlement:\n%s", prescribed, out)
	}
}

// TestAxiAbortOfTerminalRunNamesTheSupportedCustodySettlement is the issue #824
// regression on the abort surface. Aborting an already-terminal run is an
// idempotent no-op by design - there is nothing left to cancel - but the
// reporter was left with that no-op plus a `sync --check` that kept offering a
// recovery which always refused. The no-op must therefore name the command
// that can actually settle the record.
func TestAxiAbortOfTerminalRunNamesTheSupportedCustodySettlement(t *testing.T) {
	t.Run("daemon unavailable", func(t *testing.T) {
		runID, _, _ := wedgedCustodyAbortFixture(t)

		out, err := executeCmd("axi", "abort", "--run", runID)
		t.Logf("daemon-down terminal abort output:\n%s", out)
		if err != nil {
			t.Fatalf("terminal run must resolve idempotently: %v\n%s", err, out)
		}
		assertNamesKeepLocalSettlement(t, out)
	})

	t.Run("daemon reports no active run", func(t *testing.T) {
		runID, p, _ := wedgedCustodyAbortFixture(t)
		startInactiveAbortDaemon(t, p, runID)

		out, err := executeCmd("axi", "abort", "--run", runID)
		t.Logf("daemon-up terminal abort output:\n%s", out)
		if err != nil {
			t.Fatalf("terminal run must resolve idempotently: %v\n%s", err, out)
		}
		assertNamesKeepLocalSettlement(t, out)
	})
}

// startInactiveAbortDaemon serves the exact daemon responses a terminal run
// produces: cancel_run has nothing active to cancel, and get_run reports the
// durable terminal record.
func startInactiveAbortDaemon(t *testing.T, p *paths.Paths, runID string) {
	t.Helper()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: false}, nil
	})
	srv.Handle(ipc.MethodCancelRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, noActiveRunErr(runID)
	})
	srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: &ipc.RunInfo{
			ID: runID, Branch: "feature/wedged", Status: types.RunFailed,
		}}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client, dialErr := ipc.Dial(p.Socket()); dialErr == nil {
			client.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake daemon did not become reachable")
}

// TestBareAbortNoOpNeverPrescribesLaunchingAPipeline is the review regression
// for the branch-scoped abort no-op. The `--run` sites already guard their
// help behind an exact run match, but the bare-abort site emitted the branch's
// next action unconditionally - so on a branch whose custody was already
// returned it answered an abort by prescribing `axi run`, telling the operator
// to LAUNCH a pipeline. Abort help exists to name a custody settlement (issue
// #824 constraint 2); it must never prescribe starting a run.
func TestBareAbortNoOpNeverPrescribesLaunchingAPipeline(t *testing.T) {
	runID, p, _ := wedgedCustodyAbortFixture(t)
	// Custody already returned: the branch's own next action becomes
	// run_pipeline, which an abort response must not hand back.
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunCustodyReturned(runID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	startNoActiveRunDaemon(t, p)

	out, err := executeCmd("axi", "abort")
	t.Logf("bare abort on a released branch:\n%s", out)
	if err != nil {
		t.Fatalf("bare abort with no active run must be a no-op success: %v\n%s", err, out)
	}
	if !strings.Contains(out, "aborted: false") {
		t.Errorf("bare abort no-op output missing %q:\n%s", "aborted: false", out)
	}
	// The structured branch_sync object still REPORTS the branch's own
	// next_action (run_pipeline here); that is ownership state, not a
	// prescription. Only the abort's own help prescribes a command, and a
	// non-settlement action must yield no help at all.
	if strings.Contains(out, "Run `no-mistakes axi run") {
		t.Errorf("abort prescribed launching a pipeline:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "help[") {
			t.Errorf("abort emitted help for a non-settlement next action:\n%s", line)
		}
	}
}

// startNoActiveRunDaemon serves a daemon with no active run for the branch, so
// the bare abort takes its documented idempotent no-op path.
func startNoActiveRunDaemon(t *testing.T, p *paths.Paths) {
	t.Helper()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: false}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: nil}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client, dialErr := ipc.Dial(p.Socket()); dialErr == nil {
			client.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake daemon did not become reachable")
}

// TestBareAbortNoOpEmitsNoHelpForOrdinaryDivergence pins the custody scope of
// the abort help. inspect_and_reconcile_manually is not custody-specific -
// classifyRelation emits it for ordinary divergence with a `git log` command -
// so a bare abort on a branch no run holds must stay silent rather than answer
// with unrelated reconciliation advice.
func TestBareAbortNoOpEmitsNoHelpForOrdinaryDivergence(t *testing.T) {
	runID, p, local := wedgedCustodyAbortFixture(t)
	root := filepath.Dir(local)

	// A real pipeline push binding, then local work that conflicts with it, so
	// classification lands on ordinary divergence rather than pipeline custody.
	cliGit(t, local, "checkout", "-b", "pipeline-pushed")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("pipeline rewrite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "pipeline rewrite")
	pushed := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "feature/wedged")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("local rewrite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "local rewrite")

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(runID, db.PushBinding{
		HeadSHA:           pushed,
		TargetKind:        "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(filepath.Join(root, "remote.git")),
		Ref:               "refs/heads/feature/wedged",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunCustodyReturned(runID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	startNoActiveRunDaemon(t, p)

	out, err := executeCmd("axi", "abort")
	t.Logf("bare abort on a diverged, unheld branch:\n%s", out)
	if err != nil {
		t.Fatalf("bare abort with no active run must be a no-op success: %v\n%s", err, out)
	}
	if !strings.Contains(out, "aborted: false") {
		t.Errorf("bare abort no-op output missing %q:\n%s", "aborted: false", out)
	}
	if !strings.Contains(out, "safety: blocked_diverged") {
		t.Fatalf("fixture did not reach ordinary divergence:\n%s", out)
	}
	// The structured branch_sync object still REPORTS the branch's own
	// next_action; that is ownership state, not a prescription. Only the
	// abort's own help prescribes, and here it must prescribe nothing.
	if !strings.Contains(out, "code: inspect_and_reconcile_manually") {
		t.Errorf("branch_sync stopped reporting the branch's own next action:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "help[") {
			t.Errorf("abort emitted help for a branch no run holds:\n%s", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "Run `git log") {
			t.Errorf("abort prescribed unrelated divergence advice:\n%s", line)
		}
	}
}

// TestRunScopedAbortNoOpEmitsNoHelpForOrdinaryDivergence is the sibling of
// TestBareAbortNoOpEmitsNoHelpForOrdinaryDivergence for the two `--run <id>`
// abort sites. Their only guard is state.Pipeline.RunID != runID, which proves
// the branch RESOLVES to that run - not that a run still holds it - so a
// diverged, already-released branch still reached custodySettlementHelp and
// answered an abort with `git log` reconciliation advice the bare site had
// already been taught not to emit.
func TestRunScopedAbortNoOpEmitsNoHelpForOrdinaryDivergence(t *testing.T) {
	runID, _, _ := divergedReleasedBranchFixture(t)

	t.Run("daemon unavailable", func(t *testing.T) {
		out, err := executeCmd("axi", "abort", "--run", runID)
		t.Logf("daemon-down --run abort on a diverged, unheld branch:\n%s", out)
		if err != nil {
			t.Fatalf("terminal run must resolve idempotently: %v\n%s", err, out)
		}
		assertNoAbortHelpEmitted(t, out)
	})
}

// TestRunScopedAbortNoOpEmitsNoHelpForOrdinaryDivergenceWithDaemon covers the
// same guard on the daemon-up resolveInactiveAbortTruth path.
func TestRunScopedAbortNoOpEmitsNoHelpForOrdinaryDivergenceWithDaemon(t *testing.T) {
	runID, p, _ := divergedReleasedBranchFixture(t)
	startInactiveAbortDaemon(t, p, runID)

	out, err := executeCmd("axi", "abort", "--run", runID)
	t.Logf("daemon-up --run abort on a diverged, unheld branch:\n%s", out)
	if err != nil {
		t.Fatalf("terminal run must resolve idempotently: %v\n%s", err, out)
	}
	assertNoAbortHelpEmitted(t, out)
}

// divergedReleasedBranchFixture builds a branch whose run pushed successfully
// and whose custody was already returned, then diverges it locally, so
// classification lands on ordinary divergence rather than pipeline custody
// while the branch still resolves to that run.
func divergedReleasedBranchFixture(t *testing.T) (string, *paths.Paths, string) {
	t.Helper()
	runID, p, local := wedgedCustodyAbortFixture(t)
	root := filepath.Dir(local)

	cliGit(t, local, "checkout", "-b", "pipeline-pushed")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("pipeline rewrite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "pipeline rewrite")
	pushed := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "feature/wedged")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("local rewrite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "local rewrite")

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(runID, db.PushBinding{
		HeadSHA:           pushed,
		TargetKind:        "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(filepath.Join(root, "remote.git")),
		Ref:               "refs/heads/feature/wedged",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunCustodyReturned(runID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return runID, p, local
}

func assertNoAbortHelpEmitted(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "aborted: false") {
		t.Errorf("abort no-op output missing %q:\n%s", "aborted: false", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "help[") {
			t.Errorf("abort emitted help for a branch no run holds:\n%s", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "Run `git log") {
			t.Errorf("abort prescribed unrelated divergence advice:\n%s", line)
		}
	}
}
