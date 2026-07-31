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
// the prompt, and branchHistoryMaxDescriptionChars bounds each entry. A
// long-lived branch accumulates runs, and the whole point of the section is to
// spend fewer tokens than re-deriving the findings would; the first version
// rendered each finding as its full JSON and cost 26KB per review turn.
const (
	branchHistoryMaxFindings         = 25
	branchHistoryMaxDescriptionChars = 160
)

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
// The reviewer fans the review out to per-aspect sub-agents whose prompts it
// composes itself, so the section carries an explicit instruction to pass it
// on: those sub-agents are where the derivation cost is actually paid, and a
// section that reaches only the aggregating agent buys output-side filtering at
// input-side prices.
//
// Returns an empty string when the branch has no prior reviewed run. The
// section begins with two newlines so it appends cleanly to a prompt.
func branchHistoryPromptSection(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil {
		return ""
	}
	history, err := sctx.DB.PreviousBranchStepHistory(sctx.Run.RepoID, sctx.Run.Branch, types.StepReview, sctx.Run.ID)
	if err != nil || history == nil {
		return ""
	}

	grouped, truncated := groupBranchHistoryByDisposition(history)
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
	b.WriteString("Each entry is `id | severity | file:line | summary`; the summary is abbreviated for recognition, so judge the current code rather than the wording.\n")
	b.WriteString("Include this whole section verbatim in the prompt of every review sub-agent you spawn, appended after their brief. They cannot see this prompt, and they are where re-deriving these findings actually costs.\n")
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
//
// Only a run that reached completion can decline or address anything. Review
// auto-fix is off by default, so every gate resolves through a user selection
// and selection_source is "user" on every round of every run - including runs
// that failed or were cancelled before the user finished deciding. Reading
// those as declines told the reviewer to stay quiet about findings nobody ever
// saw; reading their selections as fixes told it a fix landed that the run
// never got to verify. Both degrade to "open", which costs a restatement and
// nothing else.
func groupBranchHistoryByDisposition(history *db.BranchStepHistory) (map[string][]string, int) {
	dispositions := make(map[string]string)
	lines := make(map[string]string)
	var order []string

	runFinished := history.RunStatus == types.RunCompleted

	for _, r := range history.Rounds {
		if r == nil || r.FindingsJSON == nil || strings.TrimSpace(*r.FindingsJSON) == "" {
			continue
		}
		roundFindings := parseBranchHistoryFindings(*r.FindingsJSON)
		selected := selectedFindingIDSet(r.SelectedFindingIDs)
		userDecided := runFinished && selectionSourceValue(r.SelectionSource) == db.RoundSelectionSourceUser

		for _, item := range roundFindings {
			if item.ID == "" {
				continue
			}
			if _, seen := lines[item.ID]; !seen {
				order = append(order, item.ID)
			}
			lines[item.ID] = item.Line

			switch {
			case runFinished && selected[item.ID]:
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

// parseBranchHistoryFindings renders each finding as a single recognition line.
// History serves matching a past finding to current code, not acting on it: the
// fixer gets the full finding from its own selection, and a reviewer that needs
// the argument can re-derive it from the code in front of it.
func parseBranchHistoryFindings(raw string) []roundFindingLine {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	lines := make([]roundFindingLine, 0, len(findings.Items))
	for _, item := range findings.Items {
		location := sanitizePromptText(item.File)
		if location != "" && item.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, item.Line)
		}
		if location == "" {
			location = "-"
		}
		fields := []string{
			sanitizePromptText(item.ID),
			sanitizePromptText(item.Severity),
			location,
			abbreviateFindingDescription(item.Description),
		}
		lines = append(lines, roundFindingLine{ID: item.ID, Line: strings.Join(fields, " | ")})
	}
	return lines
}

// abbreviateFindingDescription keeps the first sentence, which is where
// reviewers put what the finding is, and drops the rationale that follows it.
// A description with no sentence break is cut on the character bound instead,
// so one run-on finding cannot reintroduce the whole cost.
func abbreviateFindingDescription(description string) string {
	clean := sanitizePromptMultilineText(description)
	if clean == "" {
		return "-"
	}
	if end := strings.Index(clean, ". "); end >= 0 && end+1 <= branchHistoryMaxDescriptionChars {
		return clean[:end+1]
	}
	if len(clean) <= branchHistoryMaxDescriptionChars {
		return clean
	}
	return strings.TrimSpace(clean[:branchHistoryMaxDescriptionChars]) + "..."
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
