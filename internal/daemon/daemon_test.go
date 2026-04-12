package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// startTestDaemon starts RunWithResources in a goroutine with a temp root.
// Returns paths, db, and a cleanup function that stops the daemon.
func startTestDaemon(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()

	// Use short temp dir to avoid macOS 104-byte Unix socket path limit.
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithResources(p, d)
	}()

	// Wait for socket to appear.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		// Ensure daemon stops.
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})

	return p, d
}

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

func TestIsRunningFalseWhenNoSocket(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	alive, err := IsRunning(p)
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Error("expected not running when no socket exists")
	}
}

func TestIsRunningTrueWhenDaemonRunning(t *testing.T) {
	p, _ := startTestDaemon(t)

	alive, err := IsRunning(p)
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Error("expected running when daemon is started")
	}
}

func TestStopNotRunningIsNoop(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	if err := Stop(p); err != nil {
		t.Fatalf("stop should succeed when daemon is not running: %v", err)
	}
}

func TestReadPIDNoFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	_, err = ReadPID(p)
	if err == nil {
		t.Error("expected error when no PID file")
	}
}

func TestReadPIDInvalid(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	os.WriteFile(filepath.Join(tmpDir, "daemon.pid"), []byte("notanumber"), 0o644)
	_, err = ReadPID(p)
	if err == nil {
		t.Error("expected error for invalid PID content")
	}
}

// --- Mock steps and helpers for RunManager tests ---

// mockPassStep is a step that completes immediately without needing approval.
type mockPassStep struct {
	name    types.StepName
	execCnt atomic.Int32
}

func (s *mockPassStep) Name() types.StepName { return s.name }
func (s *mockPassStep) Execute(_ *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.execCnt.Add(1)
	return &pipeline.StepOutcome{}, nil
}

// mockApprovalStep pauses for approval every time.
type mockApprovalStep struct {
	name types.StepName
}

