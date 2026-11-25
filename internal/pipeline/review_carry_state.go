package pipeline

import "github.com/kunchenguid/no-mistakes/internal/db"

// CarriedPendingVerificationIDs names the findings in a Review gate payload
// that an earlier round already dispatched to the fixer and no later round has
// positively verified yet. They are carried state, not fresh defects: a status
// surface that counts them as live work reads a converging gate as a growing
// one (the 2026-11-25 pingap stall's "18 findings" were mostly this).
//
// The derivation is the same content-identity retention recovery uses
// (retainFindingIDsByIdentity): an ID counts only while the gate's finding
// still matches the content recorded when it was selected, so a re-minted
// positional ID never aliases an unrelated finding as carried.
func CarriedPendingVerificationIDs(gate string, rounds []*db.StepRound) []string {
	if gate == "" || len(rounds) == 0 {
		return nil
	}
	var selected []string
	for _, round := range rounds {
		if round.SelectedFindingIDs != nil {
			selected = combineFindingIDLists(selected, findingIDsFromSelectionJSON(*round.SelectedFindingIDs))
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return retainFindingIDsByIdentity(gate, selected, selectedFindingIdentities(rounds))
}
