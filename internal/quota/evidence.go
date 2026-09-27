// Package quota routes the pipeline agent to whichever configured harness has
// the best measured provider quota left.
//
// The problem it solves is operational: a pipeline run that pins one harness
// strands itself when that provider's quota runs out mid-run, and the operator
// has only two remedies, both manual - edit the config and restart the daemon
// in a window with no active run, or wait for the window to reset. The evidence
// needed to choose better already exists on the machine: quota-axi publishes,
// per provider, the remaining percent, a spend-priority signal, and a projected
// runway.
//
// This package is that decision, and nothing else. It reads one quota-axi
// report, applies the same three gates a human dispatcher would apply to an
// agent (is the provider set up and usable, does it fit the reasoning class the
// step needs, does its runway cover the step's budget), and returns either the
// chosen candidate plus the evidence behind it or a per-candidate report saying
// why each one was refused. It never guesses: a candidate with no measurable
// evidence is refused rather than assumed healthy, and a run with no eligible
// candidate fails closed with the report instead of starting on a coin flip.
//
// The vocabulary of the evidence is quota-axi's, deliberately: no-mistakes
// re-states nothing (no percentages of its own, no local runway model), so the
// routing decision stays auditable against the tool that produced it. The one
// piece of judgement this package owns is the step policy in stepclass.go.
package quota

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Class is a coarse reasoning class. It is the vocabulary shared by a step's
// requirement (StepClass) and a provider's attested capability, so the two can
// be compared without inventing a numeric score.
type Class string

const (
	ClassLow    Class = "low"
	ClassMedium Class = "medium"
	ClassHigh   Class = "high"
)

// classRank orders the classes for comparison; an unknown class ranks lowest so
// a value this build does not understand can never satisfy a requirement.
func classRank(c Class) int {
	switch c {
	case ClassLow:
		return 1
	case ClassMedium:
		return 2
	case ClassHigh:
		return 3
	default:
		return 0
	}
}

// ParseClass maps a catalog or configuration spelling onto a Class.
func ParseClass(raw string) (Class, bool) {
	c := Class(strings.ToLower(strings.TrimSpace(raw)))
	if classRank(c) == 0 {
		return "", false
	}
	return c, true
}

// Meets reports whether this class satisfies a requirement.
func (c Class) Meets(required Class) bool { return classRank(c) >= classRank(required) }

// Runway verdicts published by quota-axi. The set is closed here because the
// feasibility gate branches on it explicitly; an unrecognized verdict is treated
// as "unknown", which is refused rather than assumed safe.
const (
	RunwayThroughReset        = "through_reset"
	RunwayProjectedExhaustion = "projected_exhaustion"
	RunwayExhaustedNow        = "exhausted_now"
	RunwayUnknown             = "unknown"
)

// Provider state statuses published by quota-axi.
const providerStateFresh = "fresh"

// ModelEvidence is one catalog model row joined to the provider's quota
// evidence: what the model is, and how much of the provider's allowance is left.
type ModelEvidence struct {
	ID           string
	Label        string
	Intelligence Class
	// Measurable is true when the row carried a joined effective-availability
	// object. A catalog model with no measurable scope (an unauthenticated
	// provider, or one whose windows are not established) answers false.
	Measurable bool
	Scope      string
	// PercentRemaining and SpendPriority are the routing signals. SpendPriority
	// is quota-axi's own selection metric: positive means this scope is on track
	// to reach its reset with paid allowance unused, so spending here reclaims
	// what would otherwise be forfeited, while negative means the scope is
	// already overdrawn against its reset clock.
	PercentRemaining *int
	SpendPriority    *float64
	RunwayStatus     string
	RunwaySeconds    *int
	Confidence       string
}

// Feasible reports whether this row's projected runway covers the budget a step
// may spend. It fails closed: a row whose runway the tool could not project is
// refused, because "we cannot tell" is not evidence that the step will fit.
func (m ModelEvidence) Feasible(budget time.Duration) (bool, string) {
	switch m.RunwayStatus {
	case RunwayThroughReset:
		return true, "runway through reset"
	case RunwayProjectedExhaustion:
		if m.RunwaySeconds == nil || *m.RunwaySeconds <= 0 {
			return false, "projected exhaustion with no usable runway"
		}
		runway := time.Duration(*m.RunwaySeconds) * time.Second
		if runway >= budget {
			return true, fmt.Sprintf("runway %s covers the %s step budget", HumanDuration(runway), HumanDuration(budget))
		}
		return false, fmt.Sprintf("runway %s is shorter than the %s step budget", HumanDuration(runway), HumanDuration(budget))
	case RunwayExhaustedNow:
		return false, "quota exhausted now"
	default:
		return false, "runway unknown"
	}
}

