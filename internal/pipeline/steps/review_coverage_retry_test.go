package steps

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// These tests close the repeated partial reviewed_paths failure: a clean
// review round (zero findings) whose coverage record omits trusted reviewable
// files used to park the head with exactly one non-waiver way forward -
// selecting the coverage finding for a "fix" round that had nothing to fix,
// followed by a full re-review that re-rolled the same dice. A real 15-file
// branch parked three consecutive rounds this way, each time on a different
// omitted file. The repair enumerates the coverage contract in the prompt and
// adds one bounded focused completion turn; the gate's fail-closed park on
// anything still uncovered is unchanged.

const coverageCompletionMarker = "Focused coverage completion:"

// coverageFindingJSON renders a zero-finding review payload whose
// reviewed_paths is exactly the given slice (nil encodes the field omitted).
func coverageFindingJSON(reviewed []string) string {
	payload := map[string]any{
		"findings":       []any{},
		"risk_level":     "low",
		"risk_rationale": "clean",
		"risk_scope":     "source-or-external",
	}
	if reviewed != nil {
		payload["reviewed_paths"] = reviewed
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

// twoFileRepo extends the single-file template with a second changed file so
// partial coverage has somewhere real to be partial.
func twoFileRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(dir+"/feature2.txt", []byte("feature two code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add feature two")
	return dir, baseSHA, gitCmd(t, dir, "rev-parse", "HEAD")
}

// coverageScript is one scripted agent invocation: a plain review turn answer
// (onCompletion false) or the focused completion turn's answer (true).
type coverageScript struct {
	onCompletion bool
	output       string
}

// coverageScriptedAgent consumes scripts strictly in invocation order, so a
// test's second review turn cannot silently reuse the first turn's answer. A
// mismatch between the scripted turn type and the actual prompt fails the
// test instead of answering it.
type coverageScriptedAgent struct {
	*mockAgent
	t       *testing.T
	scripts []coverageScript
	next    int
}

func newCoverageScriptedAgent(t *testing.T, scripts []coverageScript) *coverageScriptedAgent {
	ag := &coverageScriptedAgent{mockAgent: &mockAgent{name: "coverage-scripted"}, t: t, scripts: scripts}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if ag.next >= len(ag.scripts) {
			ag.t.Errorf("unexpected agent invocation %d (prompt marker present: %v)", ag.next+1, strings.Contains(opts.Prompt, coverageCompletionMarker))
			return &agent.Result{Text: "coverage script exhausted"}, nil
		}
		script := ag.scripts[ag.next]
		ag.next++
		isCompletion := strings.Contains(opts.Prompt, coverageCompletionMarker)
		if script.onCompletion != isCompletion {
			ag.t.Errorf("invocation %d: scripted %s turn, got %s turn", ag.next, coverageTurnName(script.onCompletion), coverageTurnName(isCompletion))
		}
		return &agent.Result{Output: json.RawMessage(script.output)}, nil
	}
	return ag
}

func coverageTurnName(onCompletion bool) string {
	if onCompletion {
		return "completion"
	}
	return "review"
}

// TestReviewStep_PartialCoverageCompletesInOneFocusedPass is the red test for
// the production symptom: a zero-finding review whose reviewed_paths omits a
// trusted reviewable file. Before the repair the step parked on the first
// invocation and the completion turn never ran; after it, one focused pass
// covers exactly the omitted files and the round certifies the head with no
// operator decision.
func TestReviewStep_PartialCoverageCompletesInOneFocusedPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := newCoverageScriptedAgent(t, []coverageScript{
		{onCompletion: false, output: coverageFindingJSON([]string{"feature.txt"})},
		{onCompletion: true, output: coverageFindingJSON([]string{"feature2.txt"})},
	})
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(msg string) { logs = append(logs, msg) }

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("review invocations = %d, want 2 (review turn + one focused completion pass)", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatal("a completed coverage record must certify the head without an approval park")
	}
	if got := outcome.ReviewedPaths; len(got) != 2 {
		t.Fatalf("merged ReviewedPaths = %v, want both changed files", got)
	}
	completionPrompt := ag.calls[1].Prompt
	if !strings.HasPrefix(completionPrompt, ag.calls[0].Prompt) {
		t.Fatal("the completion turn must ride the full review prompt, not a bare fragment")
	}
	if !strings.Contains(completionPrompt, "feature2.txt") {
		t.Fatal("the completion prompt must name exactly the uncovered file")
	}
	if strings.Contains(completionPrompt, "  - feature.txt\n") {
		t.Fatal("the completion prompt must not re-list files the first turn already covered")
	}
	if ag.calls[1].Purpose != "review-coverage" {
		t.Fatalf("completion purpose = %q, want review-coverage so the turn stays auditable", ag.calls[1].Purpose)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "running one focused review pass over 1 unverified file(s): feature2.txt") {
		t.Fatalf("log must announce the focused pass and the file it covers; logs:\n%s", joined)
	}
}

