package cli

import (
	"fmt"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// maxFindingDesc caps a finding description rendered inline. Findings are the
// decision content at a gate, so the limit is generous; only pathological
// descriptions get truncated, with the full length disclosed.
const maxFindingDesc = 600

// Row types carry `toon` tags so the encoder renders a []row slice as a
// tabular array (name[N]{cols}:) with one comma-delimited line per element.
type stepRow struct {
	Step       string `toon:"step"`
	Status     string `toon:"status"`
	Findings   int    `toon:"findings"`
	DurationMS int64  `toon:"duration_ms"`
}

type findingRow struct {
	ID          string `toon:"id"`
	Severity    string `toon:"severity"`
	File        string `toon:"file"`
	Action      string `toon:"action"`
	Description string `toon:"description"`
}

type runRow struct {
	ID     string `toon:"id"`
	Branch string `toon:"branch"`
	Status string `toon:"status"`
	Head   string `toon:"head"`
	PR     string `toon:"pr"`
}

// logRow is a single log line; a []logRow renders as a block array so multiline
// logs stay readable rather than collapsing onto one inline row.
type logRow struct {
	Line string `toon:"line"`
}

// fixRow is one fix the pipeline applied: the step it ran under and the
// agent's one-line summary of the change.
type fixRow struct {
	Step    string `toon:"step"`
	Summary string `toon:"summary"`
}

// stepView is a render-ready view of a single pipeline step, decoupled from
// whether it came from the daemon (ipc) or the local database.
type stepView struct {
	Name         string
	Status       string
	DurationMS   int64
	FindingsJSON string
	FixSummaries []string
}

// runView is a render-ready view of a pipeline run.
type runView struct {
	ID      string
	Branch  string
	Status  string
	HeadSHA string
	PRURL   string
	Steps   []stepView
}

func runViewFromIPC(r *ipc.RunInfo) runView {
	rv := runView{
		ID:      r.ID,
		Branch:  r.Branch,
		Status:  string(r.Status),
		HeadSHA: r.HeadSHA,
	}
	if r.PRURL != nil {
		rv.PRURL = *r.PRURL
	}
	for _, s := range r.Steps {
		sv := stepView{Name: string(s.StepName), Status: string(s.Status), FixSummaries: s.FixSummaries}
		if s.DurationMS != nil {
			sv.DurationMS = *s.DurationMS
		}
		if s.FindingsJSON != nil {
			sv.FindingsJSON = *s.FindingsJSON
		}
		rv.Steps = append(rv.Steps, sv)
	}
	return rv
}

func runViewFromDB(r *db.Run, steps []*db.StepResult) runView {
	rv := runView{
		ID:      r.ID,
		Branch:  r.Branch,
		Status:  string(r.Status),
		HeadSHA: r.HeadSHA,
	}
	if r.PRURL != nil {
		rv.PRURL = *r.PRURL
	}
	for _, s := range steps {
		sv := stepView{Name: string(s.StepName), Status: string(s.Status)}
		if s.DurationMS != nil {
			sv.DurationMS = *s.DurationMS
		}
		if s.FindingsJSON != nil {
			sv.FindingsJSON = *s.FindingsJSON
		}
		rv.Steps = append(rv.Steps, sv)
	}
	return rv
}

// awaitingStep returns the step currently blocking on a human decision, if any.
// At most one step awaits at a time, so the first match is the active gate.
func (rv runView) awaitingStep() (stepView, bool) {
	for _, s := range rv.Steps {
		if s.Status == string(types.StepStatusAwaitingApproval) || s.Status == string(types.StepStatusFixReview) {
			return s, true
		}
	}
	return stepView{}, false
}

// shortSHA trims a commit SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// findingCount returns the number of findings recorded for a step.
func (s stepView) findingCount() int {
	if s.FindingsJSON == "" {
		return 0
	}
	parsed, err := types.ParseFindingsJSON(s.FindingsJSON)
	if err != nil {
		return 0
	}
	return len(parsed.Items)
}

// findingsTally summarizes a run's findings across all steps by action, so an
// agent sees the shape of outstanding work without a follow-up call.
func (rv runView) findingsTally() string {
	var awaiting, autofix, info int
	for _, s := range rv.Steps {
		if s.FindingsJSON == "" {
			continue
		}
		parsed, err := types.ParseFindingsJSON(s.FindingsJSON)
		if err != nil {
			continue
		}
		for _, f := range parsed.Items {
			switch f.Action {
			case types.ActionAskUser:
				awaiting++
			case types.ActionAutoFix:
				autofix++
			default:
				info++
			}
		}
	}
	parts := make([]string, 0, 3)
	if awaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting", awaiting))
	}
	if autofix > 0 {
		parts = append(parts, fmt.Sprintf("%d auto-fix", autofix))
	}
	if info > 0 {
		parts = append(parts, fmt.Sprintf("%d info", info))
	}
	if len(parts) == 0 {
		return "none"
	}
	return joinComma(parts)
}

