package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// --- mock step helpers ---

// mockStep is a test step that returns a configurable outcome.
type mockStep struct {
	name    types.StepName
	outcome *StepOutcome
	err     error
	calls   int
	mu      sync.Mutex
}

func (m *mockStep) Name() types.StepName { return m.name }

func (m *mockStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("executing %s", m.name))
	}
	return m.outcome, m.err
}

func (m *mockStep) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newPassStep(name types.StepName) *mockStep {
	return &mockStep{name: name, outcome: &StepOutcome{ExitCode: 0}}
}

func newApprovalStep(name types.StepName, findings string) *mockStep {
	return &mockStep{name: name, outcome: &StepOutcome{NeedsApproval: true, Findings: findings}}
}

func newFailStep(name types.StepName, err error) *mockStep {
	return &mockStep{name: name, err: err}
}

// --- test helpers ---

func setupTest(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo) {
	t.Helper()
	dir := t.TempDir()
	p := paths.WithRoot(dir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepoWithID("testrepo", "/tmp/test-repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	return database, p, run, repo
}

// eventCollector is a thread-safe event accumulator for tests.
type eventCollector struct {
	mu     sync.Mutex
	events []ipc.Event
}

func (ec *eventCollector) handler(e ipc.Event) {
	ec.mu.Lock()
	ec.events = append(ec.events, e)
	ec.mu.Unlock()
}

func (ec *eventCollector) all() []ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	out := make([]ipc.Event, len(ec.events))
	copy(out, ec.events)
	return out
}

func (ec *eventCollector) find(eventType ipc.EventType, stepName types.StepName) *ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, e := range ec.events {
		if e.Type == eventType && e.StepName != nil && *e.StepName == stepName {
			cp := e
			return &cp
		}
	}
	return nil
}

func (ec *eventCollector) findLast(eventType ipc.EventType, status string) *ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for i := len(ec.events) - 1; i >= 0; i-- {
		e := ec.events[i]
		if e.Type == eventType && e.Status != nil && *e.Status == status {
			cp := e
			return &cp
		}
	}
	return nil
}

func (ec *eventCollector) findRunEvent(eventType ipc.EventType) *ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, e := range ec.events {
		if e.Type == eventType && e.StepName == nil {
			cp := e
			return &cp
		}
	}
	return nil
}

func collectEvents(exec *Executor) *eventCollector {
	ec := &eventCollector{}
	exec.onEvent = ec.handler
	return ec
}

// --- helper types ---

// adaptiveCallStep allows custom Execute logic via a function.
type adaptiveCallStep struct {
	name types.StepName
	fn   func(sctx *StepContext) (*StepOutcome, error)
}

func (a *adaptiveCallStep) Name() types.StepName { return a.name }
func (a *adaptiveCallStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	return a.fn(sctx)
}

// waitForStepEvent polls the event collector until an event with the given type and step name appears.
func waitForStepEvent(t *testing.T, ec *eventCollector, eventType ipc.EventType, stepName types.StepName) *ipc.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e := ec.find(eventType, stepName); e != nil {
			return e
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s for step %s not found within timeout", eventType, stepName)
	return nil
}

// waitForEvent polls the event collector until an event with the given type and status appears.
func waitForEvent(t *testing.T, ec *eventCollector, eventType ipc.EventType, status string) *ipc.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e := ec.findLast(eventType, status); e != nil {
			return e
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s with status %q not found within timeout", eventType, status)
	return nil
}

// waitForStepStatus polls the DB until a step reaches the expected status.
func waitForStepStatus(t *testing.T, database *db.DB, runID string, stepName types.StepName, expected types.StepStatus) {
	t.Helper()
	// The executor and this poller share one SQLite connection (MaxOpenConns(1)).
	// A 10ms loop starved writers on Windows CI: auto-fix rounds never reached
	// fix_review before the deadline, then TempDir cleanup failed because the
	// still-running executor held lint.log open. Sleep long enough that a write
	// can land between polls, and keep the deadline above a multi-round
	// transition under filesystem contention.
	deadline := time.Now().Add(30 * time.Second)
	var last []string
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err == nil {
			last = last[:0]
			for _, s := range steps {
				last = append(last, fmt.Sprintf("%s=%s", s.StepName, s.Status))
				if s.StepName == stepName && s.Status == expected {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("step %s did not reach status %q within timeout; last seen %v", stepName, expected, last)
}

// startExecutor runs Execute in a goroutine and cancels it during cleanup so a
// parked step closes its log file before t.TempDir removes the tree. Windows
// refuses unlinkat on a still-open handle; leaving Execute running after a
// failed wait was the lint.log leak in TestExecutor_AutoFixRespectsMaxAttempts.
func startExecutor(t *testing.T, exec *Executor, run *db.Run, repo *db.Repo, workDir string) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var finished atomic.Bool
	t.Cleanup(func() {
		cancel()
		if finished.Load() {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("executor did not return after cancel")
		}
	})
	go func() {
		err := exec.Execute(ctx, run, repo, workDir)
		finished.Store(true)
		done <- err
	}()
	return done, cancel
}

func waitExecutorDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("executor timed out")
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

type findingJSON struct {
	ID               string `json:"id"`
	Severity         string `json:"severity"`
	Description      string `json:"description"`
	Source           string `json:"source"`
	UserInstructions string `json:"user_instructions"`
}

func mustParseFindingItems(t *testing.T, raw string) []findingJSON {
	t.Helper()
	var payload struct {
		Findings []findingJSON `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("parse findings JSON: %v", err)
	}
	return payload.Findings
}

// initGitRepo creates a git repo with an initial commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@test.com")
	execGit(t, dir, "config", "user.name", "Test")
	writeTestFile(t, dir, "README.md", "# test\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "initial")
}

func execGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
