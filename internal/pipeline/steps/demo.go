package steps

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var demoWait = func(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// IsDemoMode returns true when the NM_DEMO environment variable is set.
func IsDemoMode() bool {
	return os.Getenv("NM_DEMO") == "1"
}

// DemoSteps returns mock pipeline steps for demo recordings.
func DemoSteps() []pipeline.Step {
	return []pipeline.Step{
		&demoStep{
			name:       types.StepRebase,
			delay:      6 * time.Second,
			displayDur: 8 * time.Second,
			log:        "Fetching origin...\nChecking default branch...\nRebasing onto origin/main...\nAlready up to date.",
		},
		&demoStep{
			name:       types.StepReview,
			delay:      5 * time.Second,
			fixDelay:   4 * time.Second,
			displayDur: 45 * time.Second,
			log:        "Reviewing diff against main...\nAnalyzing changed files...\nChecking for bugs, security issues, and design problems...",
			fixLog:     "Fixing review findings...\nApplied fix: added nil check in handler\nApplied fix: removed unused import",
			findings: demoFindings{
				Items: []types.Finding{
					{ID: "review-1", Severity: "error", File: "internal/handler.go", Line: 42, Description: "Nil pointer dereference: req.Body used without nil check", Action: types.ActionAutoFix},
					{ID: "review-2", Severity: "warning", File: "internal/handler.go", Line: 5, Description: "Unused import \"fmt\"", Action: types.ActionAutoFix},
				},
				Summary:       "2 findings: 1 error, 1 warning",
				RiskLevel:     "medium",
				RiskRationale: "Missing nil check could cause runtime panic on malformed requests",
			},
		},
		&demoStep{
			name:       types.StepTest,
			delay:      4 * time.Second,
			displayDur: 32 * time.Second,
			log:        "Running: go test -race ./...\n\nok  \tgithub.com/kunchenguid/no-mistakes/internal/handler\t1.2s\nok  \tgithub.com/kunchenguid/no-mistakes/internal/config\t0.8s\nok  \tgithub.com/kunchenguid/no-mistakes/internal/server\t1.5s\n\nPASS",
		},
		&demoStep{
			name:       types.StepDocument,
			delay:      3 * time.Second,
			displayDur: 18 * time.Second,
			log:        "Checking documentation coverage...\nScanning changed files for doc gaps...\nAll documentation is up to date.",
		},
		&demoStep{
			name:       types.StepLint,
			delay:      3 * time.Second,
			fixDelay:   2 * time.Second,
			displayDur: 12 * time.Second,
			log:        "Running: golangci-lint run ./...\nChecking formatting and style...",
			fixLog:     "Fixing lint findings...\nApplied fix: formatted handler.go",
			findings: demoFindings{
				Items: []types.Finding{
					{ID: "lint-1", Severity: "warning", File: "internal/handler.go", Line: 38, Description: "File is not gofmt-ed", Action: types.ActionAutoFix},
				},
				Summary:   "1 finding: 1 warning",
				RiskLevel: "low",
			},
		},
		&demoStep{
			name:       types.StepPush,
			delay:      2 * time.Second,
			displayDur: 5 * time.Second,
			log:        "Pushing to origin...\nTo github.com:kunchenguid/no-mistakes.git\n   a1b2c3d..e4f5g6h  fix/nil-check -> fix/nil-check",
		},
		&demoStep{
			name:       types.StepPR,
			delay:      3 * time.Second,
			displayDur: 8 * time.Second,
			log:        "Creating pull request...\nhttps://github.com/kunchenguid/no-mistakes/pull/42",
			prURL:      "https://github.com/kunchenguid/no-mistakes/pull/42",
		},
		&demoCIStep{
			displayDur: 120 * time.Second,
		},
	}
}

type demoFindings struct {
	Items         []types.Finding `json:"findings"`
	Summary       string          `json:"summary"`
	RiskLevel     string          `json:"risk_level"`
	RiskRationale string          `json:"risk_rationale,omitempty"`
}

type demoStep struct {
	name       types.StepName
	delay      time.Duration
	fixDelay   time.Duration
	displayDur time.Duration // duration shown in TUI (overrides wall clock)
	log        string
	fixLog     string
	findings   demoFindings
	prURL      string
	fixed      bool
}

func (s *demoStep) Name() types.StepName { return s.name }

func (s *demoStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	// Fix round: emit fix log and return clean.
	if sctx.Fixing && s.fixLog != "" && !s.fixed {
		s.fixed = true
		if err := streamDemoLog(sctx, s.fixLog, s.fixDelay); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{}, nil
	}

	if err := streamDemoLog(sctx, s.log, s.delay); err != nil {
		return nil, err
	}

	outcome := &pipeline.StepOutcome{
		PRURL:              s.prURL,
		DurationOverrideMS: s.displayDur.Milliseconds(),
	}

	// Return findings on first run if we have them.
	if len(s.findings.Items) > 0 && !s.fixed {
		raw, err := json.Marshal(s.findings)
		if err != nil {
			return nil, err
		}
		outcome.Findings = string(raw)
		outcome.AutoFixable = true
	}

	return outcome, nil
}

// streamDemoLog emits log text in chunks with realistic pacing.
func streamDemoLog(sctx *pipeline.StepContext, text string, total time.Duration) error {
	if text == "" {
		return nil
	}
	if sctx.Ctx != nil && sctx.Ctx.Err() != nil {
		return sctx.Ctx.Err()
	}
	lines := splitLines(text)
	if len(lines) == 0 {
		return nil
	}
	pause := total / time.Duration(len(lines))
	if pause < 50*time.Millisecond {
		pause = 50 * time.Millisecond
	}
	for i, line := range lines {
		if sctx.Ctx != nil && sctx.Ctx.Err() != nil {
			return sctx.Ctx.Err()
		}
		if i > 0 && !demoWait(sctx.Ctx, pause) {
			if sctx.Ctx != nil && sctx.Ctx.Err() != nil {
				return sctx.Ctx.Err()
			}
			return nil
		}
		sctx.Log(line)
	}
	return nil
}

// demoCIStep simulates the CI monitor's failure-fix-retry flow.
// The real CI step handles its own fix loop internally (not through the executor),
// so this demo step does the same: emit logs that drive the TUI's CI view.
type demoCIStep struct {
	displayDur time.Duration
}

func (s *demoCIStep) Name() types.StepName { return types.StepCI }

func (s *demoCIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	pause := func(d time.Duration) bool { return demoWait(sctx.Ctx, d) }
	canceled := func() (*pipeline.StepOutcome, error) {
		if sctx.Ctx != nil && sctx.Ctx.Err() != nil {
			return nil, sctx.Ctx.Err()
		}
		return &pipeline.StepOutcome{DurationOverrideMS: s.displayDur.Milliseconds()}, nil
	}

	// Phase 1: initial monitoring, find a failure.
	sctx.Log("monitoring CI for PR #42")
	if !pause(2 * time.Second) {
		return canceled()
	}
	sctx.Log("")
	sctx.Log("  ✓  build (12s)")
	if !pause(1 * time.Second) {
		return canceled()
	}
	sctx.Log("  ✗  test (45s)")
	if !pause(500 * time.Millisecond) {
		return canceled()
	}
	sctx.Log("  ✓  lint (8s)")
	if !pause(1 * time.Second) {
		return canceled()
	}

	// Phase 2: failure detected, auto-fix triggered.
	sctx.Log("CI failures detected: test")
	if !pause(1 * time.Second) {
		return canceled()
	}
	sctx.Log("running agent to fix CI")
	if !pause(1 * time.Second) {
		return canceled()
	}
	sctx.Log("Diagnosing test failure from CI logs...")
	if !pause(2 * time.Second) {
		return canceled()
	}
	sctx.Log("Fix: updated handler_test.go to match new nil-check signature")
	if !pause(1 * time.Second) {
		return canceled()
	}
	sctx.Log("committed and pushed fixes")
	if !pause(2 * time.Second) {
		return canceled()
	}

	// Phase 3: re-monitor, all checks pass.
	sctx.Log("")
	sctx.Log("  ✓  build (11s)")
	if !pause(1 * time.Second) {
		return canceled()
	}
	sctx.Log("  ✓  test (44s)")
	if !pause(500 * time.Millisecond) {
		return canceled()
	}
	sctx.Log("  ✓  lint (8s)")
	if !pause(1 * time.Second) {
		return canceled()
	}
	sctx.Log("")
	sctx.Log("All checks passed.")

	return &pipeline.StepOutcome{
		DurationOverrideMS: s.displayDur.Milliseconds(),
	}, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
