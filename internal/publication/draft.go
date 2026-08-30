package publication

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var publicationDefenseSteps = []types.StepName{
	types.StepIntent,
	types.StepRebase,
	types.StepReview,
	types.StepTest,
	types.StepDocument,
	types.StepLint,
}

var draftAbsolutePathPattern = regexp.MustCompile(`(?i)(^|[[:space:]"'(=:\[])(?:[a-z]:[\\/]|/)[^[:space:]<>"')\]]+`)

// RenderPRDraft renders the inspectable, marker-free PR body from durable
// publication state. The Manager remains the sole owner of appending the
// reconciliation marker and persisting the finalized draft bytes.
func (m *Manager) RenderPRDraft(_ context.Context, publicationID string) ([]byte, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return nil, err
	}
	result, err := m.resultFor(publication, run)
	if err != nil {
		return nil, err
	}
	if result.Status != StatusReadyForPR {
		return nil, fmt.Errorf("publication status is %s, want %s", result.Status, StatusReadyForPR)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return nil, fmt.Errorf("parse stored publication request: %w", err)
	}
	steps, err := m.db.GetStepsByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("read durable publication steps: %w", err)
	}
	return renderPRDraftBody(parsed, publication, steps)
}

func renderPRDraftBody(parsed ParsedRequest, publication *db.Publication, steps []*db.StepResult) ([]byte, error) {
	if publication == nil {
		return nil, fmt.Errorf("publication is required")
	}
	if parsed.PublicationID != publication.PublicationID ||
		!bytes.Equal(parsed.CanonicalBytes, publication.CanonicalRequest) {
		return nil, fmt.Errorf("canonical publication request does not match durable publication")
	}

	defenses, err := exactDefenseSummary(publication.RunID, steps)
	if err != nil {
		return nil, err
	}

	request := parsed.Request
	var body strings.Builder
	body.WriteString("# Protected Factory publication\n\n")
	body.WriteString("This pull request publishes one content-addressed Factory candidate after the protected defense chain completed.\n\n")
	body.WriteString("## Exact bindings\n\n")
	writeDraftBinding(&body, "Publication", publication.PublicationID)
	writeDraftBinding(&body, "Publication run", publication.RunID)
	writeDraftBinding(&body, "Factory run", request.Factory.RunID)
	writeDraftBinding(&body, "Factory terminal T10 sequence", strconv.FormatInt(request.Factory.TerminalT10Sequence, 10))
	writeDraftBinding(&body, "Factory run-state prefix SHA-256", request.Factory.RunStatePrefixSHA256)
	writeDraftBinding(&body, "Factory PlanBinding SHA-256", request.Factory.PlanBindingSHA256)
	writeDraftBinding(&body, "Candidate commit (H)", request.Candidate.CommitSHA)
	writeDraftBinding(&body, "Candidate tree", request.Candidate.TreeSHA)
	writeDraftBinding(&body, "Base commit", request.Candidate.BaseSHA)
	writeDraftBinding(&body, "WorkContract path", request.WorkContract.Path)
	writeDraftBinding(&body, "WorkContract raw-byte SHA-256", request.WorkContract.SHA256)

	body.WriteString("\n## Build intent\n\n")
	writeDraftText(&body, request.BuildIntent.Summary)
	body.WriteString("\n### Acceptance criteria\n")
	for index, criterion := range request.BuildIntent.AcceptanceCriteria {
		body.WriteString("\nCriterion ")
		body.WriteString(strconv.Itoa(index + 1))
		body.WriteString(":\n\n")
		writeDraftText(&body, criterion)
	}

	body.WriteString("\n## Durable defense summary\n\n")
	body.WriteString("| Step | Status | Exit code |\n")
	body.WriteString("| --- | --- | ---: |\n")
	for _, defense := range defenses {
		body.WriteString("| ")
		body.WriteString(string(defense.StepName))
		body.WriteString(" | ")
		body.WriteString(string(defense.Status))
		body.WriteString(" | ")
		body.WriteString(strconv.Itoa(*defense.ExitCode))
		body.WriteString(" |\n")
	}

	return []byte(body.String()), nil
}

func exactDefenseSummary(runID string, steps []*db.StepResult) ([]*db.StepResult, error) {
	if len(steps) != len(types.AllSteps()) {
		return nil, fmt.Errorf("durable publication step records are incomplete")
	}
	byName := make(map[types.StepName]*db.StepResult, len(steps))
	for _, step := range steps {
		if step == nil || step.RunID != runID || step.StepName.Order() == 0 || step.StepName.Order() != step.StepOrder {
			return nil, fmt.Errorf("durable publication step records do not match their run and canonical order")
		}
		if _, exists := byName[step.StepName]; exists {
			return nil, fmt.Errorf("duplicate durable publication step %s", step.StepName)
		}
		byName[step.StepName] = step
	}
	for _, name := range types.AllSteps() {
		if byName[name] == nil {
			return nil, fmt.Errorf("durable publication step %s is missing", name)
		}
	}

	defenses := make([]*db.StepResult, 0, len(publicationDefenseSteps))
	for _, name := range publicationDefenseSteps {
		step := byName[name]
		if step == nil || step.Status != types.StepStatusCompleted || step.ExitCode == nil || *step.ExitCode != 0 {
			return nil, fmt.Errorf("durable publication defense %s is not an exact successful completion", name)
		}
		defenses = append(defenses, step)
	}
	return defenses, nil
}

func writeDraftBinding(body *strings.Builder, label, value string) {
	body.WriteString("- ")
	body.WriteString(label)
	body.WriteString(": <code>")
	body.WriteString(sanitizeDraftText(value))
	body.WriteString("</code>\n")
}

func writeDraftText(body *strings.Builder, value string) {
	body.WriteString("<pre>")
	body.WriteString(sanitizeDraftText(value))
	body.WriteString("</pre>\n")
}

func sanitizeDraftText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = safeurl.RedactText(value)
	// Publication rendering must remain byte-stable across daemon restarts, so
	// path redaction cannot consult the current process's HOME or user lookup.
	// This deliberately conservative rule replaces every path-shaped token.
	value = draftAbsolutePathPattern.ReplaceAllString(value, "${1}~")
	return html.EscapeString(value)
}
