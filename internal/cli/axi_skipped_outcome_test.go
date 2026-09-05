package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// unavailableProviderStep runs the real PR/CI step with no provider executable.
type unavailableProviderStep struct {
	pipeline.Step
	path string
}

func (s unavailableProviderStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx.Env = []string{"PATH=" + s.path}
	return s.Step.Execute(ctx)
}

func TestAxiOutcomeProviderUnavailableSkips(t *testing.T) {
	dir, p, database, repo := setupAxiQueryRepo(t)
	run(t, dir, "git", "checkout", "-b", "feature/skips")
	repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := database.InsertRun(repo.ID, "feature/skips", head, head)
	if err != nil {
		t.Fatal(err)
	}
	missingCLI := t.TempDir()
	executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{
		unavailableProviderStep{&steps.PRStep{}, missingCLI},
		unavailableProviderStep{&steps.CIStep{}, missingCLI},
	}, nil)
	if err := executor.Execute(context.Background(), r, repo, dir); err != nil {
		t.Fatal(err)
	}
	r, err = database.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	results, err := database.GetStepsByRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != types.RunCompleted || len(results) != 2 {
		t.Fatalf("run = %+v, steps = %+v", r, results)
	}
	for _, result := range results {
		if result.Status != types.StepStatusSkipped {
			t.Fatalf("%s status = %s, want skipped", result.StepName, result.Status)
		}
	}
	rv := runViewFromDB(r, results, database)
	if got := outcomeForRun(rv); got != "passed-with-skips" {
		t.Fatalf("provider-unavailable PR and CI: outcome = %q, want passed-with-skips", got)
	}
	for _, result := range results {
		if result.SkipReason == nil || !strings.Contains(*result.SkipReason, "glab") {
			t.Fatalf("%s lost the provider skip cause: %v", result.StepName, result.SkipReason)
		}
	}
	chdir(t, dir)
	var status struct {
		Outcome string `toon:"outcome"`
		Run     struct {
			HeadSHA        string             `toon:"head_sha"`
			AutomaticSkips []automaticSkipRow `toon:"automatic_skips"`
		} `toon:"run"`
	}
	if err := toon.UnmarshalString(axiStatusOutput(t, ""), &status); err != nil {
		t.Fatal(err)
	}
	if status.Outcome != "passed-with-skips" || status.Run.HeadSHA != head || len(status.Run.AutomaticSkips) != 2 {
		t.Fatalf("status lost skip evidence or exact head: %+v", status)
	}

	// The explicit --skip path bypasses execution and must keep its prior outcome.
	explicit, err := database.InsertRun(repo.ID, "feature/skips", head, head)
	if err != nil {
		t.Fatal(err)
	}
	executor.SetSkippedSteps([]types.StepName{types.StepPR, types.StepCI})
	if err := executor.Execute(context.Background(), explicit, repo, dir); err != nil {
		t.Fatal(err)
	}
	explicitResults, err := database.GetStepsByRun(explicit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeForRun(runViewFromDB(explicit, explicitResults, database)); got != "passed" {
		t.Fatalf("explicit per-run skips: outcome = %q, want passed", got)
	}
}

func TestAxiDriveAutomaticSkips(t *testing.T) {
	for _, tc := range []struct {
		name, reason, override string
		status                 types.RunStatus
		stepStatus             types.StepStatus
		want                   string
	}{
		{"automatic skip", "gh is not authenticated", "", types.RunCompleted, types.StepStatusSkipped, "passed-with-skips"},
		{"explicit skip", "", "", types.RunCompleted, types.StepStatusSkipped, "passed"},
		{"completed step", "stale reason", "", types.RunCompleted, types.StepStatusCompleted, "passed"},
		{"override and skip", "provider unavailable", "live CI failure", types.RunCompleted, types.StepStatusSkipped, "passed-with-override"},
		{"failed run", "provider unavailable", "", types.RunFailed, types.StepStatusSkipped, "failed"},
		{"cancelled run", "provider unavailable", "", types.RunCancelled, types.StepStatusSkipped, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			r := &ipc.RunInfo{ID: "skip-run", Status: tc.status, HeadSHA: strings.Repeat("a", 40), CIOverrideReason: tc.override,
				Steps: []ipc.StepResultInfo{{StepName: types.StepCI, Status: tc.stepStatus, SkipReason: tc.reason}}}
			err := renderDriveResult(cmd, r, false)
			if (err == nil) != (tc.status == types.RunCompleted) {
				t.Fatalf("renderDriveResult error = %v for %s", err, tc.status)
			}
			var doc struct {
				Outcome string `toon:"outcome"`
				Run     struct {
					HeadSHA        string             `toon:"head_sha"`
					AutomaticSkips []automaticSkipRow `toon:"automatic_skips"`
				} `toon:"run"`
			}
			if err := toon.UnmarshalString(out.String(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Outcome != tc.want || doc.Run.HeadSHA != r.HeadSHA {
				t.Fatalf("drive output = %+v", doc)
			}
			if tc.reason != "" && tc.stepStatus == types.StepStatusSkipped {
				if len(doc.Run.AutomaticSkips) != 1 || doc.Run.AutomaticSkips[0].Reason != tc.reason {
					t.Fatalf("skip cause lost: %+v", doc.Run.AutomaticSkips)
				}
			} else if len(doc.Run.AutomaticSkips) != 0 {
				t.Fatalf("unexpected automatic skips: %+v", doc.Run.AutomaticSkips)
			}
		})
	}
}