// Standings renders the routing signals of one row as a stable, bounded phrase
// for logs, records and per-candidate reports.
func (m ModelEvidence) Standings() string {
	parts := make([]string, 0, 3)
	if m.PercentRemaining != nil {
		parts = append(parts, fmt.Sprintf("%d%% remaining", *m.PercentRemaining))
	}
	if m.SpendPriority != nil {
		parts = append(parts, fmt.Sprintf("spend priority %s", trimFloat(*m.SpendPriority)))
	}
	if m.RunwayStatus != "" {
		runway := "runway " + m.RunwayStatus
		if m.RunwaySeconds != nil {
			runway += " " + HumanDuration(time.Duration(*m.RunwaySeconds)*time.Second)
		}
		if m.Confidence != "" {
			runway += " (" + m.Confidence + " confidence)"
		}
		parts = append(parts, runway)
	}
	if len(parts) == 0 {
		return "no routing signals"
	}
	return strings.Join(parts, ", ")
}

// betterThan reports whether m outranks other as a place to spend. The order is
// the evidence's own: spend priority first (quota-axi's selection metric), then
// the remaining percentage, then the longer runway. A signal the tool could not
// compute never outranks one it could, so an unmeasurable row only wins against
// an equally unmeasurable one; ties fall back to the configured candidate order,
// which the caller applies.
func (m ModelEvidence) betterThan(other ModelEvidence) bool {
	if cmp := compareOptionalFloat(m.SpendPriority, other.SpendPriority); cmp != 0 {
		return cmp > 0
	}
	if cmp := compareOptionalInt(m.PercentRemaining, other.PercentRemaining); cmp != 0 {
		return cmp > 0
	}
	return compareOptionalInt(m.RunwaySeconds, other.RunwaySeconds) > 0
}

// compareOptionalFloat orders two optional signals with the missing one last.
func compareOptionalFloat(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case *a > *b:
		return 1
	case *a < *b:
		return -1
	default:
		return 0
	}
}

// compareOptionalInt orders two optional signals with the missing one last.
func compareOptionalInt(a, b *int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case *a > *b:
		return 1
	case *a < *b:
		return -1
	default:
		return 0
	}
}

// ProviderEvidence is one provider's slice of the report.
type ProviderEvidence struct {
	Provider string
	State    string
	Stale    bool
	Models   []ModelEvidence
}

// usable reports whether the provider itself is set up and its credential
// usable, which is the eligibility gate's provider half. A provider the tool
// reports as anything other than fresh is refused with the tool's own word for
// it, so an operator reads the real cause (a signed-out CLI, an expired token)
// rather than a generic "not eligible".
func (p ProviderEvidence) usable() (bool, string) {
	if p.State == "" {
		return false, "provider state unknown"
	}
	if p.State != providerStateFresh {
		return false, fmt.Sprintf("provider state %s", p.State)
	}
	if len(p.Models) == 0 {
		return false, "no catalog evidence for this provider"
	}
	return true, ""
}

// measurable reports whether any of the provider's models carries joined quota
// evidence, so a set-up provider with no measurable scope is refused too.
func (p ProviderEvidence) measurable() (bool, string) {
	for _, m := range p.Models {
		if m.Measurable {
			return true, ""
		}
	}
	return false, "no measurable quota scope"
}

// rowsAt returns the models attested at or above the required class, in report
// order. It is the candidate's fit for a step: a provider can serve the step only
// if something in its catalog is at least as capable as the step requires.
func (p ProviderEvidence) rowsAt(required Class) []ModelEvidence {
	rows := make([]ModelEvidence, 0, len(p.Models))
	for _, m := range p.Models {
		if m.Intelligence.Meets(required) {
			rows = append(rows, m)
		}
	}
	return rows
}

// bestAttested returns the provider's highest catalog class.
func (p ProviderEvidence) bestAttested() Class {
	best := Class("")
	for _, m := range p.Models {
		if classRank(m.Intelligence) > classRank(best) {
			best = m.Intelligence
		}
	}
	return best
}

// row returns the provider's evidence for one catalog model id.
func (p ProviderEvidence) row(id string) (ModelEvidence, bool) {
	for _, m := range p.Models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelEvidence{}, false
}

// Report is one parsed quota-axi models report.
type Report struct {
	GeneratedAt    time.Time
	SchemaVersion  int
	CatalogVersion string
	// Providers is keyed by quota-axi provider name. Providers absent from the
	// report have no catalog coverage at all - that is the evidence itself, and
	// Select reports it per candidate rather than treating it as a tool error.
	Providers map[string]ProviderEvidence
}

