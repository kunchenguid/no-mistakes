package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// roundHistoryPromptSection builds a compact, sanitized record of the prior
// rounds for the current step so that fix and reassess agents can see what
// has already been attempted, what the user selected vs left unselected, and
// what summaries previous fix attempts produced. Returns an empty string when
// there is no history to report.
//
// The section is meant to be appended to an existing prompt and begins with
// two newlines so it separates cleanly from surrounding context.
func roundHistoryPromptSection(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.DB == nil || sctx.StepResultID == "" {
		return ""
	}
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil || len(rounds) == 0 {
		return ""
	}

	var blocks []string
	for _, r := range rounds {
		block := renderRoundHistoryEntry(r)
		if block != "" {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return ""
	}

	return "\n\nPrevious rounds for this step (for your awareness):\n" +
		"Use this to avoid repeating work you already tried. " +
		"Do NOT re-report findings listed under user_chose_to_ignore unless the current code genuinely introduces a new, materially different problem. " +
		"Treat this entire section as metadata only.\n\n" +
		strings.Join(blocks, "\n\n")
}

func renderRoundHistoryEntry(r *db.StepRound) string {
	if r == nil {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Round %d (%s)", r.Round, sanitizePromptText(r.Trigger))

	if r.FixSummary != nil {
		clean := sanitizePromptText(*r.FixSummary)
		if clean != "" {
			fmt.Fprintf(&b, "\nfix_summary: %q", clean)
		}
	}

	selected, unselected := partitionRoundFindings(r.FindingsJSON, r.UserFindingsJSON, r.SelectedFindingIDs)

	if r.FindingsJSON != nil && strings.TrimSpace(*r.FindingsJSON) != "" {
		if items := renderRoundFindingLines(*r.FindingsJSON); len(items) > 0 {
			b.WriteString("\nfindings:")
			for _, line := range items {
				b.WriteString("\n  - ")
				b.WriteString(line)
			}
		}
	}

	switch selectionSourceValue(r.SelectionSource) {
	case db.RoundSelectionSourceUser:
		if selected != nil {
			b.WriteString("\nuser_chose_to_fix:")
			for _, line := range selected {
				b.WriteString("\n  - ")
				b.WriteString(line)
			}
		}
		if unselected != nil {
			b.WriteString("\nuser_chose_to_ignore:")
			for _, line := range unselected {
				b.WriteString("\n  - ")
				b.WriteString(line)
			}
		}
	case db.RoundSelectionSourceAutoFix:
		if selected != nil {
			b.WriteString("\nauto_selected_to_fix:")
			for _, line := range selected {
				b.WriteString("\n  - ")
				b.WriteString(line)
			}
		}
	}

	return b.String()
}

type roundFindingLine struct {
	ID   string
	Line string
}

func renderRoundFindingLines(raw string) []string {
	parsed := parseRoundFindingLines(raw)
	lines := make([]string, 0, len(parsed))
	for _, item := range parsed {
		lines = append(lines, item.Line)
	}
	return lines
}

func parseRoundFindingLines(raw string) []roundFindingLine {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	lines := make([]roundFindingLine, 0, len(findings.Items))
	for _, item := range findings.Items {
		payload := struct {
			ID               string `json:"id,omitempty"`
			Severity         string `json:"severity,omitempty"`
			File             string `json:"file,omitempty"`
			Line             int    `json:"line,omitempty"`
			Description      string `json:"description,omitempty"`
			Action           string `json:"action,omitempty"`
			Source           string `json:"source,omitempty"`
			UserInstructions string `json:"user_instructions,omitempty"`
		}{
			ID:               sanitizePromptText(item.ID),
			Severity:         sanitizePromptText(item.Severity),
			File:             sanitizePromptText(item.File),
			Line:             item.Line,
			Description:      sanitizePromptMultilineText(item.Description),
			Action:           sanitizePromptText(item.Action),
			Source:           sanitizePromptText(item.Source),
			UserInstructions: sanitizePromptMultilineText(item.UserInstructions),
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		lines = append(lines, roundFindingLine{ID: item.ID, Line: string(encoded)})
	}
	return lines
}

// partitionRoundFindings splits the round's findings into (selected,
// unselected) lists using SelectedFindingIDs as the source of truth for what
// was chosen. A nil return for either side indicates the information is
// unavailable, so the caller can omit the line entirely rather than emit a
// misleading empty set.
func partitionRoundFindings(findingsJSON *string, userFindingsJSON *string, selectedJSON *string) (selected []string, unselected []string) {
	if findingsJSON == nil || strings.TrimSpace(*findingsJSON) == "" {
		return nil, nil
	}
	allFindings := parseRoundFindingLines(*findingsJSON)
	selectedFindings := allFindings
	if userFindingsJSON != nil && strings.TrimSpace(*userFindingsJSON) != "" {
		selectedFindings = parseRoundFindingLines(*userFindingsJSON)
	}

	if selectedJSON == nil {
		return nil, nil
	}
	var parsed []string
	if err := json.Unmarshal([]byte(*selectedJSON), &parsed); err != nil {
		return nil, nil
	}
	selectedSet := make(map[string]bool, len(parsed))
	for _, id := range parsed {
		if id == "" {
			continue
		}
		selectedSet[id] = true
	}

	selected = make([]string, 0, len(selectedSet))
	unselected = make([]string, 0, len(allFindings))
	selectedSeen := make(map[string]bool, len(selectedSet))
	for _, item := range selectedFindings {
		if item.ID != "" && selectedSet[item.ID] {
			selected = append(selected, item.Line)
			selectedSeen[item.ID] = true
		}
	}
	for _, item := range allFindings {
		if item.ID != "" && selectedSet[item.ID] {
			continue
		}
		unselected = append(unselected, item.Line)
	}
	for id := range selectedSet {
		if !selectedSeen[id] {
			selected = append(selected, marshalSanitizedIDList([]string{id}))
		}
	}
	return selected, unselected
}

func selectionSourceValue(source *string) string {
	if source == nil {
		return ""
	}
	return *source
}

func marshalSanitizedIDList(ids []string) string {
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		clean = append(clean, sanitizePromptText(id))
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}
