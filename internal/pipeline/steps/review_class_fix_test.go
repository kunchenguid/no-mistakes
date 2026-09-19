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
)

// The review-fix contract's unit of work is the invariant a finding violates,
// at every site in the changed area where it must hold, not the one reported
// instance. The superseded "fix the reported instance narrowly" rule produced
// the thrash it was measured against: on SSHHIP PR #462, 11 of 17 rereviews
// reported the sibling site the previous fix had left behind (the other clamp
// axis, the Skip path beside the Fix path, the same __proto__ map in a second
// file, the next unvalidated field of one response) or a regression the fix
// itself made. The fragments below are the rendered fixer contract; the
// reviewer-side class-once rule and the rereview's follow-on labelling are
// pinned by their own tests in this file.
var (
	fixerClassRuleLines = []string{
		"- Before changing code, state for each finding the invariant it violates (what must always hold, in one sentence) and enumerate every place in the changed area where that same invariant must hold: every axis, direction, and representation; every sibling call path, command, action, and state transition; every consumer of the same input, field, or record. Fix the invariant at all of those places in this round, with the same small correction, or at the one shared boundary that makes all of them hold. A fix that closes only the reported site and leaves a sibling site reachable is incomplete; the next review will report the sibling.",
		"- Do not grow the fix into machinery: closing sibling sites with the same small edit, or moving a check to one shared boundary, is the fix; adding handling, state, fallbacks, retries, or a subsystem to manage symptoms is not. Prefer addressing a deeper architectural reason and simplifying it, than introducing machinery to handle the symptoms.",
		"- After applying the fixes and before verification, re-trace for each finding the concrete failing sequence it describes through the code as it now is, and trace the ordinary successful path through every function you changed, including each of its callers. Remove any alias, branch, parameter, or helper your fix made unreachable. A fix that makes the reported sequence pass while breaking the ordinary path, a caller's assumption, or a sibling site is a regression the next review will report.",
	}
	fixerInvariantContract = []string{
		"Before changing code, state for each finding the invariant it violates (what must always hold, in one sentence) and enumerate every place in the changed area where that same invariant must hold",
		"every axis, direction, and representation; every sibling call path, command, action, and state transition; every consumer of the same input, field, or record",
		"Fix the invariant at all of those places in this round, with the same small correction, or at the one shared boundary that makes all of them hold",
		"A fix that closes only the reported site and leaves a sibling site reachable is incomplete; the next review will report the sibling",
		"Do not grow the fix into machinery: closing sibling sites with the same small edit, or moving a check to one shared boundary, is the fix; adding handling, state, fallbacks, retries, or a subsystem to manage symptoms is not",
	}
	fixerSelfTraceContract = []string{
		"After applying the fixes and before verification, re-trace for each finding the concrete failing sequence it describes through the code as it now is, and trace the ordinary successful path through every function you changed, including each of its callers",
		"Remove any alias, branch, parameter, or helper your fix made unreachable",
		"A fix that makes the reported sequence pass while breaking the ordinary path, a caller's assumption, or a sibling site is a regression the next review will report",
	}
	// Neither superseded scope rule may return: the instance-narrow rule bred
	// sibling follow-ons, and the deepest-practical-cause rule bred machinery.
	fixerSupersededScopeRules = []string{
		"Fix the reported instance narrowly",
		"fix the deepest practical cause instead",
	}
	reviewerClassOnceContract = []string{
		"When you report a defect, enumerate in that same finding every other place in the changed code where the same invariant is violated or must hold",
		"another axis, direction, or representation; a sibling call path, command, action, or state transition; another consumer of the same input, field, or record",
		"each as file:line with a few words",
		"Report the class once, anchored at the primary site, instead of one site now and its siblings after the next fix",
		"When the defect is incomplete validation of an input, response, or record, list every consumed field that is still unvalidated in that one finding",
	}
	rereviewFollowOnContract = []string{
		"When a defect you report is in code a prior fix round changed, or is a sibling site of an invariant a prior fix round addressed, say so in the description",
		"name the round, and whether that fix introduced the defect, left this sibling behind, or moved the defect",
		"List every remaining sibling site so one fix round can close the class",
	}
)

