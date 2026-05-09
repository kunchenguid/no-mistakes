package steps

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// newIntentStepContext builds a StepContext backed by a real DB and
// freshly-inserted repo + run, suitable for testing IntentStep without
// requiring a real git repository or transcripts.
func newIntentStepContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(t.TempDir(), "git@example.com:test/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", "head-sha", "base-sha")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	return &pipeline.StepContext{
		Ctx:     context.Background(),
		Run:     run,
		Repo:    repo,
		WorkDir: repo.WorkingPath,
		Config: &config.Config{
			Intent: config.Intent{Enabled: true},
		},
		DB:       database,
		Log:      func(string) {},
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}
}

func TestIntentStep_SuccessPersistsAndAttaches(t *testing.T) {
	sctx := newIntentStepContext(t)
	step := &IntentStep{
		runIntent: func(_ context.Context, _ *pipeline.StepContext) (*intent.Result, error) {
			return &intent.Result{
				Summary:   "user added Bar()",
				AgentName: "claude",
				SessionID: "s-1",
				Score:     0.9,
			}, nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome on success, got %+v", outcome)
	}
	if sctx.Run.Intent == nil || *sctx.Run.Intent != "user added Bar()" {
		t.Errorf("in-memory run.Intent = %v, want %q", sctx.Run.Intent, "user added Bar()")
	}
	persisted, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if persisted.Intent == nil || *persisted.Intent != "user added Bar()" {
		t.Errorf("db intent = %v, want %q", persisted.Intent, "user added Bar()")
	}
	if persisted.IntentSource == nil || *persisted.IntentSource != "claude" {
		t.Errorf("db intent source = %v, want claude", persisted.IntentSource)
	}
	if persisted.IntentSessionID == nil || *persisted.IntentSessionID != "s-1" {
		t.Errorf("db intent session = %v, want s-1", persisted.IntentSessionID)
	}
	if persisted.IntentScore == nil || *persisted.IntentScore != 0.9 {
		t.Errorf("db intent score = %v, want 0.9", persisted.IntentScore)
	}
}

func TestIntentStep_NoMatchReturnsSkipped(t *testing.T) {
	sctx := newIntentStepContext(t)
	step := &IntentStep{
		runIntent: func(_ context.Context, _ *pipeline.StepContext) (*intent.Result, error) {
			return nil, intent.ErrNoMatch
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped on no-match, got %+v", outcome)
	}
	if sctx.Run.Intent != nil {
		t.Errorf("run.Intent should remain nil on no-match, got %q", *sctx.Run.Intent)
	}
}

func TestIntentStep_ExtractErrorReturnsSkippedNotError(t *testing.T) {
	sctx := newIntentStepContext(t)
	step := &IntentStep{
		runIntent: func(_ context.Context, _ *pipeline.StepContext) (*intent.Result, error) {
			return nil, errors.New("transcript reader exploded")
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute should swallow extractor errors, got %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped on error, got %+v", outcome)
	}
	if sctx.Run.Intent != nil {
		t.Errorf("run.Intent should remain nil on error, got %q", *sctx.Run.Intent)
	}
}

func TestIntentStep_DisabledByConfigSkipsExtractor(t *testing.T) {
	sctx := newIntentStepContext(t)
	sctx.Config.Intent.Enabled = false

	called := false
	step := &IntentStep{
		runIntent: func(_ context.Context, _ *pipeline.StepContext) (*intent.Result, error) {
			called = true
			return nil, nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped when disabled, got %+v", outcome)
	}
	if called {
		t.Errorf("runIntent must not run when intent extraction is disabled")
	}
}

func TestIntentStep_PanicReturnsSkipped(t *testing.T) {
	sctx := newIntentStepContext(t)
	step := &IntentStep{
		runIntent: func(_ context.Context, _ *pipeline.StepContext) (*intent.Result, error) {
			panic("boom")
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute should swallow panic, got %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped on panic, got %+v", outcome)
	}
}