func (s *mockApprovalStep) Name() types.StepName { return s.name }
func (s *mockApprovalStep) Execute(_ *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"needs review"}`}, nil
}

// mockSlowStep blocks until context is cancelled.
type mockSlowStep struct {
	name    types.StepName
	started chan struct{}
}

func (s *mockSlowStep) Name() types.StepName { return s.name }
func (s *mockSlowStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.started != nil {
		close(s.started)
	}
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

type mockVerifyDefaultBranchStep struct {
	name types.StepName
}

func (s *mockVerifyDefaultBranchStep) Name() types.StepName { return s.name }

func (s *mockVerifyDefaultBranchStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "HEAD", "origin/"+sctx.Repo.DefaultBranch); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

// startTestDaemonWithSteps starts a daemon with a custom step factory.
func startTestDaemonWithSteps(t *testing.T, sf StepFactory) (*paths.Paths, *db.DB) {
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

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, sf)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})

	return p, d
}

// setupTestGitRepo creates a git repo with one commit, pushes to a bare repo
// under p.RepoDir(repoID), and registers the repo in the DB.
// Returns the repo record and the head SHA.
func setupTestGitRepo(t *testing.T, p *paths.Paths, d *db.DB, repoID string) (*db.Repo, string) {
	t.Helper()
	ctx := context.Background()

	// Create a work repo with an initial commit.
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "initial")

	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	// Create bare repo at the expected gate path.
	bareDir := p.RepoDir(repoID)
	gitCmd(t, "", "init", "--bare", bareDir)

	// Push from work to bare so it has refs.
	gitCmd(t, workDir, "remote", "add", "gate", bareDir)
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")

	// Register repo in DB.
	repo, err := d.InsertRepoWithID(repoID, workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_ = ctx

	return repo, headSHA
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(out[:len(out)-1]) // trim trailing newline
}

func writeMockClaude(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "claude.bat")
		script := "@echo off\r\necho {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"findings\":[],\"summary\":\"clean\"}}\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"summary":"clean"}}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPushReceivedWithRealStepsPushesToUpstream(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{
			&steps.ReviewStep{},
			&steps.TestStep{},
			&steps.LintStep{},
			&steps.PushStep{},
		}
	})

	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, "", "init", "--bare", upstreamDir)

	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	gitCmd(t, workDir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("commands:\n  test: true\n  lint: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "initial")
	baseSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "remote", "add", "origin", upstreamDir)
	gitCmd(t, workDir, "push", "origin", "main")

	gateDir := p.RepoDir("testrepo-real")
	gitCmd(t, "", "init", "--bare", gateDir)
	gitCmd(t, gateDir, "remote", "add", "origin", upstreamDir)
	gitCmd(t, workDir, "remote", "add", "gate", gateDir)
	gitCmd(t, workDir, "push", "gate", "main")

	gitCmd(t, workDir, "checkout", "-b", "feature/e2e")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "README.md")
	gitCmd(t, workDir, "commit", "-m", "feature change")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "gate", "feature/e2e")

	repo, err := d.InsertRepoWithID("testrepo-real", workDir, upstreamDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if repo == nil {
		t.Fatal("expected repo record")
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: gateDir,
		Ref:  "refs/heads/feature/e2e",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCompleted {
			goto completed
		}
		if run.Status == types.RunFailed {
			var runErr string
			if run.Error != nil {
				runErr = *run.Error
			}
			t.Fatalf("run failed: %s", runErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("run did not complete within timeout")

completed:
	upstreamSHA := gitOutput(t, upstreamDir, "rev-parse", "refs/heads/feature/e2e")
	if upstreamSHA != headSHA {
		t.Fatalf("upstream feature SHA = %q, want %q (base %q)", upstreamSHA, headSHA, baseSHA)
	}
}

// --- RunManager unit tests ---

func TestRepoIDFromGatePath(t *testing.T) {
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"/home/user/.no-mistakes/repos/abc123.git", "abc123", false},
		{"/tmp/repos/test-id.git", "test-id", false},
		{"/tmp/repos/nope", "", true},
	}
	for _, tc := range tests {
		got, err := repoIDFromGatePath(tc.path)
		if (err != nil) != tc.wantErr {
			t.Errorf("repoIDFromGatePath(%q): err=%v, wantErr=%v", tc.path, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("repoIDFromGatePath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestBranchFromRef(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"refs/heads/main", "main"},
		{"refs/heads/feature/foo", "feature/foo"},
		{"main", "main"},
	}
	for _, tc := range tests {
		got := branchFromRef(tc.ref)
		if got != tc.want {
			t.Errorf("branchFromRef(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestIsZeroSHA(t *testing.T) {
	tests := []struct {
		sha  string
		want bool
	}{
		{"0000000000000000000000000000000000000000", true},
		{"abc123def456789012345678901234567890abcd", false},
		{"", false},
		{"000000", false},
	}
	for _, tc := range tests {
		got := git.IsZeroSHA(tc.sha)
		if got != tc.want {
			t.Errorf("IsZeroSHA(%q) = %v, want %v", tc.sha, got, tc.want)
		}
	}
}

// --- RunManager integration tests ---

func TestPushReceivedCreatesRun(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, headSHA := setupTestGitRepo(t, p, d, "testrepo1")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Send push_received.
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo1"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}

	// Wait for pipeline to complete (mock step is instant).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCompleted || run.Status == types.RunFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run was created with correct fields.
	run, err := d.GetRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("run not found")
	}
	if run.RepoID != repo.ID {
		t.Errorf("run repo_id = %q, want %q", run.RepoID, repo.ID)
	}
	if run.Branch != "main" {
		t.Errorf("run branch = %q, want %q", run.Branch, "main")
	}
	if run.HeadSHA != headSHA {
		t.Errorf("run head_sha = %q, want %q", run.HeadSHA, headSHA)
	}
	if run.Status != types.RunCompleted {
		t.Errorf("run status = %q, want %q", run.Status, types.RunCompleted)
	}

	// Verify step was executed.
	if step.execCnt.Load() == 0 {
		t.Error("mock step was never executed")
	}
}

func TestPushReceivedFetchesDefaultBranchIntoWorktree(t *testing.T) {
	step := &mockVerifyDefaultBranchStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, "", "init", "--bare", upstreamDir)

	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	gitCmd(t, workDir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "initial")
	gitCmd(t, workDir, "remote", "add", "origin", upstreamDir)
	gitCmd(t, workDir, "push", "origin", "main")

	gateDir := p.RepoDir("testrepo-origin-main")
	gitCmd(t, "", "init", "--bare", gateDir)
	gitCmd(t, gateDir, "remote", "add", "origin", upstreamDir)
	gitCmd(t, workDir, "remote", "add", "gate", gateDir)

	gitCmd(t, workDir, "checkout", "-b", "feature/scope")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "README.md")
	gitCmd(t, workDir, "commit", "-m", "feature change")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "gate", "feature/scope")

	if _, err := d.InsertRepoWithID("testrepo-origin-main", workDir, upstreamDir, "main"); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: gateDir,
		Ref:  "refs/heads/feature/scope",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	waitForRunTerminalState(t, d, result.RunID)
	run, err := d.GetRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want completed (error: %s)", run.Status, runErr)
	}
}

func TestPushReceivedUnknownRepo(t *testing.T) {
	p, _ := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("nonexistent"),
		Ref:  "refs/heads/main",
		Old:  "aaa",
		New:  "bbb",
	}, &result)
	if err == nil {
		t.Error("expected error for unknown repo")
	}
}

func TestPushReceivedCancelsActiveRun(t *testing.T) {
	started := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: started}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo2")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// First push — starts a slow pipeline.
	var result1 ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result1)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the slow step to start executing.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	// Second push — should cancel first run.
	// Need a new started channel for the second run's step.
	started2 := make(chan struct{})
	slowStep.started = started2

	var result2 ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result2)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for first run to be marked as failed/cancelled.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run1, err := d.GetRun(result1.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run1.Status == types.RunCancelled {
			if run1.Error == nil || !strings.Contains(*run1.Error, "superseded by new push") {
				var got string
				if run1.Error != nil {
					got = *run1.Error
				}
				t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
			}
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("first run was not cancelled within timeout")
}

func TestCancelRunStopsActivePipeline(t *testing.T) {
	started := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: started}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-cancel")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-cancel"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	var cancelResult ipc.CancelRunResult
	err = client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: pushResult.RunID}, &cancelResult)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelResult.OK {
		t.Fatal("cancel run should return OK")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(pushResult.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCancelled {
			if run.Error == nil || !strings.Contains(*run.Error, "aborted by user") {
				var got string
				if run.Error != nil {
					got = *run.Error
				}
				t.Fatalf("expected cancelled run error to mention aborted by user, got %q", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run was not cancelled within timeout")
}

func TestPushReceivedDoesNotCancelActiveRunOnDifferentBranch(t *testing.T) {
	startedMain := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: startedMain}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-different-branch")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var mainPush ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-different-branch"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &mainPush)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-startedMain:
	case <-time.After(5 * time.Second):
		t.Fatal("main branch run never started")
	}

	startedFeature := make(chan struct{})
	slowStep.started = startedFeature

	var featurePush ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-different-branch"),
		Ref:  "refs/heads/feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &featurePush)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-startedFeature:
	case <-time.After(5 * time.Second):
		t.Fatal("feature branch run never started")
	}

	time.Sleep(200 * time.Millisecond)

	mainRun, err := d.GetRun(mainPush.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if mainRun.Status == types.RunFailed || mainRun.Status == types.RunCancelled {
		if mainRun.Error != nil && strings.Contains(*mainRun.Error, "superseded by new push") {
			t.Fatalf("main branch run should not be superseded by a push to a different branch: %q", *mainRun.Error)
		}
		t.Fatalf("main branch run should still be active, got status %s", mainRun.Status)
	}
}

func TestRerunHandler(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, headSHA := setupTestGitRepo(t, p, d, "testrepo-rerun")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)

	var rerun ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &rerun)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.RunID == "" {
		t.Fatal("expected non-empty rerun ID")
	}
	if rerun.RunID == first.RunID {
		t.Fatal("expected rerun to create a new run")
	}

	waitForRunTerminalState(t, d, rerun.RunID)

	run, err := d.GetRun(rerun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("rerun not found")
	}
	if run.Branch != "main" {
		t.Errorf("run branch = %q, want %q", run.Branch, "main")
	}
	if run.HeadSHA != headSHA {
		t.Errorf("run head_sha = %q, want %q", run.HeadSHA, headSHA)
	}
	if run.BaseSHA != "0000000000000000000000000000000000000000" {
		t.Errorf("run base_sha = %q, want zero sha", run.BaseSHA)
	}
	if step.execCnt.Load() < 2 {
		t.Fatalf("expected step to execute twice, got %d", step.execCnt.Load())
	}
}

func TestRerunHandlerNoPreviousRun(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	repo, _ := setupTestGitRepo(t, p, d, "testrepo-rerun-missing")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var rerun ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &rerun)
	if err == nil {
		t.Fatal("expected error when rerunning without a previous run")
	}
	if !strings.Contains(err.Error(), "no previous run") {
		t.Fatalf("expected no previous run error, got %v", err)
	}
}

func TestRespondToActiveRun(t *testing.T) {
	approvalStep := &mockApprovalStep{name: types.StepReview}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{approvalStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo3")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Start a pipeline that will pause for approval.
	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo3"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for step to reach awaiting_approval status.
	deadline := time.Now().Add(5 * time.Second)
	awaitingApproval := false
	for time.Now().Before(deadline) {
		steps, err := d.GetStepsByRun(pushResult.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if s.Status == types.StepStatusAwaitingApproval {
				awaitingApproval = true
				break
			}
		}
		if awaitingApproval {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !awaitingApproval {
		t.Fatal("step never reached awaiting_approval status")
	}

	// Send approve action.
	var respondResult ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID:  pushResult.RunID,
		Step:   types.StepReview,
		Action: types.ActionApprove,
	}, &respondResult)
	if err != nil {
		t.Fatal(err)
	}
	if !respondResult.OK {
		t.Error("respond should return OK")
	}

	// Wait for run to complete.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(pushResult.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCompleted {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("run did not complete after approval")
}

func TestSubscribeReceivesEvents(t *testing.T) {
	approvalStep := &mockApprovalStep{name: types.StepReview}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{approvalStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-sub1")

	// Trigger a push to get a run ID.
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub1"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for step to reach awaiting_approval.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		steps, _ := d.GetStepsByRun(pushResult.RunID)
		for _, s := range steps {
			if s.Status == types.StepStatusAwaitingApproval {
				goto subscribeNow
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("step never reached awaiting_approval")

subscribeNow:
	// Subscribe to events for this run.
	ch, cancelSub, err := ipc.Subscribe(p.Socket(), &ipc.SubscribeParams{RunID: pushResult.RunID})
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSub()

	// Approve the step to trigger completion events.
	var respondResult ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID:  pushResult.RunID,
		Step:   types.StepReview,
		Action: types.ActionApprove,
	}, &respondResult)
	if err != nil {
		t.Fatal(err)
	}

	// Collect events until channel closes.
	var events []ipc.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				goto verifyEvents
			}
			events = append(events, event)
		case <-timeout:
			t.Fatal("subscriber channel never closed")
		}
	}

verifyEvents:
	if len(events) == 0 {
		t.Fatal("received no events")
	}
	hasRunCompleted := false
	for _, e := range events {
		if e.Type == ipc.EventRunCompleted {
			hasRunCompleted = true
		}
		if e.RunID != pushResult.RunID {
			t.Errorf("event run_id=%q, want %q", e.RunID, pushResult.RunID)
		}
	}
	if !hasRunCompleted {
		t.Error("never received run_completed event")
	}
}

func TestSubscribeToSlowRunReceivesEvents(t *testing.T) {
	started := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: started}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-sub2")

	// Trigger a push first.
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the slow step to start.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	// Subscribe to the running run.
	ch, cancelSub, err := ipc.Subscribe(p.Socket(), &ipc.SubscribeParams{RunID: pushResult.RunID})
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSub()

	// Cancel the run (by sending another push, which cancels active runs).
	started2 := make(chan struct{})
	slowStep.started = started2

	var pushResult2 ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult2)
	if err != nil {
		t.Fatal(err)
	}

	// The subscriber channel should close when the first run ends.
	eventCount := 0
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto done // channel closed
			}
			eventCount++
		case <-timeout:
			t.Fatal("subscriber channel never closed")
		}
	}
done:
	// We should have received at least the run events before channel closed.
	// The exact count depends on timing, but the channel MUST close.
}

func TestSubscribeToCompletedRunReturnsClosedChannel(t *testing.T) {
	// Use a fast step so the run completes quickly.
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepTest}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-sub-done")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub-done"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the run to complete by polling get_run.
	deadline := time.After(10 * time.Second)
	for {
		var result ipc.GetRunResult
		if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: pushResult.RunID}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Run != nil && (result.Run.Status == types.RunCompleted || result.Run.Status == types.RunFailed || result.Run.Status == types.RunCancelled) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("run did not complete in time")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Subscribe to the already-completed run. The channel should be immediately closed.
	ch, cancelSub, err := ipc.Subscribe(p.Socket(), &ipc.SubscribeParams{RunID: pushResult.RunID})
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed for completed run, but received an event")
		}
		// Channel closed - expected
	case <-time.After(5 * time.Second):
		t.Fatal("channel was not closed for completed run")
	}
}

func TestRecoverStaleRunsOnStartup(t *testing.T) {
	// Set up a DB with stale runs BEFORE starting the daemon.
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}

	// Create a "running" run with in-progress steps (simulating a crash).
	repo, err := d.InsertRepoWithID("stale-repo", "/tmp/stale-repo", "https://github.com/test/stale", "main")
	if err != nil {
		t.Fatal(err)
	}
	staleRun, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	d.UpdateRunStatus(staleRun.ID, types.RunRunning)
	staleStep, err := d.InsertStepResult(staleRun.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	d.StartStep(staleStep.ID)

	d.Close()

	// Start daemon — it should recover the stale run.
	d, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockPassStep{name: types.StepReview}}
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})

	// Verify the stale run was marked as failed.
	run, err := d.GetRun(staleRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed {
		t.Errorf("stale run status = %q, want %q", run.Status, types.RunFailed)
	}
	if run.Error == nil || *run.Error != "daemon crashed during execution" {
		t.Errorf("stale run error = %v, want %q", run.Error, "daemon crashed during execution")
	}

	// Verify the stale step was marked as failed.
	step, err := d.GetStepResult(staleStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Status != types.StepStatusFailed {
		t.Errorf("stale step status = %q, want %q", step.Status, types.StepStatusFailed)
	}
}

func TestRecoverCleansUpOrphanedWorktrees(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Create orphaned worktree directories.
	orphanDir := p.WorktreeDir("some-repo", "some-run")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(orphanDir, "test.txt"), []byte("orphan"), 0o644)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockPassStep{name: types.StepReview}}
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})

	// Orphaned worktree directory should be removed.
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphaned worktree dir still exists: %s", orphanDir)
	}
}

func TestPushReceivedCleansUpWorktreeOnConfigFailure(t *testing.T) {
	// Set up a standalone RunManager (no daemon) to test worktree cleanup
	// when config loading fails after worktree creation.
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	// Set up a real git repo and bare repo.
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "initial")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	repoID := "wt-cleanup-repo"
	bareDir := p.RepoDir(repoID)
	gitCmd(t, "", "init", "--bare", bareDir)
	gitCmd(t, workDir, "remote", "add", "gate", bareDir)
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")

	_, err = d.InsertRepoWithID(repoID, workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	// Write an invalid config.yaml to cause LoadGlobal to fail.
	if err := os.WriteFile(p.ConfigFile(), []byte("invalid: yaml: [[["), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	// HandlePushReceived should fail due to invalid config, but clean up the worktree.
	_, err = mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	})
	if err == nil {
		t.Fatal("expected error from invalid config")
	}

	// Verify worktree directory was cleaned up.
	wtRoot := filepath.Join(p.WorktreesDir(), repoID)
	entries, err := os.ReadDir(wtRoot)
	if err == nil && len(entries) > 0 {
		t.Errorf("worktree directory not cleaned up, found %d entries in %s", len(entries), wtRoot)
	}
}

func TestPushReceivedRefDeletion(t *testing.T) {
	// When a branch is deleted (git push no-mistakes :branch), the post-receive
	// hook sends newrev as all-zeros. HandlePushReceived should reject gracefully
	// without creating a run or worktree.
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	_, _ = setupTestGitRepo(t, p, d, "refdelete-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("refdelete-repo"),
		Ref:  "refs/heads/feature",
		Old:  "abc123",
		New:  "0000000000000000000000000000000000000000",
	}, &result)
	if err == nil {
		t.Fatal("expected error for ref deletion push")
	}
	if !strings.Contains(err.Error(), "ref deletion") {
		t.Errorf("error should mention ref deletion, got: %s", err.Error())
	}

	// Verify no run was created.
	runs, err := d.GetRunsByRepo("refdelete-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("expected 0 runs after ref deletion, got %d", len(runs))
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

func TestRespondNoActiveExecutor(t *testing.T) {
	p, _ := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID:  "nonexistent",
		Step:   types.StepReview,
		Action: types.ActionApprove,
	}, &result)
	if err == nil {
		t.Error("expected error when no active executor for run")
	}
}

func waitForRunTerminalState(t *testing.T, d *db.DB, runID string) *db.Run {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && (run.Status == types.RunCompleted || run.Status == types.RunFailed) {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach terminal state", runID)
	return nil
}