func promptHasExactLine(prompt, want string) bool {
	for _, line := range strings.Split(prompt, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// TestReviewStep_FixPromptClosesTheInvariantAcrossSiblingSites pins the
// fixer's invariant-complete rule as rendered: state the invariant, enumerate
// every sibling site, close all of them in this round with the same small
// correction or one shared boundary, and never as machinery. The self-trace
// rule sits between "apply all fixes first" and the single focused
// verification, so the trace is of the code as edited and precedes any test
// run. The rereview never receives the fixer's contract.
func TestReviewStep_FixPromptClosesTheInvariantAcrossSiblingSites(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
			}
			j, _ := json.Marshal(cleanReviewFindings())
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"error","file":"pane.go","line":12,"description":"Resize clamps the requested width to the terminal but not the height; the same bound is missing for rows","action":"auto-fix"}],"summary":"1 issue"}`

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("expected fix + rereview calls, got %d", len(ag.calls))
	}
	fixPrompt := ag.calls[0].Prompt
	for _, want := range append(append([]string{}, fixerInvariantContract...), fixerSelfTraceContract...) {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("review fix prompt missing invariant-complete contract %q:\n%s", want, fixPrompt)
		}
	}
	for _, stale := range fixerSupersededScopeRules {
		if strings.Contains(fixPrompt, stale) {
			t.Errorf("review fix prompt still carries the superseded scope rule %q:\n%s", stale, fixPrompt)
		}
	}
	applyFirst := strings.Index(fixPrompt, "Apply all the fixes you intend to make first")
	trace := strings.Index(fixPrompt, fixerSelfTraceContract[0])
	verify := strings.Index(fixPrompt, "After all fixes are applied, run one focused verification")
	if applyFirst < 0 || trace < 0 || verify < 0 || !(applyFirst < trace && trace < verify) {
		t.Errorf("self-trace rule must sit between applying all fixes and the single verification (apply=%d trace=%d verify=%d):\n%s", applyFirst, trace, verify, fixPrompt)
	}
	// The rereview judges the class; it is never told how to fix it.
	rereviewPrompt := ag.calls[1].Prompt
	for _, fixerOnly := range []string{fixerInvariantContract[0], fixerSelfTraceContract[0]} {
		if strings.Contains(rereviewPrompt, fixerOnly) {
			t.Errorf("rereview prompt must not carry the fixer's contract %q:\n%s", fixerOnly, rereviewPrompt)
		}
	}
}

// TestReviewStep_ReviewPromptReportsTheClassOnce pins the reviewer's
// class-once rule: a defect finding lists, in that same finding, every other
// site in the changed code where the same invariant is violated or must hold,
// and an incomplete-validation finding lists every consumed field still
// unvalidated. The anchor stays one file and line, so the carry-forward set
// and finding identity are unchanged. Both the initial review and the rereview
// carry the rule; the fixer does not.
func TestReviewStep_ReviewPromptReportsTheClassOnce(t *testing.T) {
	t.Parallel()

	t.Run("initial_review", func(t *testing.T) {
		t.Parallel()
		dir, baseSHA, headSHA := setupGitRepo(t)

		findingsJSON, _ := json.Marshal(cleanReviewFindings())
		ag := &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: findingsJSON}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		prompt := ag.calls[0].Prompt
		for _, want := range reviewerClassOnceContract {
			if !strings.Contains(prompt, want) {
				t.Errorf("review prompt missing class-once rule %q:\n%s", want, prompt)
			}
		}
		if !strings.Contains(prompt, "Anchor every finding to a specific file and one-indexed line number") {
			t.Errorf("review prompt lost the single-anchor rule the class-once rule extends:\n%s", prompt)
		}
		// Listing siblings is a reviewer obligation; the reviewer is still
		// not told to fix anything.
		if strings.Contains(prompt, fixerInvariantContract[0]) {
			t.Errorf("review prompt must not carry the fixer's invariant-complete rule:\n%s", prompt)
		}
	})

	t.Run("rereview_after_fix_round_and_not_the_fixer", func(t *testing.T) {
		t.Parallel()
		dir, baseSHA, headSHA := setupGitRepo(t)
		gitCmd(t, dir, "checkout", "--detach", headSHA)

		callCount := 0
		ag := &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				callCount++
				if callCount == 1 {
					os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0o644)
					return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
				}
				j, _ := json.Marshal(cleanReviewFindings())
				return &agent.Result{Output: j}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.Fixing = true
		sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","description":"possible nil deref","action":"auto-fix"}],"summary":"1 issue"}`

		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		if len(ag.calls) != 2 {
			t.Fatalf("expected fix + rereview calls, got %d", len(ag.calls))
		}
		for _, want := range reviewerClassOnceContract {
			if !strings.Contains(ag.calls[1].Prompt, want) {
				t.Errorf("rereview prompt missing class-once rule %q:\n%s", want, ag.calls[1].Prompt)
			}
			if strings.Contains(ag.calls[0].Prompt, want) {
				t.Errorf("fixer prompt must not carry the reviewer's class-once rule %q:\n%s", want, ag.calls[0].Prompt)
			}
		}
	})
}

