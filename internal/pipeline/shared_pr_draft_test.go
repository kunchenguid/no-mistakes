package pipeline

import "testing"

// The CI step's draft->ready publish is gated on this handoff, so "no PR step
// recorded anything" must stay distinguishable from "the PR is not a draft":
// the first has to fail safe toward publishing, the second toward skipping.
func TestRunSharedPRDraftStateDistinguishesUnknownFromPublished(t *testing.T) {
	t.Parallel()

	var absent *RunShared
	if _, known := absent.PRDraftState(); known {
		t.Fatal("a nil RunShared must report the draft state as unknown")
	}

	shared := &RunShared{}
	if _, known := shared.PRDraftState(); known {
		t.Fatal("an untouched RunShared must report the draft state as unknown")
	}

	shared.SetPRDraftState(false)
	isDraft, known := shared.PRDraftState()
	if !known || isDraft {
		t.Fatalf("PRDraftState() = (%v, %v), want (false, true)", isDraft, known)
	}

	shared.SetPRDraftState(true)
	isDraft, known = shared.PRDraftState()
	if !known || !isDraft {
		t.Fatalf("PRDraftState() = (%v, %v), want (true, true)", isDraft, known)
	}
}
