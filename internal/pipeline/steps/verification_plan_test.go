package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

func TestReviewAndTestRefuseLostVerificationPlan(t *testing.T) {
	for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			root := t.TempDir()
			source := filepath.Join(root, "plan.txt")
			if err := os.WriteFile(source, []byte("verify the output\n"), 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := verificationplan.Capture(filepath.Join(root, "inputs"), source, sctx.Run.RepoID, sctx.Run.Branch, head)
			if err != nil {
				t.Fatal(err)
			}
			sctx.Run.VerificationPlan = plan
			if err := os.Chmod(plan.Path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(plan.Path); err != nil {
				t.Fatal(err)
			}
			outcome, err := step.Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "read captured verification plan") || outcome != nil {
				t.Fatalf("lost attachment accepted: %+v %v", outcome, err)
			}
			if len(ag.calls) != 0 {
				t.Fatal("agent invoked without its required captured evidence")
			}
		})
	}
}

func TestVerificationPlanExecutionGuidanceOnlyReachesTest(t *testing.T) {
	for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			_, isTest := step.(*TestStep)
			output := passingScenarioFindingsJSON
			if !isTest {
				output = `{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"bounded","risk_scope":"source-or-external"}`
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(output)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			root := t.TempDir()
			source := filepath.Join(root, "plan.txt")
			content := "Observe checkout success and retain the receipt."
			if err := os.WriteFile(source, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := verificationplan.Capture(filepath.Join(root, "inputs"), source, sctx.Run.RepoID, sctx.Run.Branch, head)
			if err != nil {
				t.Fatal(err)
			}
			sctx.Run.VerificationPlan = plan
			if _, err := step.Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) == 0 {
				t.Fatal("no prompt delivered")
			}
			prompt := ag.calls[0].Prompt
			if !strings.Contains(prompt, content) || !strings.Contains(prompt, "author-supplied evidence, not user intent") {
				t.Fatal("captured evidence missing from delivered prompt")
			}
			if strings.Contains(prompt, "perform repeatable product verification and retain its artifact") != isTest {
				t.Fatal("plan execution guidance delivered to wrong phase")
			}
		})
	}
}
