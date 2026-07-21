package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTelemetryBranchRoleDistinguishesDefaultAndPipelineBase(t *testing.T) {
	for _, tc := range []struct {
		branch string
		want   string
	}{
		{branch: "main", want: "default"},
		{branch: "staging", want: "base"},
		{branch: "feature/x", want: "feature"},
	} {
		if got := telemetryBranchRole(tc.branch, "main", "staging"); got != tc.want {
			t.Errorf("telemetryBranchRole(%q) = %q, want %q", tc.branch, got, tc.want)
		}
	}
}

func TestProtectedSourceBranchRejectsRemoteDefaultAndPipelineBase(t *testing.T) {
	repo := &db.Repo{DefaultBranch: "main", BaseBranch: "staging"}
	for _, tc := range []struct {
		branch string
		want   bool
	}{
		{branch: "main", want: true},
		{branch: "staging", want: true},
		{branch: "feature/x", want: false},
	} {
		if got := protectedSourceBranch(repo, tc.branch); got != tc.want {
			t.Errorf("protectedSourceBranch(%q) = %v, want %v", tc.branch, got, tc.want)
		}
	}
}

func TestPushReceivedRefusesProtectedSourcesBeforeRunCreation(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, nil)
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-base-source")
	if _, err := database.UpdateRepoMetadataAndBase(repo.ID, repo.UpstreamURL, "main", "staging"); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, branch := range []string{"main", "staging"} {
		var result ipc.PushReceivedResult
		err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir(repo.ID),
			Ref:  "refs/heads/" + branch,
			Old:  "0000000000000000000000000000000000000000",
			New:  headSHA,
		}, &result)
		if err == nil {
			t.Fatalf("push to protected source %q unexpectedly created run %q", branch, result.RunID)
		}
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("protected source pushes created %d run(s)", len(runs))
	}
}

type baseCaptureStep struct {
	seen chan string
}

func (s *baseCaptureStep) Name() types.StepName { return types.StepReview }
func (s *baseCaptureStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.seen <- sctx.BaseBranch() + "|" + sctx.Config.Commands.Test
	return &pipeline.StepOutcome{}, nil
}

func TestPushReceivedSnapshotsConfiguredBaseAndLoadsItsTrustedConfig(t *testing.T) {
	step := &baseCaptureStep{seen: make(chan string, 1)}
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	repo, headSHA := setupTestGitRepo(t, p, database, "configured-base-run")

	configPath := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("base_branch: staging\ncommands:\n  test: echo staging-trusted\n")...)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "staging")
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure staging")
	gitCmd(t, repo.WorkingPath, "push", "gate", "staging")
	gitCmd(t, repo.WorkingPath, "checkout", "main")

	if _, err := database.UpdateRepoMetadataAndBase(repo.ID, repo.UpstreamURL, "main", "staging"); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/feature/test",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, database, result.RunID)
	run, err := database.GetRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.BaseBranch != "staging" {
		t.Fatalf("run base snapshot = %q, want staging", run.BaseBranch)
	}
	select {
	case got := <-step.seen:
		if got != "staging|echo staging-trusted" {
			t.Fatalf("captured base and trusted test command = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("capture step did not run; terminal status=%s error=%v", run.Status, run.Error)
	}
}
