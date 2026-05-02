package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
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
