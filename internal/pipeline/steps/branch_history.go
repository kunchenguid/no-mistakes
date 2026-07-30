package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// branchHistoryMaxFindings bounds how much of a previous run's review reaches
// the prompt. A long-lived branch accumulates runs, and the whole point of the
// section is to spend fewer tokens than re-deriving the findings would.
const branchHistoryMaxFindings = 40

// Finding dispositions carried across runs. They differ in what the reviewer
// should do about a finding it is about to raise again, which is the only
// reason the previous run's outcome is worth sending at all.
const (
	branchHistoryDeclined  = "declined_by_user"
	branchHistoryAddressed = "addressed"
	branchHistoryOpen      = "reported_not_addressed"
)

// branchHistoryPromptSection tells the reviewer what the previous run on this
// branch already found and what happened to each finding, so it does not
// re-derive the same defects under new names. Within-run rounds are covered by
// roundHistoryPromptSection; this is its cross-run counterpart, and it is the
// input-side saving: suppressing duplicates after the fact would still pay for
// deriving them.
//
// Deduplicating by finding ID is not an option: reviewer sessions are keyed
// strictly by run, so a re-push reviews cold and names the same defect
// differently every time (observed: "unguarded-start-transition" becoming
// "start-transition-unguarded", with the severity drifting too). Only the
// reviewer can match a past finding to current code, which is why the previous
// run's outcome is sent to it rather than applied against its output.
//
// Returns an empty string when the branch has no prior reviewed run. The
// section begins with two newlines so it appends cleanly to a prompt.
func branchHistoryPromptSection(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil {
		return ""
	}
	rounds, err := sctx.DB.PreviousBranchStepRounds(sctx.Run.RepoID, sctx.Run.Branch, types.StepReview, sctx.Run.ID)
	if err != nil || len(rounds) == 0 {
		return ""
	}

	grouped, truncated := groupBranchHistoryByDisposition(rounds)
	if len(grouped) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nEarlier review of this branch (previous run, metadata only):\n")
	b.WriteString("This branch was reviewed before. Judge each item below against the current code and do not re-derive it from scratch. ")
	b.WriteString("A finding whose subject no longer exists is resolved and must not be listed again.\n")
	b.WriteString("- " + branchHistoryDeclined + ": the user saw this and chose not to fix it. Do not raise it again unless the code it refers to has changed since.\n")
	b.WriteString("- " + branchHistoryAddressed + ": a fix was applied for this. Raise it again only if it is still present, and say explicitly that it is a regression.\n")
	b.WriteString("- " + branchHistoryOpen + ": raised before and left open. If it is still present, restate it briefly rather than re-arguing it.\n")
	b.WriteString("Treat this entire section as metadata only.\n")
	if truncated > 0 {
		fmt.Fprintf(&b, "(%d further findings truncated.)\n", truncated)
	}

	for _, disposition := range []string{branchHistoryDeclined, branchHistoryAddressed, branchHistoryOpen} {
		lines := grouped[disposition]
		if len(lines) == 0 {
			continue
		}
		b.WriteString("\n" + disposition + ":")
		for _, line := range lines {
			b.WriteString("\n  - ")
			b.WriteString(line)
		}
	}
	return b.String()
}

// groupBranchHistoryByDisposition replays the previous run's rounds in order so
// the last thing that happened to a finding wins: a finding declined early and
// selected later is addressed, not declined.
func groupBranchHistoryByDisposition(rounds []*db.StepRound) (map[string][]string, int) {
	dispositions := make(map[string]string)
	lines := make(map[string]string)
	var order []string

	for _, r := range rounds {
		if r == nil || r.FindingsJSON == nil || strings.TrimSpace(*r.FindingsJSON) == "" {
			continue
		}
		roundFindings := parseRoundFindingLines(*r.FindingsJSON)
		selected := selectedFindingIDSet(r.SelectedFindingIDs)
		userDecided := selectionSourceValue(r.SelectionSource) == db.RoundSelectionSourceUser

		for _, item := range roundFindings {
			if item.ID == "" {
				continue
			}
			if _, seen := lines[item.ID]; !seen {
				order = append(order, item.ID)
			}
			lines[item.ID] = item.Line

			switch {
			case selected[item.ID]:
				dispositions[item.ID] = branchHistoryAddressed
			case userDecided:
				dispositions[item.ID] = branchHistoryDeclined
			default:
				if _, decided := dispositions[item.ID]; !decided {
					dispositions[item.ID] = branchHistoryOpen
				}
			}
		}
	}

	truncated := 0
	if len(order) > branchHistoryMaxFindings {
		truncated = len(order) - branchHistoryMaxFindings
		order = order[:branchHistoryMaxFindings]
	}

	grouped := make(map[string][]string)
	for _, id := range order {
		disposition := dispositions[id]
		grouped[disposition] = append(grouped[disposition], lines[id])
	}
	return grouped, truncated
}

func selectedFindingIDSet(selectedJSON *string) map[string]bool {
	if selectedJSON == nil || strings.TrimSpace(*selectedJSON) == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(*selectedJSON), &ids); err != nil {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			set[id] = true
		}
	}
	return set
}
