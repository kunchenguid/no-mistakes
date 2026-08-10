package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAllSteps_OnlyReviewDeclaresScopeLimitedFindings pins the wiring the
// executor reads to decide whether a step's unresolved findings carry across
// its fix rounds. Review's rereview is scoped to the fix diff, so its silence
// about an earlier finding is not evidence; every other step re-derives its
// findings from complete current state each round, so a reduced fresh round
// IS evidence and carrying there would re-park the operator on state the
// pipeline can currently disprove (a CI gate reporting a check failing while
// the forge reports it green).
func TestAllSteps_OnlyReviewDeclaresScopeLimitedFindings(t *testing.T) {
	scopeLimited := map[types.StepName]bool{
		types.StepReview: true,
	}

	for _, step := range AllSteps() {
		declared := false
		if scoped, ok := step.(pipeline.ScopeLimitedFindingsStep); ok {
			declared = scoped.FindingsMayBeScopeLimited()
		}
		if want := scopeLimited[step.Name()]; declared != want {
			t.Errorf("step %s declares FindingsMayBeScopeLimited=%v, want %v", step.Name(), declared, want)
		}
	}
}
