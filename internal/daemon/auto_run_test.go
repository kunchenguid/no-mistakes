package daemon

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHandlePushReceived_DefaultAutoRun(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, headSHA := setupTestGitRepo(t, p, d, "default-autorun-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/new-feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatalf("unexpected error on default auto_run: %v", err)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty RunID for default auto_run")
	}
}

func TestHandlePushReceived_OptOutScopesToLedger(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	// Opt out of mirror-triggered auto-run
	existingConfig, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(existingConfig, []byte("\nauto_run: false\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, headSHA := setupTestGitRepo(t, p, d, "optout-autorun-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// 1. Push for a branch with no prior runs in the ledger is refused.
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/unlaunched-feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err == nil {
		t.Fatal("expected error for unlaunched branch when auto_run is false, got nil")
	}
	if !strings.Contains(err.Error(), "auto_run disabled") || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("unexpected error message: %v", err)
	}

	has, err := d.HasRunsForBranch(repo.ID, "unlaunched-feature")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("expected no runs created in ledger for unlaunched branch")
	}

	// 2. Once a run exists in the home's ledger (e.g. explicitly launched), a push continues it.
	initialRun, err := d.InsertRun(repo.ID, "unlaunched-feature", headSHA, "0000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if initialRun == nil {
		t.Fatal("expected run created")
	}

	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/unlaunched-feature",
		Old:  headSHA,
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatalf("unexpected error for launched branch with auto_run: false: %v", err)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty RunID for launched branch")
	}
}

func TestHandlePushReceived_MirrorAutoRunOptOut(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	// Configure mirror.auto_run: false
	existingConfig, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(existingConfig, []byte("\nmirror:\n  auto_run: false\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, headSHA := setupTestGitRepo(t, p, d, "mirror-optout-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/unlaunched-mirror-branch",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err == nil {
		t.Fatal("expected error for unlaunched branch with mirror.auto_run: false, got nil")
	}
	if !strings.Contains(err.Error(), "auto_run disabled") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRunManager_HandlePushReceived_Direct(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	repo, headSHA := setupTestGitRepo(t, p, d, "direct-handle-repo")

	mgr := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	ctx := context.Background()

	// 1. Default: unlaunched branch succeeds
	runID, err := mgr.HandlePushReceived(ctx, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/direct-branch-1",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	})
	if err != nil {
		t.Fatalf("default HandlePushReceived error: %v", err)
	}
	if runID == "" {
		t.Fatal("expected non-empty runID")
	}

	// 2. Set auto_run: false
	cfgData, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(cfgData, []byte("\nauto_run: false\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unlaunched branch fails
	_, err = mgr.HandlePushReceived(ctx, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/direct-branch-2",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	})
	if err == nil {
		t.Fatal("expected error for unlaunched branch with auto_run: false")
	}
	if !strings.Contains(err.Error(), "auto_run disabled") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Pre-seeded branch succeeds
	if _, err := d.InsertRun(repo.ID, "direct-branch-2", headSHA, "0000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	runID2, err := mgr.HandlePushReceived(ctx, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/direct-branch-2",
		Old:  headSHA,
		New:  headSHA,
	})
	if err != nil {
		t.Fatalf("expected launched branch to succeed: %v", err)
	}
	if runID2 == "" {
		t.Fatal("expected non-empty runID")
	}
}
