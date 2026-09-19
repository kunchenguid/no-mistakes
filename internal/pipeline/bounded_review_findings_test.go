package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const boundedReviewFindingsFixture = `{"review_strategy":"bounded","findings":[{"id":"review-1","severity":"error","description":"bug","evidence":"trace","verification":"test one"},{"id":"review-2","severity":"warning","description":"unsupported","evidence":"guess","verification":"inspect caller"},{"id":"review-3","severity":"warning","description":"large redesign","evidence":"design","verification":"product decision"}],"summary":"three"}`

func TestApplyFindingDispositions_RecordsEveryDecisionAndSelectsOnlyConfirmedFixes(t *testing.T) {
	t.Parallel()
	raw, fixIDs, err := applyFindingDispositionsJSON(boundedReviewFindingsFixture, map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
		"review-2": {Decision: types.FindingDispositionReject, Reason: "caller excludes nil"},
		"review-3": {Decision: types.FindingDispositionDefer, Reason: "requires an API redesign"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixIDs) != 1 || fixIDs[0] != "review-1" {
		t.Fatalf("confirmed fix IDs = %v, want [review-1]", fixIDs)
	}
	got, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewStrategy != config.ReviewStrategyBounded {
		t.Fatalf("review strategy = %q", got.ReviewStrategy)
	}
	for i, decision := range []string{types.FindingDispositionFix, types.FindingDispositionReject, types.FindingDispositionDefer} {
		if got.Items[i].Disposition != decision || got.Items[i].DispositionReason == "" {
			t.Fatalf("finding %d disposition = %+v", i, got.Items[i])
		}
	}
	if hasBlockingFindingsJSON(raw) {
		t.Fatal("resolved bounded findings must not keep the gate blocking")
	}
}

func TestApplyFindingDispositions_RequiresCompleteEvidenceBackedDecisionSet(t *testing.T) {
	t.Parallel()
	valid := map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
		"review-2": {Decision: types.FindingDispositionReject, Reason: "not reachable"},
		"review-3": {Decision: types.FindingDispositionDefer, Reason: "outside scope"},
	}
	cases := []struct {
		name, want string
		mutate     func(map[string]types.FindingDisposition)
	}{
		{"missing", "no disposition", func(v map[string]types.FindingDisposition) { delete(v, "review-2") }},
		{"unknown", "unknown finding", func(v map[string]types.FindingDisposition) {
			v["review-9"] = types.FindingDisposition{Decision: types.FindingDispositionReject, Reason: "absent"}
		}},
		{"invalid", "invalid disposition", func(v map[string]types.FindingDisposition) {
			v["review-2"] = types.FindingDisposition{Decision: "improve", Reason: "nice"}
		}},
		{"reason", "requires a reason", func(v map[string]types.FindingDisposition) {
			v["review-2"] = types.FindingDisposition{Decision: types.FindingDispositionReject}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decisions := make(map[string]types.FindingDisposition, len(valid))
			for id, decision := range valid {
				decisions[id] = decision
			}
			tc.mutate(decisions)
			_, _, err := applyFindingDispositionsJSON(boundedReviewFindingsFixture, decisions)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEscalatedFindingRemainsBlocking(t *testing.T) {
	t.Parallel()
	raw, _, err := applyFindingDispositionsJSON(boundedReviewFindingsFixture, map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionReject, Reason: "not reachable"},
		"review-2": {Decision: types.FindingDispositionDefer, Reason: "outside scope"},
		"review-3": {Decision: types.FindingDispositionEscalate, Reason: "security policy decision"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlockingFindingsJSON(raw) {
		t.Fatal("authority finding must remain blocking")
	}
}
