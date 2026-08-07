package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var errAgentUnavailableForTaskIDTest = errors.New("agent unavailable")

// prTitleFromFakeCLILog returns the value of the single --title argument the
// fake provider CLI recorded, so assertions test the exact string the PR
// actually received rather than a substring of the whole argv log.
func prTitleFromFakeCLILog(t *testing.T, logFile string) string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.SplitN(line, " --title ", 2)
		if len(fields) != 2 {
			continue
		}
		// Every gh PR invocation puts --body-file last, so the title runs up to
		// that flag (titles may legitimately contain spaces).
		title := fields[1]
		if idx := strings.Index(title, " --body-file"); idx >= 0 {
			title = title[:idx]
		}
		return strings.TrimSpace(title)
	}
	t.Fatalf("no --title argument recorded in provider CLI log:\n%s", data)
	return ""
}

// newTaskIDPRContext builds a PR-step context whose agent returns a fixed
// conventional title, so the only variable under test is the task-id baking.
func newTaskIDPRContext(t *testing.T, task conventional.TaskID, existingPRURL string) (*pipeline.StepContext, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, existingPRURL)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix(carousel): tighten slide spacing","body":"## What Changed\n\n- tighten slide spacing"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.TaskID = task

	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	return sctx, logFile
}

func TestPRStep_CreateBakesTaskIDIntoTitlePerFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		format conventional.TaskIDFormat
		want   string
	}{
		{"release-please", conventional.TaskIDFormatReleasePlease, "fix(carousel): tighten slide spacing (WA-3093)"},
		{"prefix", conventional.TaskIDFormatPrefix, "[WA-3093] fix(carousel): tighten slide spacing"},
		{"suffix", conventional.TaskIDFormatSuffix, "fix(carousel): tighten slide spacing [WA-3093]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := conventional.TaskID{ID: "WA-3093", Format: tt.format}
			sctx, logFile := newTaskIDPRContext(t, task, "")

			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if got := prTitleFromFakeCLILog(t, logFile); got != tt.want {
				t.Fatalf("created PR title = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRStep_ReleasePleaseFormatKeepsConventionalTypeParseable(t *testing.T) {
	t.Parallel()
	// The default format exists so release-please still sees the type at
	// position 0. Assert the real title the provider received, not just that
	// the id is somewhere in the string.
	task := conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatReleasePlease}
	sctx, logFile := newTaskIDPRContext(t, task, "")

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	title := prTitleFromFakeCLILog(t, logFile)
	if !strings.HasPrefix(title, "fix(carousel): ") {
		t.Fatalf("release-please format must keep type(scope) at position 0, got %q", title)
	}
	if !conventional.IsTitle(title) {
		t.Fatalf("release-please format produced a non-conventional title: %q", title)
	}
	if !strings.Contains(title, "(WA-3093)") {
		t.Fatalf("release-please format dropped the task id: %q", title)
	}
}

func TestPRStep_UpdateBakesTaskIDIntoTitle(t *testing.T) {
	t.Parallel()
	task := conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatPrefix}
	sctx, logFile := newTaskIDPRContext(t, task, "https://github.com/test/repo/pull/42")

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "pr edit") {
		t.Fatalf("expected the update path to run gh pr edit, got:\n%s", data)
	}
	want := "[WA-3093] fix(carousel): tighten slide spacing"
	if got := prTitleFromFakeCLILog(t, logFile); got != want {
		t.Fatalf("updated PR title = %q, want %q", got, want)
	}
}

func TestPRStep_UpdateDoesNotAccreteAnAlreadyPresentTaskID(t *testing.T) {
	t.Parallel()
	// Re-running the pipeline on the same branch must not stack a second copy
	// of the id onto a title that already carries it. The agent drafting a
	// title that already ends in the id is exactly what a re-run looks like.
	task := conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatReleasePlease}
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix(carousel): tighten slide spacing (WA-3093)","body":"## What Changed\n\n- tighten slide spacing"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.TaskID = task

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	title := prTitleFromFakeCLILog(t, logFile)
	want := "fix(carousel): tighten slide spacing (WA-3093)"
	if title != want {
		t.Fatalf("updated PR title = %q, want the unchanged %q", title, want)
	}
	if strings.Count(title, "WA-3093") != 1 {
		t.Fatalf("task id accreted on the update path: %q", title)
	}
}

func TestPRStep_NoTaskIDLeavesTheGeneratedTitleUntouched(t *testing.T) {
	t.Parallel()
	// An absent --task-id must be a full no-op: never an empty "()" or "[]".
	sctx, logFile := newTaskIDPRContext(t, conventional.TaskID{}, "")

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	want := "fix(carousel): tighten slide spacing"
	got := prTitleFromFakeCLILog(t, logFile)
	if got != want {
		t.Fatalf("PR title = %q, want the untouched %q", got, want)
	}
	if strings.Contains(got, "()") || strings.Contains(got, "[]") {
		t.Fatalf("empty task id rendered placeholder brackets: %q", got)
	}
}

func TestPRStep_TaskIDAppliesToTheFallbackTitleToo(t *testing.T) {
	t.Parallel()
	// When drafting fails the step falls back to a neutral conventional title;
	// the tracking id still belongs on it.
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return nil, errAgentUnavailableForTaskIDTest
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.TaskID = conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatReleasePlease}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	want := "chore: update pull request (WA-3093)"
	if got := prTitleFromFakeCLILog(t, logFile); got != want {
		t.Fatalf("fallback PR title = %q, want %q", got, want)
	}
}
