package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsDemoMode(t *testing.T) {
	if IsDemoMode() {
		t.Fatal("expected demo mode to be off by default")
	}
	t.Setenv("NM_DEMO", "1")
	if !IsDemoMode() {
		t.Fatal("expected demo mode to be on when NM_DEMO=1")
	}
}

func TestDemoSteps(t *testing.T) {
	steps := DemoSteps()
	want := []types.StepName{
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	if len(steps) != len(want) {
		t.Fatalf("DemoSteps() returned %d steps, want %d", len(steps), len(want))
	}
	for i, s := range steps {
		if s.Name() != want[i] {
			t.Errorf("step %d: got %s, want %s", i, s.Name(), want[i])
		}
	}
}

func TestAllStepsDemoMode(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	steps := AllSteps()
	// Verify we get demo steps, not real ones, by checking the type.
	for _, s := range steps {
		switch s.(type) {
		case *demoStep, *demoCIStep:
			// ok
		default:
			t.Fatalf("expected demo step type in demo mode, got %T", s)
		}
	}
}

func TestDemoStepExecute(t *testing.T) {
	steps := DemoSteps()
	for _, step := range steps {
		t.Run(string(step.Name()), func(t *testing.T) {
			var logs []string
			sctx := &pipeline.StepContext{
				Log:      func(s string) { logs = append(logs, s) },
				LogChunk: func(s string) { logs = append(logs, s) },
				LogFile:  func(string) {},
			}
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatalf("Execute() error: %v", err)
			}
			if outcome == nil {
				t.Fatal("Execute() returned nil outcome")
			}
			if len(logs) == 0 {
				t.Error("expected log output")
			}
		})
	}
}

func TestDemoStepReviewAutoFix(t *testing.T) {
	steps := DemoSteps()
	var review pipeline.Step
	for _, s := range steps {
		if s.Name() == types.StepReview {
			review = s
			break
		}
	}

	var logs []string
	sctx := &pipeline.StepContext{
		Log:      func(s string) { logs = append(logs, s) },
		LogChunk: func(s string) { logs = append(logs, s) },
		LogFile:  func(string) {},
	}

	// First execution should return findings.
	outcome, err := review.Execute(sctx)
	if err != nil {
		t.Fatalf("first Execute() error: %v", err)
	}
	if outcome.Findings == "" {
		t.Fatal("expected findings on first execution")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected AutoFixable=true")
	}

	// Fix execution should return clean.
	logs = nil
	sctx.Fixing = true
	sctx.PreviousFindings = outcome.Findings
	outcome, err = review.Execute(sctx)
	if err != nil {
		t.Fatalf("fix Execute() error: %v", err)
	}
	if outcome.Findings != "" {
		t.Fatal("expected no findings after fix")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "Fixing") || strings.Contains(l, "fix") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected fix log output")
	}
}

func TestDemoStepPRURL(t *testing.T) {
	steps := DemoSteps()
	var pr pipeline.Step
	for _, s := range steps {
		if s.Name() == types.StepPR {
			pr = s
			break
		}
	}

	sctx := &pipeline.StepContext{
		Log:      func(string) {},
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}

	outcome, err := pr.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if outcome.PRURL == "" {
		t.Fatal("expected PR URL from PR step")
	}
}
