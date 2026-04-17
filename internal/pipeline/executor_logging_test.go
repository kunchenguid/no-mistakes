package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
