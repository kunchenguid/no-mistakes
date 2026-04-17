package steps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func withoutDemoSleep(t *testing.T) {
	t.Helper()
	prev := demoWait
	demoWait = func(context.Context, time.Duration) bool { return true }
	t.Cleanup(func() {
		demoWait = prev
	})
}

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
	withoutDemoSleep(t)

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
	withoutDemoSleep(t)

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
	withoutDemoSleep(t)

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

func TestDemoStepExecuteReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prev := demoWait
	demoWait = func(context.Context, time.Duration) bool {
		cancel()
		return false
	}
	t.Cleanup(func() {
		demoWait = prev
	})

	step := &demoStep{
		name:  types.StepReview,
		delay: 2 * time.Second,
		log:   "first\nsecond",
	}

	var logs []string
	outcome, err := step.Execute(&pipeline.StepContext{
		Ctx:      ctx,
		Log:      func(s string) { logs = append(logs, s) },
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if outcome != nil {
		t.Fatalf("expected nil outcome, got %#v", outcome)
	}
	if len(logs) != 1 {
		t.Fatalf("expected logging to stop after cancellation, got %d lines", len(logs))
	}
	if logs[0] != "first" {
		t.Fatalf("got first log %q, want %q", logs[0], "first")
	}
	if !step.fixed {
		// sanity check that we did not accidentally enter the fix path
		step.fixed = false
	}
}

func TestDemoStepReviewRequiresApproval(t *testing.T) {
	withoutDemoSleep(t)

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

	// First execution should return findings that require human approval.
	outcome, err := review.Execute(sctx)
	if err != nil {
		t.Fatalf("first Execute() error: %v", err)
	}
	if outcome.Findings == "" {
		t.Fatal("expected findings on first execution")
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval=true so the demo pauses on the approval screen")
	}
	if outcome.AutoFixable {
		t.Fatal("expected AutoFixable=false so auto-fix does not bypass the approval screen")
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	for _, f := range parsed.Items {
		if f.Action != types.ActionAskUser {
			t.Errorf("finding %q: Action=%q, want %q", f.ID, f.Action, types.ActionAskUser)
		}
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
	withoutDemoSleep(t)

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

func TestStreamDemoLogStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prev := demoWait
	demoWait = func(context.Context, time.Duration) bool {
		cancel()
		return false
	}
	t.Cleanup(func() {
		demoWait = prev
	})

	var logs []string
	streamDemoLog(&pipeline.StepContext{
		Ctx:      ctx,
		Log:      func(s string) { logs = append(logs, s) },
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}, "first\nsecond", 2*time.Second)

	if len(logs) != 1 {
		t.Fatalf("expected logging to stop after cancellation, got %d lines", len(logs))
	}
	if logs[0] != "first" {
		t.Fatalf("got first log %q, want %q", logs[0], "first")
	}
}

func TestDemoCIStepStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prev := demoWait
	demoWait = func(context.Context, time.Duration) bool {
		cancel()
		return false
	}
	t.Cleanup(func() {
		demoWait = prev
	})

	var logs []string
	step := &demoCIStep{displayDur: 120 * time.Second}
	outcome, err := step.Execute(&pipeline.StepContext{
		Ctx:      ctx,
		Log:      func(s string) { logs = append(logs, s) },
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if outcome != nil {
		t.Fatalf("expected nil outcome, got %#v", outcome)
	}
	if len(logs) != 1 {
		t.Fatalf("expected CI demo to stop after cancellation, got %d lines", len(logs))
	}
	if logs[0] != "monitoring CI for PR #42" {
		t.Fatalf("got first log %q, want %q", logs[0], "monitoring CI for PR #42")
	}
}
