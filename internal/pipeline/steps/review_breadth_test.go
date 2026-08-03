package steps

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The review loop's cost is dominated by the per-aspect sub-agents
// comprehensive-code-review spawns, so a later round has to narrow the aspect
// list to actually save anything: filtering the aggregated findings happens
// after every one of those agents has already run.
func TestReviewBreadthForRound_NarrowsAsRoundsAccumulate(t *testing.T) {
	tests := []struct {
		name        string
		round       int
		narrowAfter int
		wantAspects []string
		wantMin     string
	}{
		{"first round is a full sweep", 1, 2, nil, ""},
		{"threshold round is still full", 2, 2, nil, ""},
		{"past threshold drops advisory aspects", 3, 2, coreReviewAspects, "warning"},
		{"double threshold is the last core round", 4, 2, coreReviewAspects, "warning"},
		{"past double threshold keeps only defects", 5, 2, minimalReviewAspects, "error"},
		{"threshold scales", 5, 4, coreReviewAspects, "warning"},
		{"zero disables narrowing", 9, 0, nil, ""},
		{"negative disables narrowing", 9, -1, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewBreadthForRound(tt.round, tt.narrowAfter)
			if strings.Join(got.Aspects, ",") != strings.Join(tt.wantAspects, ",") {
				t.Errorf("Aspects = %v, want %v", got.Aspects, tt.wantAspects)
			}
			if got.MinSeverity != tt.wantMin {
				t.Errorf("MinSeverity = %q, want %q", got.MinSeverity, tt.wantMin)
			}
		})
	}
}

// A full sweep must keep the exact skill invocation the review step has always
// used, so narrowing cannot silently change round one.
func TestReviewBreadthPromptSections_FullSweepIsUnchanged(t *testing.T) {
	b := reviewBreadthForRound(1, 2)
	if got := b.skillInvocation(); got != fullSweepSkillInvocation {
		t.Errorf("skillInvocation() = %q, want %q", got, fullSweepSkillInvocation)
	}
	if got := b.severityRule(); got != "" {
		t.Errorf("severityRule() = %q, want empty", got)
	}
}

// The narrowed invocation must name the aspects (the skill runs only named
// aspects) and the floor must forbid generating the findings, not just
// reporting them - generation is where the tokens go.
func TestReviewBreadthPromptSections_NarrowedNamesAspectsAndFloor(t *testing.T) {
	b := reviewBreadthForRound(3, 2)

	invocation := b.skillInvocation()
	for _, aspect := range coreReviewAspects {
		if !strings.Contains(invocation, aspect) {
			t.Errorf("skillInvocation() = %q, missing aspect %q", invocation, aspect)
		}
	}
	if strings.Contains(invocation, "no arguments") {
		t.Errorf("skillInvocation() = %q, must not still claim no arguments", invocation)
	}
	// The invocation is interpolated mid-sentence, so the aspect list must be
	// closed: with an open list the prose that follows reads as one more aspect
	// name, and the aspect names are the one string narrowing depends on the
	// agent parsing exactly.
	last := coreReviewAspects[len(coreReviewAspects)-1]
	if !strings.Contains(invocation, "- "+strings.Join(coreReviewAspects, ", ")+" -") {
		t.Errorf("skillInvocation() = %q, want the aspect list delimited on both sides", invocation)
	}
	if strings.HasSuffix(invocation, last) {
		t.Errorf("skillInvocation() = %q, want the list closed after %q", invocation, last)
	}

	rule := b.severityRule()
	if !strings.Contains(rule, "warning") {
		t.Errorf("severityRule() = %q, want it to name the warning floor", rule)
	}
	if !strings.Contains(rule, "info") {
		t.Errorf("severityRule() = %q, want it to name the excluded severity", rule)
	}
	if !strings.Contains(rule, "Do not spend review effort") {
		t.Errorf("severityRule() = %q, want it to suppress generation, not just reporting", rule)
	}
}

// The warning-floor rule also names all three severities, so this asserts the
// phrase that distinguishes the top rung: the error floor must fail if it ever
// regresses into the warning floor.
func TestReviewBreadthPromptSections_ErrorFloorExcludesWarningAndInfo(t *testing.T) {
	rule := reviewBreadthForRound(5, 2).severityRule()
	if !strings.Contains(rule, `Report only "error" findings`) {
		t.Errorf("severityRule() = %q, want it to report only error findings", rule)
	}
	if strings.Contains(rule, `and "warning" findings`) {
		t.Errorf("severityRule() = %q, must not still admit warning findings", rule)
	}
	if !strings.Contains(rule, `Do not spend review effort looking for "warning" or "info"`) {
		t.Errorf("severityRule() = %q, want it to suppress warning and info generation", rule)
	}
}