// ParseReport decodes a `quota-axi models --json` document. It rejects only what
// cannot be evidence at all (not JSON, not an object); a report with no model
// rows is a valid answer meaning "nothing on this machine is catalog-backed".
func ParseReport(data []byte) (Report, error) {
	var raw modelsResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return Report{}, fmt.Errorf("decode quota-axi models report: %w", err)
	}
	report := Report{
		SchemaVersion:  raw.SchemaVersion,
		CatalogVersion: raw.Catalog.Version,
		Providers:      make(map[string]ProviderEvidence, len(raw.Models)),
	}
	if raw.GeneratedAt != "" {
		parsed, err := time.Parse(time.RFC3339, raw.GeneratedAt)
		if err != nil {
			return Report{}, fmt.Errorf("decode quota-axi models report: generatedAt %q: %w", raw.GeneratedAt, err)
		}
		report.GeneratedAt = parsed
	}
	for _, row := range raw.Models {
		provider := strings.TrimSpace(row.Provider)
		if provider == "" {
			continue
		}
		entry := report.Providers[provider]
		entry.Provider = provider
		entry.State = row.State.Status
		entry.Stale = row.State.Stale
		entry.Models = append(entry.Models, row.evidence())
		report.Providers[provider] = entry
	}
	return report, nil
}

// ProviderNames returns the report's provider names in a stable order.
func (r Report) ProviderNames() []string {
	names := make([]string, 0, len(r.Providers))
	for name := range r.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Summary renders a one-line digest of the report, used in decision log lines.
func (r Report) Summary() string {
	if len(r.Providers) == 0 {
		return "no provider evidence"
	}
	parts := make([]string, 0, len(r.Providers))
	for _, name := range r.ProviderNames() {
		entry := r.Providers[name]
		if best := entry.bestAttested(); best != "" {
			parts = append(parts, fmt.Sprintf("%s(%s)", name, best))
		} else {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, " ")
}

type modelsResponse struct {
	GeneratedAt   string `json:"generatedAt"`
	SchemaVersion int    `json:"schemaVersion"`
	Catalog       struct {
		Version string `json:"version"`
	} `json:"catalog"`
	Models []modelRow `json:"models"`
}

type modelRow struct {
	Provider     string   `json:"provider"`
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Intelligence string   `json:"intelligence"`
	QuotaScopes  []string `json:"quotaScopes"`
	State        struct {
		Status string `json:"status"`
		Stale  bool   `json:"stale"`
	} `json:"state"`
	Effective *struct {
		Scope            string `json:"scope"`
		Status           string `json:"status"`
		PercentRemaining *int   `json:"effectivePercentRemaining"`
		Runway           struct {
			Status         string `json:"status"`
			UsableSeconds  *int   `json:"usableRunwaySeconds"`
			Confidence     string `json:"projectionConfidence"`
			LimitingWindow string `json:"limitingWindowId"`
		} `json:"runway"`
		Selection struct {
			Status        string   `json:"status"`
			SpendPriority *float64 `json:"spendPriority"`
		} `json:"selection"`
	} `json:"effective"`
}

func (row modelRow) evidence() ModelEvidence {
	evidence := ModelEvidence{
		ID:    row.ID,
		Label: row.Label,
	}
	if class, ok := ParseClass(row.Intelligence); ok {
		evidence.Intelligence = class
	}
	if row.Effective == nil {
		return evidence
	}
	evidence.Measurable = true
	evidence.Scope = row.Effective.Scope
	evidence.PercentRemaining = row.Effective.PercentRemaining
	if row.Effective.Selection.Status == "known" {
		evidence.SpendPriority = row.Effective.Selection.SpendPriority
	}
	evidence.RunwayStatus = row.Effective.Runway.Status
	evidence.RunwaySeconds = row.Effective.Runway.UsableSeconds
	evidence.Confidence = row.Effective.Runway.Confidence
	if evidence.RunwayStatus == "" {
		evidence.RunwayStatus = RunwayUnknown
	}
	return evidence
}

// HumanDuration renders a duration for operator-facing text: coarse enough to
// stay readable in a one-line decision, exact enough to compare with a budget.
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// trimFloat renders a priority without trailing zeros, so an uninteresting
// value does not dominate the recorded evidence.
func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

// stepNames keeps the policy table honest: every step the pipeline can run must
// have a class, and this compile-time list is what the test asserts against.
var stepNames = []types.StepName{
	types.StepIntent,
	types.StepRebase,
	types.StepReview,
	types.StepTest,
	types.StepDocument,
	types.StepLint,
	types.StepPush,
	types.StepPR,
	types.StepCI,
}
