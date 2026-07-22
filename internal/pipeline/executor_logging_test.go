package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_LogCallback(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var logMessages []string
	var mu sync.Mutex

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.Log != nil {
				sctx.Log("hello from review")
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	onEvent := func(e ipc.Event) {
		if e.Type == ipc.EventLogChunk && e.Content != nil {
			mu.Lock()
			logMessages = append(logMessages, *e.Content)
			mu.Unlock()
		}
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, onEvent)
	exec.Execute(context.Background(), run, repo, workDir)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, msg := range logMessages {
		if strings.TrimSpace(msg) == "hello from review" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected log message 'hello from review' in events, got: %v", logMessages)
	}
}

func TestExecutor_LogCallbackTouchesStepActivity(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("heartbeat marker")
			got, err := database.GetStepResult(sctx.StepResultID)
			if err != nil {
				t.Fatalf("get step result: %v", err)
			}
			if got.LastActivity == nil || !strings.Contains(*got.LastActivity, "heartbeat marker") {
				t.Fatalf("last_activity = %v, want heartbeat marker", got.LastActivity)
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestExecutor_LogChunkThrottlesStepActivityWrites(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	counterDB, err := sql.Open("sqlite", p.DB()+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open counter db: %v", err)
	}
	defer counterDB.Close()
	if _, err := counterDB.Exec(`
		CREATE TABLE step_activity_update_count (n INTEGER NOT NULL);
		INSERT INTO step_activity_update_count (n) VALUES (0);
		CREATE TRIGGER count_step_activity_update AFTER UPDATE OF last_activity_at, last_activity ON step_results
		BEGIN
			UPDATE step_activity_update_count SET n = n + 1;
		END;
	`); err != nil {
		t.Fatalf("install activity counter: %v", err)
	}

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			for i := 0; i < 100; i++ {
				sctx.LogChunk(fmt.Sprintf("delta-%03d ", i))
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var updates int
	if err := counterDB.QueryRow(`SELECT n FROM step_activity_update_count`).Scan(&updates); err != nil {
		t.Fatalf("read activity update count: %v", err)
	}
	if updates > 5 {
		t.Fatalf("step activity updates = %d, want throttled count <= 5", updates)
	}
}

func TestStepActivityFromLogUsesBoundedAllocationForLargeLogs(t *testing.T) {
	lastLine := strings.Repeat("x", maxStepActivityText+100)
	largeLog := strings.Repeat("noise line\n", 8192) + lastLine + "\n\n"
	want := "log: " + strings.Repeat("x", maxStepActivityText) + "..."

	if got := stepActivityFromLog(largeLog); got != want {
		t.Fatalf("stepActivityFromLog() = %q, want %q", got, want)
	}

	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const iterations = 25
	for i := 0; i < iterations; i++ {
		if got := stepActivityFromLog(largeLog); got != want {
			t.Fatalf("stepActivityFromLog() = %q, want %q", got, want)
		}
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	allocatedPerCall := (after.TotalAlloc - before.TotalAlloc) / iterations
	if allocatedPerCall > 4*1024 {
		t.Fatalf("stepActivityFromLog allocated %d bytes per call, want <= 4096", allocatedPerCall)
	}
}

type lifecycleTestAgent struct{}

func (lifecycleTestAgent) Name() string { return "codex" }

func (lifecycleTestAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if opts.OnLifecycle != nil {
		opts.OnLifecycle(agent.LifecycleEvent{Agent: "codex", Phase: "start", PID: 4242, Message: "codex started pid=4242"})
		opts.OnLifecycle(agent.LifecycleEvent{Agent: "codex", Phase: "exit", PID: 4242, Message: "codex exited pid=4242 status=success"})
	}
	return &agent.Result{Text: "ok"}, nil
}

func (lifecycleTestAgent) Close() error { return nil }

func TestExecutor_AgentLifecycleLoggedAndClearsPID(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "work", CWD: sctx.WorkDir}); err != nil {
				t.Fatalf("agent run: %v", err)
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, lifecycleTestAgent{}, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	logPath := filepath.Join(p.RunLogDir(run.ID), "review.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)
	for _, want := range []string{"codex started pid=4242", "codex exited pid=4242 status=success"} {
		if !strings.Contains(content, want) {
			t.Fatalf("log missing %q in:\n%s", want, content)
		}
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if steps[0].AgentPID != nil {
		t.Fatalf("agent pid = %v, want nil after exit", steps[0].AgentPID)
	}
}

func TestExecutor_LogVsLogChunk(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var chunks []string
	var mu sync.Mutex

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			// Streaming chunk without trailing newline (simulates agent SSE delta).
			sctx.LogChunk("streaming partial")
			// Discrete message after unterminated stream - should get leading \n.
			sctx.Log("after stream")
			// Consecutive discrete message - no leading \n needed.
			sctx.Log("second discrete")
			// Raw streaming chunk should pass through unchanged.
			sctx.LogChunk("raw chunk")
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	onEvent := func(e ipc.Event) {
		if e.Type == ipc.EventLogChunk && e.Content != nil {
			mu.Lock()
			chunks = append(chunks, *e.Content)
			mu.Unlock()
		}
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, onEvent)
	exec.Execute(context.Background(), run, repo, workDir)

	mu.Lock()
	defer mu.Unlock()

	want := []string{
		"streaming partial",   // raw chunk, no newline
		"\nafter stream\n\n",  // leading \n flushes partial, trailing \n\n separates
		"second discrete\n\n", // no leading \n (previous Log ended with \n)
		"raw chunk",           // raw chunk, unchanged
	}
	if len(chunks) != len(want) {
		t.Fatalf("expected %d chunks, got %d: %q", len(want), len(chunks), chunks)
	}
	for i, w := range want {
		if chunks[i] != w {
			t.Errorf("chunks[%d] = %q, want %q", i, chunks[i], w)
		}
	}
}

func TestExecutor_RunLogDir(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	// Verify log dir was created
	logDir := p.RunLogDir(run.ID)
	if !dirExists(logDir) {
		t.Errorf("expected run log dir to exist: %s", logDir)
	}

	// Verify step log_path is set
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].LogPath == nil {
		t.Fatal("expected log_path to be set")
	}
	expected := filepath.Join(logDir, "review.log")
	if *dbSteps[0].LogPath != expected {
		t.Errorf("expected log_path %q, got %q", expected, *dbSteps[0].LogPath)
	}
}

func TestExecutor_LogFileWritten(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("first log line")
			sctx.Log("second log line")
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	// Verify log file exists and contains the log messages
	logPath := filepath.Join(p.RunLogDir(run.ID), "review.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file at %s: %v", logPath, err)
	}
	content := string(data)
	if !strings.Contains(content, "first log line") {
		t.Errorf("expected log file to contain 'first log line', got: %s", content)
	}
	if !strings.Contains(content, "second log line") {
		t.Errorf("expected log file to contain 'second log line', got: %s", content)
	}
}

func TestExecutor_LogFileWritten_OnStepError(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// The error a step returns (e.g. the underlying git stderr from a rejected
	// push) must be persisted to the step's own log file, not only surfaced via
	// the db/event. Otherwise the step log looks opaque: it shows the work
	// starting but never why it failed.
	stepErr := fmt.Errorf("push to upstream: git push: exit status 1: remote rejected: file exceeds 100.00 MB")
	exec := NewExecutor(database, p, nil, nil, []Step{newFailStep(types.StepPush, stepErr)}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err == nil {
		t.Fatal("expected error from failing step")
	}

	logPath := filepath.Join(p.RunLogDir(run.ID), "push.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file at %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), stepErr.Error()) {
		t.Errorf("expected push log to contain the step error %q, got: %s", stepErr.Error(), data)
	}
}

func TestExecutor_StepErrorRedactsCredentialURL(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// A step error carrying a credentialled upstream URL (as a real git push
	// rejection error would). It must be redacted before reaching the step log
	// file, the DB error column, the returned executor error, and the IPC event.
	const token = "ghp_secret_DO_NOT_LEAK"
	credURL := "https://x-access-token:" + token + "@github.com/o/r.git"
	stepErr := fmt.Errorf("push to upstream: git push %s: remote rejected: file exceeds 100.00 MB", credURL)

	ec := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newFailStep(types.StepPush, stepErr)}, ec.handler)
	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error from failing step")
	}

	// 1) Step log file must contain the redacted form, never the token.
	logPath := filepath.Join(p.RunLogDir(run.ID), "push.log")
	data, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("expected log file at %s: %v", logPath, rerr)
	}
	logContent := string(data)
	if strings.Contains(logContent, token) {
		t.Errorf("step log leaked credential: %s", logContent)
	}
	if !strings.Contains(logContent, "redacted@github.com/o/r.git") {
		t.Errorf("step log missing redacted URL, got: %s", logContent)
	}

	// 2) DB step error column must not carry the token.
	steps, _ := database.GetStepsByRun(run.ID)
	if steps[0].Error == nil {
		t.Fatal("expected step error to be persisted")
	}
	dbErr := *steps[0].Error
	if strings.Contains(dbErr, token) {
		t.Errorf("DB step error leaked credential: %q", dbErr)
	}
	if !strings.Contains(dbErr, "redacted@github.com/o/r.git") {
		t.Errorf("DB step error missing redacted URL, got: %q", dbErr)
	}

	// 3) Returned executor error must not carry the token.
	if strings.Contains(err.Error(), token) {
		t.Errorf("returned error leaked credential: %q", err.Error())
	}

	// 4) IPC step-completed event error must not carry the token.
	ev := ec.find(ipc.EventStepCompleted, types.StepPush)
	if ev == nil {
		t.Fatal("expected step completed event")
	}
	if ev.Error != nil && strings.Contains(*ev.Error, token) {
		t.Errorf("IPC event error leaked credential: %q", *ev.Error)
	}
}

func TestExecutor_LogFileMultipleSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step1 := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("review message")
			return &StepOutcome{}, nil
		},
	}
	step2 := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("test message")
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	// Each step should have its own log file
	reviewLog, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "review.log"))
	if err != nil {
		t.Fatalf("expected review log file: %v", err)
	}
	if !strings.Contains(string(reviewLog), "review message") {
		t.Errorf("review log missing message, got: %s", reviewLog)
	}

	testLog, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "test.log"))
	if err != nil {
		t.Fatalf("expected test log file: %v", err)
	}
	if !strings.Contains(string(testLog), "test message") {
		t.Errorf("test log missing message, got: %s", testLog)
	}

	// Review log should NOT contain test message
	if strings.Contains(string(reviewLog), "test message") {
		t.Error("review log should not contain test message")
	}
}