// fixRows flattens the fixes the pipeline applied across all steps into
// renderable rows, in step then round order. A fix round that recorded no
// summary still produced a fix commit, so it gets an explicit placeholder
// rather than being dropped.
func (rv runView) fixRows() []fixRow {
	var rows []fixRow
	for _, s := range rv.Steps {
		for _, summary := range s.FixSummaries {
			if summary == "" {
				summary = "fix applied (no summary recorded)"
			}
			rows = append(rows, fixRow{Step: s.Name, Summary: summary})
		}
	}
	return rows
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// runObjectField renders a run as a TOON "run:" object with a steps table.
func runObjectField(rv runView) toon.Field {
	fields := []toon.Field{
		{Key: "id", Value: rv.ID},
		{Key: "branch", Value: rv.Branch},
		{Key: "status", Value: rv.Status},
		{Key: "head", Value: shortSHA(rv.HeadSHA)},
	}
	if rv.PRURL != "" {
		fields = append(fields, toon.Field{Key: "pr", Value: rv.PRURL})
	}
	fields = append(fields, toon.Field{Key: "findings", Value: rv.findingsTally()})

	rows := make([]stepRow, 0, len(rv.Steps))
	for _, s := range rv.Steps {
		rows = append(rows, stepRow{Step: s.Name, Status: s.Status, Findings: s.findingCount(), DurationMS: s.DurationMS})
	}
	fields = append(fields, toon.Field{Key: "steps", Value: rows})
	return toon.Field{Key: "run", Value: toon.NewObject(fields...)}
}

// gateFields renders the active approval gate: the awaiting step, its findings
// table, and the next-step commands an agent can run to clear it.
func gateFields(gate stepView) []toon.Field {
	parsed, _ := types.ParseFindingsJSON(gate.FindingsJSON)
	gfields := []toon.Field{
		{Key: "step", Value: gate.Name},
		{Key: "status", Value: gate.Status},
	}
	if parsed.Summary != "" {
		gfields = append(gfields, toon.Field{Key: "summary", Value: parsed.Summary})
	}
	if parsed.RiskLevel != "" {
		gfields = append(gfields, toon.Field{Key: "risk", Value: parsed.RiskLevel})
	}
	rows := make([]findingRow, 0, len(parsed.Items))
	for _, f := range parsed.Items {
		rows = append(rows, findingRow{
			ID:          f.ID,
			Severity:    f.Severity,
			File:        f.File,
			Action:      f.Action,
			Description: truncate(f.Description, maxFindingDesc),
		})
	}
	gfields = append(gfields, toon.Field{Key: "findings", Value: rows})

	return []toon.Field{
		{Key: "gate", Value: toon.NewObject(gfields...)},
		{Key: "help", Value: []string{
			"Run `no-mistakes axi respond --action approve` to accept this step and continue",
			"Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself)",
			"Run `no-mistakes axi respond --action skip` to skip this step",
			fmt.Sprintf("Run `no-mistakes axi logs --step %s --full` to read the full step log", gate.Name),
		}},
	}
}

// truncate shortens s to limit runes, appending a disclosure of the full size
// when it actually trims, per the AXI content-truncation convention.
func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + fmt.Sprintf("… (truncated, %d chars total)", len(runes))
}

// --- output helpers ---

// axiDoc marshals an ordered set of TOON fields into a document with a trailing
// newline. Encoding errors are impossible for the value shapes we build here,
// so a failure degrades to an empty document rather than propagating.
func axiDoc(fields ...toon.Field) string {
	out, err := toon.MarshalString(toon.NewObject(fields...))
	if err != nil {
		return ""
	}
	return out + "\n"
}

// emitDoc writes a finished TOON document to stdout.
func emitDoc(cmd *cobra.Command, fields ...toon.Field) {
	fmt.Fprint(cmd.OutOrStdout(), axiDoc(fields...))
}

// emitError renders a structured TOON error to stdout and returns an exitError
// so the process exits non-zero without cobra printing the Go error.
func emitError(cmd *cobra.Command, code int, msg string, help ...string) error {
	fields := []toon.Field{{Key: "error", Value: msg}}
	if len(help) > 0 {
		fields = append(fields, toon.Field{Key: "help", Value: help})
	}
	emitDoc(cmd, fields...)
	return &exitError{code: code}
}