// Narrowing names aspects, and the review skill runs ONLY the aspects it is
// given, so every list must keep the 3a Spec/Standards track: it is the axis
// that checks the change against the author's intent, and late rounds are
// exactly when that matters most. Narrowing may only drop 3b spawns.
func TestReviewBreadthForRound_NarrowedRoundsKeepSpecConformance(t *testing.T) {
	for _, round := range []int{3, 4, 5, 9, 40} {
		b := reviewBreadthForRound(round, 2)
		if len(b.Aspects) == 0 {
			t.Fatalf("round %d: expected a narrowed aspect list", round)
		}
		if !slices.Contains(b.Aspects, specConformanceAspect) {
			t.Errorf("round %d: Aspects = %v, want it to keep %q", round, b.Aspects, specConformanceAspect)
		}
		if !strings.Contains(b.skillInvocation(), specConformanceAspect) {
			t.Errorf("round %d: skillInvocation() = %q, want it to name %q", round, b.skillInvocation(), specConformanceAspect)
		}
	}
}

// Breadth only saves anything if the narrowed invocation and the severity floor
// actually reach the agent, so this runs ReviewStep end to end against recorded
// round history rather than the renderer alone: the round the step derives from
// the database is what selects the breadth.
func TestReviewStep_NarrowsThePromptOnceRoundsPassTheThreshold(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, recordedRounds int) string {
		t.Helper()
		dir, baseSHA, headSHA := setupGitRepo(t)

		findingsJSON, _ := json.Marshal(Findings{Summary: "clean"})
		ag := &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: findingsJSON}, nil
			},
		}

		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.Config.Review.NarrowAfterRound = 2
		sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
		if err != nil {
			t.Fatal(err)
		}
		sctx.StepResultID = sr.ID
		prior := `{"findings":[],"summary":"clean"}`
		for i := 1; i <= recordedRounds; i++ {
			if _, err := sctx.DB.InsertStepRound(sctx.StepResultID, i, "initial", &prior, nil, 10); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		if len(ag.calls) != 1 {
			t.Fatalf("expected 1 review call, got %d", len(ag.calls))
		}
		return ag.calls[0].Prompt
	}

	t.Run("round at the threshold stays a full sweep", func(t *testing.T) {
		t.Parallel()
		prompt := run(t, 1)
		if !strings.Contains(prompt, fullSweepSkillInvocation) {
			t.Errorf("prompt lost the full-sweep invocation:\n%s", prompt)
		}
		if strings.Contains(prompt, "naming exactly these aspects") {
			t.Errorf("prompt narrowed the aspect list on round 2 of narrow_after_round=2:\n%s", prompt)
		}
		if strings.Contains(prompt, "later round of an ongoing review") {
			t.Errorf("prompt applied a severity floor before the threshold:\n%s", prompt)
		}
	})

	t.Run("round past the threshold narrows aspects and floor", func(t *testing.T) {
		t.Parallel()
		prompt := run(t, 2)
		if strings.Contains(prompt, fullSweepSkillInvocation) {
			t.Errorf("prompt kept the full-sweep invocation past the threshold:\n%s", prompt)
		}
		for _, aspect := range coreReviewAspects {
			if !strings.Contains(prompt, aspect) {
				t.Errorf("prompt missing narrowed aspect %q:\n%s", aspect, prompt)
			}
		}
		if !strings.Contains(prompt, reviewBreadthForRound(3, 2).severityRule()) {
			t.Errorf("prompt missing the warning floor:\n%s", prompt)
		}
	})
}

// A namespaced skill name (plugin or directory-scoped install) still satisfies
// the mandate; anything else does not, and an adapter that reports no skill use
// at all is unobservable rather than a violation.
func TestReviewSkillMandateSatisfied_MatchesNamespacedSkillNames(t *testing.T) {
	tests := []struct {
		name  string
		skill []string
		want  bool
	}{
		{"unreported skills are unobservable", nil, true},
		{"bare name", []string{requiredReviewSkill}, true},
		{"plugin scoped", []string{"plugin:" + requiredReviewSkill}, true},
		{"directory scoped", []string{"apps/web:" + requiredReviewSkill}, true},
		{"among others", []string{"tdd", "plugin:" + requiredReviewSkill}, true},
		{"empty list is a violation", []string{}, false},
		{"other skills only", []string{"tdd", "code-review"}, false},
		{"suffix of another skill is not a match", []string{"not-comprehensive-code-review"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewSkillMandateSatisfied(&agent.Result{SkillsUsed: tt.skill})
			if got != tt.want {
				t.Errorf("reviewSkillMandateSatisfied(%v) = %v, want %v", tt.skill, got, tt.want)
			}
		})
	}
}