// TestReviewStep_MultiRoundCoverageOmissionsConvergeWithoutAWaiver replays the
// production symptom across rounds: every full review turn returns zero
// findings with a DIFFERENT partial reviewed_paths record (round 1 omits
// feature2.txt, the post-fix rereview omits feature.txt). Before the repair
// each round parked and the only non-waiver path was a nothing-to-fix fix
// round plus a re-roll; after it, each round completes its own gap in one
// focused pass and both certify the head.
func TestReviewStep_MultiRoundCoverageOmissionsConvergeWithoutAWaiver(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := newCoverageScriptedAgent(t, []coverageScript{
		{onCompletion: false, output: coverageFindingJSON([]string{"feature.txt"})},
		{onCompletion: true, output: coverageFindingJSON([]string{"feature2.txt"})},
		// The fix-round rereview is a fresh session-free reviewer; its lossy
		// coverage record omits a DIFFERENT file.
		{onCompletion: false, output: coverageFindingJSON([]string{"feature2.txt"})},
		{onCompletion: true, output: coverageFindingJSON([]string{"feature.txt"})},
	})
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(msg string) { logs = append(logs, msg) }

	first, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("initial Execute() error = %v", err)
	}
	if first.NeedsApproval {
		t.Fatal("round 1 parked on a coverage gap the focused pass had closed")
	}

	// Round 2: the rereview turn after a fix selection, run without re-running
	// the fixer (the answer-finalize path exercises the same review turn).
	sctx.Fixing = true
	sctx.SkipFixExecution = true
	second, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rereview Execute() error = %v", err)
	}
	if second.NeedsApproval {
		t.Fatal("round 2 parked on a coverage gap the focused pass had closed")
	}
	if len(ag.calls) != 4 {
		t.Fatalf("review invocations = %d, want 4 (two review turns + two focused passes)", len(ag.calls))
	}
	for i, outcome := range []*pipeline.StepOutcome{first, second} {
		if got := outcome.ReviewedPaths; len(got) != 2 {
			t.Fatalf("round %d merged ReviewedPaths = %v, want full coverage", i+1, got)
		}
	}
	// Each round's focused pass must target that round's own gap: the
	// omissions change across rounds, which is exactly the production
	// symptom the naive full re-review re-rolled.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "running one focused review pass over 1 unverified file(s): feature2.txt") {
		t.Fatalf("round 1 must complete feature2.txt; logs:\n%s", joined)
	}
	if !strings.Contains(joined, "running one focused review pass over 1 unverified file(s): feature.txt") {
		t.Fatalf("round 2 must complete feature.txt; logs:\n%s", joined)
	}
}

