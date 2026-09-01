package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// capturePrompt runs one review turn and returns the prompt the reviewer saw.
func capturePrompt(t *testing.T, fixing bool, scope string) string {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	var prompt string
	ag := &mockAgent{
		name: "prompt-probe",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "Review the code changes") {
				prompt = opts.Prompt
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.RereviewScope = scope
	if fixing {
		sctx.Fixing = true
		sctx.SkipFixExecution = true
		sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"error","file":"a.txt","line":1,"description":"bug","action":"auto-fix"}]}`
	}
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if prompt == "" {
		t.Fatalf("review prompt was never issued")
	}
	return prompt
}

// The initial review carries the reachability rule and the re-balanced
// action classification, in every scope.
func TestReviewStep_PromptCarriesReachabilityAndActionBalanceRules(t *testing.T) {
	prompt := capturePrompt(t, false, "")
	for _, want := range []string{
		"must name an input, state, or call sequence the system can actually receive",
		"sparse-array holes in JSON-sourced data, wall-clock rollback, in-process Proxy or prototype adversaries",
		"When in doubt about the author's INTENT, default to \"ask-user\"",
		"is \"auto-fix\" regardless of its severity; severity says how bad the defect is, not who decides the remedy",
		"Do a full review pass before returning",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("initial review prompt missing %q:\n%s", want, prompt)
		}
	}
}

// review.rereview_scope decides whether a rereview re-enumerates the branch
// (default, the upstream contract) or reads only the fix-round diff.
func TestReviewStep_RereviewScopeSelectsTheTaskBlock(t *testing.T) {
	full := capturePrompt(t, true, "")
	if !strings.Contains(full, "Do a full review pass before returning") || strings.Contains(full, "scoped to the fix-round diff") {
		t.Fatalf("default rereview must stay a full pass:\n%s", full)
	}

	scoped := capturePrompt(t, true, config.ReviewRereviewScopeFixDiff)
	for _, want := range []string{
		"Task (re-review of this run's fix round, scoped to the fix-round diff)",
		"First verify each previous finding against the current code",
		"Author code outside the fix-round diff was reviewed in the initial round and is out of scope",
		"Do not continue into a full branch review",
		"review scope: fix-round changes only: commits after starting head",
		// The shared discipline and the provenance clause survive scoping.
		"construct at least one concrete input or state and trace it",
		"Fix-round provenance:",
	} {
		if !strings.Contains(scoped, want) {
			t.Fatalf("scoped rereview prompt missing %q:\n%s", want, scoped)
		}
	}
	if strings.Contains(scoped, "Do a full review pass before returning") {
		t.Fatalf("scoped rereview must not demand a full branch pass:\n%s", scoped)
	}

	// Scope applies to rereviews only: the initial review under fix-diff is still full.
	initial := capturePrompt(t, false, config.ReviewRereviewScopeFixDiff)
	if !strings.Contains(initial, "Do a full review pass before returning") {
		t.Fatalf("initial review under rereview_scope: fix-diff must remain a full pass:\n%s", initial)
	}
}
