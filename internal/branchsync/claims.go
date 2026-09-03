package branchsync

import "strings"

// SettlementGateMoveQualifier is the condition under which the keep-local
// custody return actually moves the gate branch. Every path that returns
// before recoverKeepLocal's swap leaves the gate untouched: the equal/ahead
// branch stamps custody directly, recoverSettleInconsistent returns early when
// the gate branch is PROVEN ABSENT or no gate is configured, and
// recoverKeepLocal skips its whole block when the gate already names the kept
// head. A record whose gate branch was deleted is admitted by the
// advertisement predicates on purpose, so a surface promising the settlement
// points the gate branch anywhere is telling that operator something false.
const SettlementGateMoveQualifier = "still names a different head"

// settlementGateMovePromises are the ways a surface states that promise. The
// list lives beside the code whose behavior makes the promise conditional, not
// beside any one surface, because the drift this guards against was never
// confined to a single file: a correction applied to the four cobra help
// strings left the identical claim standing on the structured branch_sync
// error, the TUI confirmation, the agent guidance, the skill and the agents
// guide.
var settlementGateMovePromises = []string{
	"points the gate branch at",
	"point the gate branch at",
	"points that branch at",
	"point that branch at",
	"points the gate branch to",
	"moves the gate branch to",
	"move the gate branch to",
	"moving the gate branch to",
	"moving the local gate branch to",
	"moving that branch to",
	"compare-and-swaps onto the kept head",
	"the gate follows the kept head",
	"points it at the kept head",
}

// UnqualifiedGateMovePromise returns the first sentence of text that promises
// the keep-local custody return moves the gate branch onto the kept head
// without conditioning it on SettlementGateMoveQualifier, or "" when the
// invariant holds. Sentences are the unit because the qualifier has to travel
// with the promise an operator reads, not merely appear somewhere in the same
// document.
func UnqualifiedGateMovePromise(text string) string {
	for _, sentence := range claimSentences(text) {
		promised := false
		for _, promise := range settlementGateMovePromises {
			if strings.Contains(sentence, promise) {
				promised = true
				break
			}
		}
		if !promised || strings.Contains(sentence, SettlementGateMoveQualifier) {
			continue
		}
		return sentence
	}
	return ""
}

// claimSentences normalizes whitespace before splitting so a claim stays one
// sentence across hard-wrapped help text, TUI box lines, and Go string
// concatenation.
func claimSentences(text string) []string {
	var out []string
	for _, part := range strings.Split(strings.Join(strings.Fields(text), " "), ". ") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
