package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type configuredIntentStep struct {
	steps.IntentStep
	enabled bool
}

func (s *configuredIntentStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if ctx.Config == nil {
		ctx.Config = &config.Config{}
	}
	ctx.Config.Intent.Enabled = s.enabled
	return s.IntentStep.Execute(ctx)
}

func TestAxiIntentSkipPersistsAndRenders(t *testing.T) {
	for _, tc := range []struct {
		name              string
		explicit, enabled bool
		cause             string
	}{
		{name: "disabled", cause: "disabled in config"},
		{name: "no transcript", enabled: true, cause: "no matching agent transcript"},
		{name: "explicit omission", explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, p, database, repo := setupAxiQueryRepo(t)
			run(t, dir, "git", "checkout", "-b", "feature/intent-skip")
			head, err := git.HeadSHA(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			r, err := database.InsertRun(repo.ID, "feature/intent-skip", head, head)
			if err != nil {
				t.Fatal(err)
			}
			executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{&configuredIntentStep{enabled: tc.enabled}}, nil)
			if tc.explicit {
				executor.SetSkippedSteps([]types.StepName{types.StepIntent})
			}
			if err := executor.Execute(context.Background(), r, repo, dir); err != nil {
				t.Fatal(err)
			}
			r, err = database.GetRun(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			results, err := database.GetStepsByRun(r.ID)
			if err != nil || len(results) != 1 || results[0].Status != types.StepStatusSkipped || r.Intent != nil {
				t.Fatalf("run = %+v, steps = %+v, err = %v", r, results, err)
			}
			wantOutcome := "passed-with-skips"
			wantSkips := 1
			if tc.explicit {
				wantOutcome, wantSkips = "passed", 0
				if results[0].SkipReason != nil && *results[0].SkipReason != "" {
					t.Fatalf("explicit omission acquired an automatic cause: %q", *results[0].SkipReason)
				}
			} else if results[0].SkipReason == nil || !strings.Contains(*results[0].SkipReason, tc.cause) {
				t.Fatalf("persisted skip lost its cause: %+v", results[0])
			}
			chdir(t, dir)
			assertOutput := func(output string, outcome string) {
				t.Helper()
				var doc struct {
					Outcome string `toon:"outcome"`
					Run     struct {
						HeadSHA        string             `toon:"head_sha"`
						AutomaticSkips []automaticSkipRow `toon:"automatic_skips"`
					} `toon:"run"`
				}
				if err := toon.UnmarshalString(output, &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Outcome != outcome || doc.Run.HeadSHA != head || len(doc.Run.AutomaticSkips) != wantSkips {
					t.Fatalf("output lost skip evidence: %+v\n%s", doc, output)
				}
				if !tc.explicit && (doc.Run.AutomaticSkips[0].Step != "intent" || doc.Run.AutomaticSkips[0].Reason != *results[0].SkipReason) {
					t.Fatalf("output changed skip cause: %+v", doc.Run.AutomaticSkips)
				}
			}
			assertOutput(axiStatusOutput(t, ""), wantOutcome)
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&output)
			reason := ""
			if results[0].SkipReason != nil {
				reason = *results[0].SkipReason
			}
			info := &ipc.RunInfo{ID: r.ID, Branch: r.Branch, HeadSHA: head, Status: types.RunCompleted,
				Steps: []ipc.StepResultInfo{{StepName: types.StepIntent, Status: types.StepStatusSkipped, SkipReason: reason}}}
			if err := renderDriveResult(cmd, info, false); err != nil {
				t.Fatal(err)
			}
			assertOutput(output.String(), wantOutcome)
			// A review gate carries the same warning before the run finishes.
			output.Reset()
			info.Status = types.RunRunning
			info.Steps = append(info.Steps, ipc.StepResultInfo{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval})
			if err := renderDriveResult(cmd, info, false); err != nil {
				t.Fatal(err)
			}
			assertOutput(output.String(), "")
		})
	}
}
