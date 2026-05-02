package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMain(m *testing.M) {
	switch os.Getenv("NM_DAEMON_HELPER_PROCESS") {
	case "1":
		if capturePath := os.Getenv("NM_CAPTURE_NM_HOME_FILE"); capturePath != "" {
			_ = os.WriteFile(capturePath, []byte(os.Getenv("NM_HOME")), 0o644)
		}
		os.Exit(0)
	case "block":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	// The post-receive hook embeds os.Executable() as NM_BIN. In tests that
	// path is the test binary itself, so every `git push` to a bare gate
	// would re-enter TestMain and run the whole daemon test suite inside
	// the hook. Short-circuit: when we're being invoked as the hook's
	// notifier, exit 0 immediately. Tests that care about the daemon seeing
	// the push call push_received via IPC directly.
	if os.Getenv("NM_HOOK_HELPER") == "1" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

type mockPanicStep struct {
	name types.StepName
}

func (s *mockPanicStep) Name() types.StepName { return s.name }
func (s *mockPanicStep) Execute(_ *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	panic("boom")
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

	// Keep daemon tests hermetic now that the default config auto-detects agents.
	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
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
	// Disable auto-fix so approval-based tests pause immediately.
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("auto_fix:\n  lint: 0\n  test: 0\n  review: 0\n"), 0o644); err != nil {
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
