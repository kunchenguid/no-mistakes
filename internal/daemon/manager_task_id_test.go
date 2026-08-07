package daemon

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// captureTaskIDStep records the task id the executor handed to a running step,
// which is the value the PR step would bake into the title.
type captureTaskIDStep struct {
	seen chan<- conventional.TaskID
}

func (s *captureTaskIDStep) Name() types.StepName { return types.StepReview }
func (s *captureTaskIDStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.seen <- sctx.TaskID
	return &pipeline.StepOutcome{}, nil
}

// TestStartRunStampsTaskIDOntoTheRunAndReachesSteps covers the whole run-start
// half of the plumbing: a supplied task id is persisted on the run row and the
// executor hands it back to steps, format included.
func TestStartRunStampsTaskIDOntoTheRunAndReachesSteps(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "task-id")

	seen := make(chan conventional.TaskID, 1)
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&captureTaskIDStep{seen: seen}}
	})
	t.Cleanup(manager.Shutdown)

	task := conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatPrefix}
	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "", task)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if got := <-seen; got != task {
		t.Fatalf("step task id = %+v, want %+v", got, task)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}

	stored, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TaskID == nil || *stored.TaskID != "WA-3093" {
		t.Fatalf("persisted task id = %v, want WA-3093", stored.TaskID)
	}
	if stored.TaskIDFormat == nil || *stored.TaskIDFormat != string(conventional.TaskIDFormatPrefix) {
		t.Fatalf("persisted task id format = %v, want prefix", stored.TaskIDFormat)
	}
}

// TestStartRunWithoutATaskIDLeavesTheRunUnstamped keeps the absent case a full
// no-op rather than an empty-string stamp.
func TestStartRunWithoutATaskIDLeavesTheRunUnstamped(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "no-task-id")

	seen := make(chan conventional.TaskID, 1)
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&captureTaskIDStep{seen: seen}}
	})
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "", conventional.TaskID{})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if got := <-seen; !got.Empty() {
		t.Fatalf("step task id = %+v, want empty", got)
	}
	waitForRunTerminalState(t, database, runID)

	stored, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TaskID != nil || stored.TaskIDFormat != nil {
		t.Fatalf("run was stamped without a task id: %v / %v", stored.TaskID, stored.TaskIDFormat)
	}
}

// TestTaskIDIsStickyPerBranch is the regression for a later run stripping the
// ticket reference back off an already-open PR: the PR step rewrites the title
// on every run, so a rerun or follow-up that omits --task-id must inherit the
// id the branch already carries.
func TestTaskIDIsStickyPerBranch(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "sticky-task-id")

	seen := make(chan conventional.TaskID, 4)
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&captureTaskIDStep{seen: seen}}
	})
	t.Cleanup(manager.Shutdown)

	run := func(branch string, task conventional.TaskID) conventional.TaskID {
		t.Helper()
		runID, err := manager.startRun(context.Background(), repo, branch, head, refreshTestZeroSHA, "test", nil, "", task)
		if err != nil {
			t.Fatalf("start run: %v", err)
		}
		got := <-seen
		waitForRunTerminalState(t, database, runID)
		return got
	}

	first := conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatPrefix}
	if got := run("main", first); got != first {
		t.Fatalf("first run task id = %+v, want %+v", got, first)
	}

	// A run that supplies nothing keeps the branch's id, format included.
	if got := run("main", conventional.TaskID{}); got != first {
		t.Fatalf("inherited task id = %+v, want %+v", got, first)
	}

	// An explicit id always wins over the inherited one.
	explicit := conventional.TaskID{ID: "WA-4001", Format: conventional.TaskIDFormatSuffix}
	if got := run("main", explicit); got != explicit {
		t.Fatalf("explicit task id = %+v, want %+v", got, explicit)
	}
	if got := run("main", conventional.TaskID{}); got != explicit {
		t.Fatalf("inherited task id after an override = %+v, want %+v", got, explicit)
	}

	// Inheritance is per branch: a ticket id belongs to one branch, not to the
	// repository, so an unrelated branch stays unstamped.
	if got := run("other", conventional.TaskID{}); !got.Empty() {
		t.Fatalf("unrelated branch inherited a task id: %+v", got)
	}
}

func TestTaskIDFromParams(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		format string
		want   conventional.TaskID
	}{
		{"id and format", "WA-3093", "suffix", conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatSuffix}},
		{"id without format", "WA-3093", "", conventional.TaskID{ID: "WA-3093", Format: conventional.DefaultTaskIDFormat}},
		// An unknown format must not drop the id: the tracking reference is the
		// point, and the release-safe default is always a valid placement.
		{"unknown format falls back", "WA-3093", "jira", conventional.TaskID{ID: "WA-3093", Format: conventional.DefaultTaskIDFormat}},
		{"no id", "", "prefix", conventional.TaskID{}},
		{"unusable id", "WA\n3093", "prefix", conventional.TaskID{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := taskIDFromParams(tt.id, tt.format); got != tt.want {
				t.Fatalf("taskIDFromParams(%q, %q) = %+v, want %+v", tt.id, tt.format, got, tt.want)
			}
		})
	}
}
