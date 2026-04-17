package tui

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsCIActive(t *testing.T) {
	run := testRunWithCI()

	// Pending → not active.
	if isCIActive(run.Steps) {
		t.Error("expected false when CI is pending")
	}

	// Running → active.
	run.Steps[5].Status = types.StepStatusRunning
	if !isCIActive(run.Steps) {
		t.Error("expected true when CI is running")
	}

	// Completed → not active.
	run.Steps[5].Status = types.StepStatusCompleted
	if isCIActive(run.Steps) {
		t.Error("expected false when CI is completed")
	}
}

func TestIsCIActive_NoCIStep(t *testing.T) {
	run := testRun() // no CI step
	if isCIActive(run.Steps) {
		t.Error("expected false when no CI step exists")
	}
}

func TestCIStepStatus(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning

	if got := ciStepStatus(run.Steps); got != types.StepStatusRunning {
		t.Errorf("expected running, got %s", got)
	}
}

func TestCIStepStatus_NoCIStep(t *testing.T) {
	run := testRun()
	if got := ciStepStatus(run.Steps); got != types.StepStatusPending {
		t.Errorf("expected pending (default), got %s", got)
	}
}

func TestExtractPRFromLogs(t *testing.T) {
	tests := []struct {
		name string
		logs []string
		want string
	}{
		{
			name: "standard CI message",
			logs: []string{"monitoring CI for PR #42 (timeout: 4h)..."},
			want: "42",
		},
		{
			name: "multiple logs",
			logs: []string{
				"some other log",
				"monitoring CI for PR #123 (timeout: 4h)...",
				"CI failures detected",
			},
			want: "123",
		},
		{
			name: "no PR reference",
			logs: []string{"running agent...", "completed"},
			want: "",
		},
		{
			name: "empty logs",
			logs: nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractPRFromLogs(tt.logs); got != tt.want {
				t.Errorf("extractPRFromLogs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseCIActivity(t *testing.T) {
	t.Run("empty logs", func(t *testing.T) {
		a := parseCIActivity(nil)
		if a.CIFixes != 0 || a.AutoFixing || a.LastEvent != "" {
			t.Error("expected zero activity for empty logs")
		}
	})

	t.Run("polling", func(t *testing.T) {
		a := parseCIActivity([]string{"monitoring CI for PR #42 (timeout: 4h)..."})
		if a.LastEvent == "" {
			t.Error("expected last event set")
		}
	})

	t.Run("ci failure detected", func(t *testing.T) {
		a := parseCIActivity([]string{
			"monitoring CI for PR #42 (timeout: 4h)...",
			"CI failures detected: test — auto-fixing...",
		})
		if a.CIFixes != 1 {
			t.Errorf("expected 1 CI fix, got %d", a.CIFixes)
		}
		if !a.AutoFixing {
			t.Error("expected auto-fixing to be true")
		}
	})

	t.Run("ci fix completed", func(t *testing.T) {
		a := parseCIActivity([]string{
			"CI failures detected: test — auto-fixing...",
			"running agent to fix CI failures...",
			"committed and pushed fixes",
		})
		if a.CIFixes != 1 {
			t.Errorf("expected 1 CI fix, got %d", a.CIFixes)
		}
		if a.AutoFixing {
			t.Error("expected auto-fixing to be false after push")
		}
	})

	t.Run("multiple ci fixes", func(t *testing.T) {
		a := parseCIActivity([]string{
			"CI failures detected: test",
			"committed and pushed fixes",
			"CI failures detected: lint",
			"running agent to fix CI failures...",
		})
		if a.CIFixes != 2 {
			t.Errorf("expected 2 CI fixes, got %d", a.CIFixes)
		}
	})

	t.Run("pr merged", func(t *testing.T) {
		a := parseCIActivity([]string{
			"monitoring CI for PR #42 (timeout: 4h)...",
			"PR has been merged!",
		})
		if !strings.Contains(a.LastEvent, "merged") {
			t.Error("expected merged as last event")
		}
	})

	t.Run("pr closed", func(t *testing.T) {
		a := parseCIActivity([]string{"PR has been closed"})
		if !strings.Contains(a.LastEvent, "closed") {
			t.Error("expected closed as last event")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		a := parseCIActivity([]string{"CI timeout reached"})
		if !strings.Contains(a.LastEvent, "timeout") {
			t.Error("expected timeout as last event")
		}
	})
}

func TestRenderCIView_Monitoring(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	out := renderCIView(run, run.Steps, "", logs, 80)

	if !strings.Contains(stripANSI(out), "CI") {
		t.Error("expected CI box title")
	}
	if !strings.Contains(out, "Monitoring") {
		t.Error("expected monitoring state")
	}
}

func TestRenderCIView_ShowsPRContextFromURL(t *testing.T) {
	run := testRunWithCI()
	run.PRURL = ptr("https://github.com/user/repo/pull/99")
	run.Steps[5].Status = types.StepStatusRunning

	out := stripANSI(renderCIView(run, run.Steps, "", nil, 80))

	if !strings.Contains(out, "PR #99") {
		t.Fatalf("expected CI panel to show PR context, got: %s", out)
	}
}

func TestRenderCIView_ShowsBitbucketPRContextFromURL(t *testing.T) {
	run := testRunWithCI()
	run.PRURL = ptr("https://bitbucket.org/user/repo/pull-requests/77")
	run.Steps[5].Status = types.StepStatusRunning

	out := stripANSI(renderCIView(run, run.Steps, "", nil, 80))

	if !strings.Contains(out, "PR #77") {
		t.Fatalf("expected CI panel to show Bitbucket PR context, got: %s", out)
	}
}

func TestRenderCIView_AutoFixing(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"CI failures detected: test — auto-fixing...",
		"running agent to fix CI failures...",
	}

	out := renderCIView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Auto-fixing CI") {
		t.Error("expected auto-fixing state indicator")
	}
	if !strings.Contains(out, "CI auto-fixes: 1") {
		t.Error("expected CI fix count")
	}
}

func TestRenderCIView_LastActivity(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"committed and pushed fixes",
	}

	out := renderCIView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Latest:") {
		t.Error("expected latest activity line")
	}
	if !strings.Contains(out, "committed and pushed fixes") {
		t.Error("expected last event text")
	}
}

func TestModel_View_CIViewWhenActive(t *testing.T) {
	run := testRunWithCI()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	view := m.View()

	if !strings.Contains(stripANSI(view), "CI") {
		t.Error("expected CI box in model output")
	}
	if !strings.Contains(view, "Monitoring") {
		t.Error("expected monitoring state in model output")
	}
}

func TestModel_View_NonCIStepUsesGenericFindings(t *testing.T) {
	run := testRun() // no CI step
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"severity":"error","description":"critical bug"}],"summary":"1 issue"}`

	view := m.View()

	// Should use generic findings, not CI view.
	// Check that no "CI" titled box appears (only Pipeline/Findings boxes).
	hasCIBox := false
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "CI") {
			hasCIBox = true
		}
	}
	if hasCIBox {
		t.Error("expected generic findings view, not CI box")
	}
	if !strings.Contains(view, "critical bug") {
		t.Error("expected generic findings content")
	}
}

func TestNewModel_PopulatesStepFindingsFromInitialSteps(t *testing.T) {
	findings := `{"findings":[{"severity":"warning","description":"potential null deref"}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID:      "run-001",
		RepoID:  "repo-001",
		Branch:  "feature/foo",
		HeadSHA: "abc123",
		BaseSHA: "000000",
		Status:  types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}

	m := NewModel("/tmp/sock", nil, run)

	// stepFindings should be populated from the initial steps' FindingsJSON.
	got, ok := m.stepFindings[types.StepReview]
	if !ok {
		t.Fatal("expected stepFindings to contain review step findings")
	}
	if got != findings {
		t.Errorf("stepFindings[review] = %q, want %q", got, findings)
	}
	// Step without findings should not appear in the map.
	if _, ok := m.stepFindings[types.StepTest]; ok {
		t.Error("expected stepFindings to NOT contain test step (no findings)")
	}
}

// --- Boxed section tests ---
