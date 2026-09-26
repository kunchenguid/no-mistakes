package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepFindingsFor(t *testing.T, database *db.DB, runID string, stepName types.StepName) types.Findings {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, sr := range steps {
		if sr.StepName != stepName {
			continue
		}
		if sr.FindingsJSON == nil {
			t.Fatalf("step %s has no findings", stepName)
		}
		findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			t.Fatalf("parse findings: %v", err)
		}
		return findings
	}
	t.Fatalf("step %s not found", stepName)
	return types.Findings{}
}

func repeatMarker(findings types.Findings) *types.Finding {
	for i := range findings.Items {
		if findings.Items[i].ID == RepeatFindingFindingID {
			return &findings.Items[i]
		}
	}
	return nil
}

// The netcup shape: an auto-fix round was dispatched for a finding and the
// rereview reported the same finding key again. The fixer must stop there and
// park the gate on "repeat finding: diagnose" instead of spending the rest of
// the auto_fix budget on the same brief.
func TestExecutor_RepeatFindingAfterAutoFixStopsTheFixer(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		// The rereview words it differently each round; the key is what repeats.
		return &StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      `{"findings":[{"id":"required-prefix-invariance-coverage-absent","severity":"error","file":"lib.go","description":"invariance coverage missing, pass ` + strings.Repeat("i", calls) + `","action":"auto-fix"}],"summary":"1 issue"}`,
		}, nil
	}}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	if calls != 2 {
		t.Fatalf("step ran %d times, want 2 (initial + the one fix whose finding came back)", calls)
	}
	findings := stepFindingsFor(t, database, run.ID, types.StepReview)
	marker := repeatMarker(findings)
	if marker == nil {
		t.Fatalf("gate findings carry no repeat-finding marker: %+v", findings.Items)
	}
	if marker.Action != types.ActionAskUser {
		t.Errorf("marker action = %q, want ask-user", marker.Action)
	}
	if !strings.HasPrefix(findings.Summary, "repeat finding: diagnose") || !strings.HasPrefix(marker.Description, "repeat finding: diagnose") {
		t.Errorf("summary/description must lead with the stop:\nsummary: %s\ndescription: %s", findings.Summary, marker.Description)
	}
	for _, want := range []string{"required-prefix-invariance-coverage-absent (rounds 1, 2)", "prerequisite or brief", "change family", "stronger model"} {
		if !strings.Contains(marker.Description, want) {
			t.Errorf("marker description missing %q: %s", want, marker.Description)
		}
	}

	// A bare fix is the round the stop prevents; the diagnosis gets through.
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err == nil || !strings.Contains(err.Error(), "repeat finding: diagnose") {
		t.Fatalf("bare fix at a repeat stop: err = %v, want a repeat-finding refusal", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

// A different finding after a fix round is progress, not a repeat: the fixer
// keeps going as before.
func TestExecutor_NewFindingAfterAutoFixStillAutoFixes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		switch calls {
		case 1:
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"nil-deref","severity":"error","file":"a.go","description":"nil dereference","action":"auto-fix"}],"summary":"1"}`}, nil
		case 2:
			if strings.Contains(sctx.PreviousFindings, RepeatFindingFindingID) {
				t.Error("fixer was handed the repeat marker")
			}
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, ReviewedPaths: []string{"a.go", "b.go"}, ReviewablePaths: []string{"a.go", "b.go"}, Findings: `{"findings":[{"id":"missing-close","severity":"error","file":"b.go","description":"file handle never closed","action":"auto-fix"}],"summary":"1"}`}, nil
		}
		return &StepOutcome{ReviewedPaths: []string{"a.go", "b.go"}, ReviewablePaths: []string{"a.go", "b.go"}}, nil
	}}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitExecutorDone(t, done)
	if calls != 3 {
		t.Fatalf("step ran %d times, want 3 (two auto-fix rounds for two distinct findings)", calls)
	}
}

// A repeat stops the round even when new findings arrive beside it, and the
// stop names only the finding that came back.
func TestExecutor_RepeatBesideANewFindingStillStops(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 3}}

	calls := 0
	step := &adaptiveCallStep{name: types.StepLint, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"severity":"warning","file":"x.go","description":"unused import","action":"auto-fix"}],"summary":"1"}`}, nil
		}
		// Positional ids shift between rounds; the file and description match.
		return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"severity":"warning","file":"y.go","description":"shadowed err","action":"auto-fix"},{"severity":"warning","file":"x.go","line":9,"description":"Unused  import","action":"auto-fix"}],"summary":"2"}`}, nil
	}}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepLint, types.StepStatusFixReview)
	if calls != 2 {
		t.Fatalf("step ran %d times, want 2", calls)
	}
	marker := repeatMarker(stepFindingsFor(t, database, run.ID, types.StepLint))
	if marker == nil || !strings.Contains(marker.Description, "lint-2 (rounds 1, 2)") || strings.Contains(marker.Description, "lint-1 (") {
		t.Fatalf("marker must name only the repeated finding: %+v", marker)
	}
	if err := exec.Respond(types.StepLint, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

// With auto_fix off, a driving agent's own `axi respond --action fix` is a fix
// round too: the same finding coming back after it is the same stop, and only
// a fix that carries a diagnosis is accepted.
func TestExecutor_RepeatFindingAfterOperatorFixStops(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}}

	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 3 && !strings.Contains(sctx.PreviousFindings, "cover the empty-prefix case") {
			t.Errorf("diagnosed fix did not reach the fixer: %s", sctx.PreviousFindings)
		}
		if calls == 3 && (strings.Contains(sctx.PreviousFindings, RepeatFindingFindingID) || strings.Contains(sctx.DeferredFindings, RepeatFindingFindingID)) {
			t.Error("fixer was handed the repeat marker")
		}
		return &StepOutcome{
			NeedsApproval: true,
			Findings:      `{"findings":[{"severity":"error","file":"lib.go","description":"prefix invariance is untested","action":"auto-fix"}],"summary":"1 issue"}`,
		}, nil
	}}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if marker := repeatMarker(stepFindingsFor(t, database, run.ID, types.StepReview)); marker != nil {
		t.Fatalf("first report is not a repeat: %+v", marker)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	marker := repeatMarker(stepFindingsFor(t, database, run.ID, types.StepReview))
	if marker == nil || !strings.Contains(marker.Description, "review-1 (rounds 1, 2)") {
		t.Fatalf("repeat after an operator fix must stop: %+v", marker)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err == nil {
		t.Fatal("a bare fix at a repeat stop was accepted")
	}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, map[string]string{"review-1": "cover the empty-prefix case"}, nil, ""); err != nil {
		t.Fatalf("diagnosed fix refused: %v", err)
	}
	waitForRounds(t, database, stepResultIDFor(t, database, run.ID, types.StepReview), 3)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
	if calls != 3 {
		t.Fatalf("step ran %d times, want 3", calls)
	}
}

// auto_fix off and no fix response: the gate is exactly what it was.
func TestExecutor_RepeatStopInertWithoutAFixRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"severity":"error","file":"lib.go","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`}, nil
	}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	findings := stepFindingsFor(t, database, run.ID, types.StepReview)
	if repeatMarker(findings) != nil || findings.Summary != "1 issue" {
		t.Fatalf("gate changed without any fix round: %+v", findings)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

func stepResultIDFor(t *testing.T, database *db.DB, runID string, stepName types.StepName) string {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, sr := range steps {
		if sr.StepName == stepName {
			return sr.ID
		}
	}
	t.Fatalf("step %s not found", stepName)
	return ""
}

func TestSameFindingAcrossRounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b types.Finding
		want bool
	}{
		{"same agent key, reworded", types.Finding{ID: "prefix-coverage", File: "a.go", Description: "x"}, types.Finding{ID: "prefix-coverage", File: "b.go", Description: "y"}, true},
		{"same positional id, different defect", types.Finding{ID: "review-1", File: "a.go", Description: "x"}, types.Finding{ID: "review-1", File: "a.go", Description: "y"}, false},
		{"same text, moved line", types.Finding{ID: "review-1", File: "./a.go", Line: 3, Description: "Nil  deref"}, types.Finding{ID: "review-4", File: "a.go", Line: 9, Description: "nil deref"}, true},
		{"same text, other file", types.Finding{File: "a.go", Description: "nil deref"}, types.Finding{File: "b.go", Description: "nil deref"}, false},
	}
	for _, tc := range cases {
		if got := sameFindingAcrossRounds(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRepeatFindingStopMarkerLeavesTheCarrySet(t *testing.T) {
	t.Parallel()
	stopped := withRepeatFindingStopJSON(`{"findings":[{"id":"a","severity":"error","file":"a.go","description":"x","action":"auto-fix"}],"summary":"s"}`, []repeatedFinding{{id: "a", rounds: []int{1, 2}}})
	if !HasRepeatFindingStop(stopped) {
		t.Fatalf("marker missing: %s", stopped)
	}
	dropped := dropRepeatFindingStopJSON(stopped)
	if HasRepeatFindingStop(dropped) || !hasFindingID(dropped, "a") {
		t.Fatalf("drop must remove only the marker: %s", dropped)
	}
	if repeatFixRefusal(types.StepReview, dropped) != "" {
		t.Fatal("fix refused without a repeat stop")
	}
}
