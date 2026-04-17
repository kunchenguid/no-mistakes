package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHealthHandler(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Errorf("health status = %q, want %q", result.Status, "ok")
	}
}

func TestShutdownHandler(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Error("shutdown result should be OK")
	}

	// Wait for socket to disappear.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("socket still exists after shutdown")
}

func TestPIDFile(t *testing.T) {
	p, _ := startTestDaemon(t)

	pid, err := ReadPID(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

func TestGetRunHandler(t *testing.T) {
	p, d := startTestDaemon(t)

	// Insert test data.
	repo, err := d.InsertRepoWithID("test-repo-123", "/tmp/test-repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Run == nil {
		t.Fatal("expected run, got nil")
	}
	if result.Run.ID != run.ID {
		t.Errorf("run id = %q, want %q", result.Run.ID, run.ID)
	}
	if result.Run.Branch != "feature" {
		t.Errorf("branch = %q, want %q", result.Run.Branch, "feature")
	}
	if len(result.Run.Steps) != 1 {
		t.Fatalf("steps count = %d, want 1", len(result.Run.Steps))
	}
	if result.Run.Steps[0].StepName != types.StepReview {
		t.Errorf("step name = %q, want %q", result.Run.Steps[0].StepName, types.StepReview)
	}
}

func TestGetRunsHandler(t *testing.T) {
	p, d := startTestDaemon(t)

	repo, err := d.InsertRepoWithID("test-repo-456", "/tmp/test-repo2", "https://github.com/test/repo2", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.InsertRun(repo.ID, "feat-a", "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.InsertRun(repo.ID, "feat-b", "ccc", "ddd")
	if err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.GetRunsResult
	if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: repo.ID}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("runs count = %d, want 2", len(result.Runs))
	}
}

func TestGetActiveRunHandler(t *testing.T) {
	p, d := startTestDaemon(t)

	repo, err := d.InsertRepoWithID("test-repo-789", "/tmp/test-repo3", "https://github.com/test/repo3", "main")
	if err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// No active run.
	var result ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Run != nil {
		t.Error("expected no active run")
	}

	// Create a pending run.
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}

	var result2 ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &result2); err != nil {
		t.Fatal(err)
	}
	if result2.Run == nil {
		t.Fatal("expected active run")
	}
	if result2.Run.ID != run.ID {
		t.Errorf("active run id = %q, want %q", result2.Run.ID, run.ID)
	}
}

func TestGetActiveRunHandlerStrictBranchMatch(t *testing.T) {
	p, d := startTestDaemon(t)

	repo, err := d.InsertRepoWithID("test-repo-branch", "/tmp/test-repo-branch", "https://github.com/test/repo-branch", "main")
	if err != nil {
		t.Fatal(err)
	}

	runA, err := d.InsertRun(repo.ID, "feature-a", "aaa", "000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, "feature-b", "bbb", "000"); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Specific branch returns only that branch's run.
	var result ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID, Branch: "feature-a"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Run == nil {
		t.Fatal("expected active run for requested branch")
	}
	if result.Run.ID != runA.ID {
		t.Fatalf("active run id = %q, want %q", result.Run.ID, runA.ID)
	}

	// Unknown branch returns nil — no fallback. The setup wizard relies on
	// this to detect that the current branch has no active run.
	var missing ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID, Branch: "missing-branch"}, &missing); err != nil {
		t.Fatal(err)
	}
	if missing.Run != nil {
		t.Fatalf("expected nil run for unmatched branch, got %q on %q", missing.Run.ID, missing.Run.Branch)
	}
}

func TestGetRunNotFound(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.GetRunResult
	err = client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: "nonexistent"}, &result)
	if err == nil {
		t.Error("expected error for nonexistent run")
	}
}

func TestGetRunIncludesFindingsJSON(t *testing.T) {
	p, d := startTestDaemon(t)

	// Insert test data with findings.
	repo, err := d.InsertRepoWithID("test-findings-repo", "/tmp/test-findings", "https://github.com/test/findings", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"issues":[{"severity":"warning","description":"potential null deref"}]}`
	if err := d.SetStepFindings(sr.ID, findings); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Run == nil {
		t.Fatal("expected run, got nil")
	}
	if len(result.Run.Steps) != 1 {
		t.Fatalf("steps count = %d, want 1", len(result.Run.Steps))
	}
	step := result.Run.Steps[0]
	if step.FindingsJSON == nil {
		t.Fatal("expected FindingsJSON to be populated, got nil")
	}
	if *step.FindingsJSON != findings {
		t.Errorf("FindingsJSON = %q, want %q", *step.FindingsJSON, findings)
	}
}
