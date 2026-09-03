package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestShutdownRefusesActiveAgentPeer(t *testing.T) {
	p, database := startTestDaemon(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run active: %v", err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	pid := os.Getpid()
	if err := database.SetStepAgentActivity(step.ID, "agent started", &pid); err != nil {
		t.Fatalf("set agent pid: %v", err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()
	err = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &ipc.ShutdownResult{})
	if err == nil || !strings.Contains(err.Error(), gatecontext.ErrorCode) {
		t.Fatalf("shutdown error = %v, want %s refusal", err, gatecontext.ErrorCode)
	}
	var health ipc.HealthResult
	if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
		t.Fatalf("daemon stopped after refused shutdown: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}
}

func TestUpdateClosingIssueRefsRefusesActiveAgentPeer(t *testing.T) {
	p, database := startTestDaemon(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run active: %v", err)
	}
	t.Cleanup(func() {
		_ = database.UpdateRunStatus(run.ID, types.RunCompleted)
	})
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	pid := os.Getpid()
	if err := database.SetStepAgentActivity(step.ID, "agent started", &pid); err != nil {
		t.Fatalf("set agent pid: %v", err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()
	err = client.Call(ipc.MethodUpdateRunClosingIssueRefs, &ipc.UpdateRunClosingIssueRefsParams{
		RunID:            run.ID,
		ClosingIssueRefs: []string{"42"},
	}, &ipc.UpdateRunClosingIssueRefsResult{})
	if err == nil || !strings.Contains(err.Error(), gatecontext.ErrorCode) {
		t.Fatalf("update error = %v, want %s refusal", err, gatecontext.ErrorCode)
	}
	updated, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(updated.ClosingIssueRefs) != 0 {
		t.Fatalf("closing issue references = %#v, want no mutation", updated.ClosingIssueRefs)
	}
}