// TestReviewStep_RereviewNamesFollowOnsOfPriorFixRounds pins the rereview's
// follow-on labelling: a defect in code a prior fix round changed, or a
// sibling site of an invariant a prior round addressed, is reported as such,
// naming the round and whether the fix introduced, left behind, or moved the
// defect, with every remaining sibling listed so one round can close the
// class. It rides both provenance framings (this run's fix rounds and a
// previous run's uncertified fixer commits), because both are prior-round
// code, and is absent from an ordinary initial review and from the fixer.
func TestReviewStep_RereviewNamesFollowOnsOfPriorFixRounds(t *testing.T) {
	t.Parallel()

	t.Run("rereview_after_this_runs_fix_round", func(t *testing.T) {
		t.Parallel()
		dir, baseSHA, headSHA := setupGitRepo(t)
		gitCmd(t, dir, "checkout", "--detach", headSHA)

		callCount := 0
		ag := &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				callCount++
				if callCount == 1 {
					os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0o644)
					return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
				}
				j, _ := json.Marshal(cleanReviewFindings())
				return &agent.Result{Output: j}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.Fixing = true
		sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","description":"possible nil deref","action":"auto-fix"}],"summary":"1 issue"}`

		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		if len(ag.calls) != 2 {
			t.Fatalf("expected fix + rereview calls, got %d", len(ag.calls))
		}
		rereviewPrompt := ag.calls[1].Prompt
		for _, want := range rereviewFollowOnContract {
			if !strings.Contains(rereviewPrompt, want) {
				t.Errorf("rereview prompt missing follow-on labelling %q:\n%s", want, rereviewPrompt)
			}
			if strings.Contains(ag.calls[0].Prompt, want) {
				t.Errorf("fixer prompt must not carry the rereview's follow-on labelling %q:\n%s", want, ag.calls[0].Prompt)
			}
		}
		// The labelling extends the provenance framing; it does not replace
		// the revert exit ramp that follows it.
		if !strings.Contains(rereviewPrompt, "Fix-round provenance:") {
			t.Errorf("rereview prompt lost the provenance framing the labelling extends:\n%s", rereviewPrompt)
		}
		if !strings.Contains(rereviewPrompt, `report a single "ask-user" finding recommending that the prior round be reverted to the minimal fix`) {
			t.Errorf("rereview prompt lost the revert exit ramp:\n%s", rereviewPrompt)
		}
	})

	t.Run("initial_review_over_uncertified_prior_run_commits", func(t *testing.T) {
		t.Parallel()
		dir, baseSHA, headSHA := setupGitRepo(t)

		findingsJSON, _ := json.Marshal(cleanReviewFindings())
		ag := &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: findingsJSON}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.UncertifiedFromSHA = "from-sha"
		sctx.UncertifiedToSHA = "to-sha"
		sctx.UncertifiedSourceRunID = "prior-run"

		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		for _, want := range rereviewFollowOnContract {
			if !strings.Contains(ag.calls[0].Prompt, want) {
				t.Errorf("uncertified-range review missing follow-on labelling %q:\n%s", want, ag.calls[0].Prompt)
			}
		}
	})

	t.Run("ordinary_initial_review_has_no_prior_round_to_name", func(t *testing.T) {
		t.Parallel()
		dir, baseSHA, headSHA := setupGitRepo(t)

		findingsJSON, _ := json.Marshal(cleanReviewFindings())
		ag := &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: findingsJSON}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		for _, stale := range rereviewFollowOnContract {
			if strings.Contains(ag.calls[0].Prompt, stale) {
				t.Errorf("a review with no prior-round code must not carry follow-on labelling %q:\n%s", stale, ag.calls[0].Prompt)
			}
		}
	})
}

// The class-fix corpus is an executable unified-diff contract, so each fixture
// must remain consumable by the documented git-based evaluation flow.
func TestReviewStep_ClassFixFixturesApply(t *testing.T) {
	t.Parallel()

	type baseline struct{ path, content string }
	cases := []struct {
		name      string
		baselines []baseline
	}{
		{
			name: "proto map in two files",
			baselines: []baseline{
				{path: "src/registry.js", content: "\"use strict\";\n"},
				{path: "src/aliases.js", content: "\"use strict\";\n"},
			},
		},
		{
			name:      "partial response validation",
			baselines: []baseline{{path: "transport/hello.go", content: "package transport\n"}},
		},
		{
			name:      "two axis clamp",
			baselines: []baseline{{path: "pane/pane.go", content: "package pane\n"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, b := range tc.baselines {
				path := filepath.Join(dir, b.path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(b.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, dir, "init", "-q")
			gitCmd(t, dir, "config", "core.autocrlf", "false")
			fixture, err := os.ReadFile(filepath.Join("testdata", "class_fix_review", strings.ReplaceAll(tc.name, " ", "-")+".diff"))
			if err != nil {
				t.Fatal(err)
			}
			fixturePath := filepath.Join(dir, "fixture.diff")
			fixture = []byte(strings.ReplaceAll(string(fixture), "\r\n", "\n"))
			if err := os.WriteFile(fixturePath, fixture, 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "apply", "--check", fixturePath)
		})
	}
}
