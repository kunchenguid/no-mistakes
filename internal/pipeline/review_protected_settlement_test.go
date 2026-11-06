package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFilterSettledReviewFindings_ProtectsSelectedSingletonFromBothDispositions(t *testing.T) {
	for _, source := range []string{db.RoundSelectionSourceUserIgnored, db.RoundSelectionSourceExternal} {
		t.Run(source, func(t *testing.T) {
			a := types.Finding{ID: "A", File: "first.go", Line: 10, Severity: types.FindingSeverityWarning, Description: "missing check", Action: types.ActionAskUser}
			b := a
			b.ID, b.Line = "B", 20
			encode := func(items ...types.Finding) string {
				t.Helper()
				raw, err := types.MarshalFindingsJSON(types.Findings{
					Items:               items,
					ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			pair := encode(a, b)
			selected := `["B"]`
			user := db.RoundSelectionSourceUser
			a.ID, a.Line = "A-moved", 11
			singletonA := encode(a)
			a.UserInstructions = "verified outside the worktree"
			resolved := encode(a)
			empty := db.DeclinedSelectionJSON
			rounds := []*db.StepRound{
				{FindingsJSON: &pair, SelectedFindingIDs: &selected, SelectionSource: &user},
				{FindingsJSON: &singletonA, SelectedFindingIDs: &empty, SelectionSource: &source, UserFindingsJSON: &resolved},
			}
			if got := SettledReviewFindingCount(rounds); got != 1 {
				t.Fatalf("settled content count = %d, want 1", got)
			}
			for _, items := range [][]types.Finding{{a, b}, {b}} {
				raw := encode(items...)
				for pass := 0; pass < 2; pass++ {
					filtered, err := filterSettledReviewFindings(raw, rounds)
					if err != nil {
						t.Fatal(err)
					}
					got, err := types.ParseFindingsJSON(filtered)
					if err != nil {
						t.Fatal(err)
					}
					if len(got.Items) != 1 || got.Items[0].ID != "B" {
						t.Fatalf("pass %d: settlement removed the selected twin or retained A: %+v", pass, got.Items)
					}
					raw = filtered
				}
			}
		})
	}
}