// TestReviewStep_CoverageCompletionFailureStillParksExplicitly pins the
// fail-closed half: whatever goes wrong with the focused pass - an agent
// error or output that fails validation - the round parks on the explicit
// uncovered-file message and never approves.
func TestReviewStep_CoverageCompletionFailureStillParksExplicitly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		completionRun func(context.Context, agent.RunOpts) (*agent.Result, error)
	}{
		{
			name: "completion agent fails",
			completionRun: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return nil, context.DeadlineExceeded
			},
		},
		{
			name: "completion output fails validation",
			completionRun: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if strings.Contains(opts.Prompt, coverageCompletionMarker) {
					return &agent.Result{Text: "prose only, no structured findings"}, nil
				}
				return &agent.Result{Output: json.RawMessage(coverageFindingJSON(nil))}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := twoFileRepo(t)
			ag := &mockAgent{name: "coverage-fail"}
			ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if strings.Contains(opts.Prompt, coverageCompletionMarker) {
					return tc.completionRun(ctx, opts)
				}
				return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{"feature.txt"}))}, nil
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			var logs []string
			sctx.Log = func(msg string) { logs = append(logs, msg) }

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("a failed completion pass must park, not fail the run: %v", err)
			}
			if !outcome.NeedsApproval {
				t.Fatal("an incomplete coverage record must never approve")
			}
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, "review coverage is incomplete; parking for approval with 1 reviewable file(s) unverified: feature2.txt") {
				t.Fatalf("park must name the uncovered file explicitly; logs:\n%s", joined)
			}
		})
	}
}

// TestReviewStep_CoverageCompletionFabricationStillParks keeps the strict
// coverage rule intact across the merge: a focused pass that covers the
// missing file but also names an out-of-scope path fails the union check and
// parks with the fabricated entry named.
func TestReviewStep_CoverageCompletionFabricationStillParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := &mockAgent{name: "coverage-fabricator"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, coverageCompletionMarker) {
			return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{"feature2.txt", "fabricated.txt"}))}, nil
		}
		return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{"feature.txt"}))}, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(msg string) { logs = append(logs, msg) }

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a coverage record containing a fabricated path must never approve")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "1 reviewed_paths entry(ies) outside the reviewable set: fabricated.txt") {
		t.Fatalf("park must name the fabricated entry; logs:\n%s", joined)
	}
}

// TestReviewStep_BlockingFindingsSkipTheCoverageCompletion keeps the
// completion pass free: it exists to close coverage gaps on otherwise-clean
// rounds, so a round with findings parks exactly as before, on one invocation.
func TestReviewStep_BlockingFindingsSkipTheCoverageCompletion(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := &mockAgent{name: "blocking-review"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, coverageCompletionMarker) {
			t.Fatal("a round with findings must not run a focused coverage pass")
		}
		output := `{"findings":[{"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"nil deref","action":"auto-fix"}],"risk_level":"low","risk_rationale":"one","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`
		return &agent.Result{Output: json.RawMessage(output)}, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("review invocations = %d, want 1 (no focused pass beside findings)", len(ag.calls))
	}
	if !outcome.NeedsApproval {
		t.Fatal("blocking findings must park the round")
	}
}

// TestReviewStep_ReviewPromptEnumeratesTheCoverageContract pins the prompt
// half of the repair: the reviewer is handed the canonical changed-file list
// the pipeline holds the round to, turning reviewed_paths into a checklist it
// can verify before returning instead of a list it must reconstruct from
// memory.
func TestReviewStep_ReviewPromptEnumeratesTheCoverageContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := newStaticReviewAgent(coverageFindingJSON([]string{"feature.txt", "feature2.txt"}))
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("full coverage must not spend the focused pass; invocations = %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Changed files this review is held to") {
		t.Fatal("the review prompt must enumerate the coverage contract")
	}
	for _, want := range []string{"- feature.txt\n", "- feature2.txt\n"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the coverage contract must list %q; prompt:\n%s", strings.TrimSpace(want), prompt)
		}
	}
	if !strings.Contains(prompt, "never treats an omission as clean") {
		t.Fatal("the coverage contract must keep the honest-reporting rule")
	}
}
