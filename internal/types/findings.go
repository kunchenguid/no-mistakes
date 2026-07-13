package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Finding action constants.
const (
	ActionNoOp    = "no-op"
	ActionAutoFix = "auto-fix"
	ActionAskUser = "ask-user"
)

// Finding source constants. An empty Source is treated as agent-produced.
const (
	FindingSourceAgent = "agent"
	FindingSourceUser  = "user"
)

// Finding category constants for the combined document+lint housekeeping
// pass. An empty Category on a housekeeping finding is treated as
// documentation (the stricter gate).
const (
	FindingCategoryDocumentation = "documentation"
	FindingCategoryLint          = "lint"
)

// Finding represents a single review, test, lint, or PR comment finding.
type Finding struct {
	ID               string `json:"id,omitempty"`
	Severity         string `json:"severity"`
	File             string `json:"file,omitempty"`
	Line             int    `json:"line,omitempty"`
	Description      string `json:"description"`
	Action           string `json:"action"`
	Source           string `json:"source,omitempty"`
	UserInstructions string `json:"user_instructions,omitempty"`
	// Category separates the combined document+lint housekeeping pass's
	// findings into their owning gates. Empty everywhere else.
	Category string `json:"category,omitempty"`
}

// TestArtifact describes evidence produced by the test step for human review.
type TestArtifact struct {
	Kind    string `json:"kind,omitempty"`
	Label   string `json:"label"`
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
	Content string `json:"content,omitempty"`
}

type findingWire struct {
	ID                  string `json:"id,omitempty"`
	Severity            string `json:"severity"`
	File                string `json:"file,omitempty"`
	Line                int    `json:"line,omitempty"`
	Description         string `json:"description"`
	Action              string `json:"action"`
	Source              string `json:"source,omitempty"`
	UserInstructions    string `json:"user_instructions,omitempty"`
	Category            string `json:"category,omitempty"`
	RequiresHumanReview *bool  `json:"requires_human_review,omitempty"`
}

// Findings is the structured findings payload exchanged across pipeline, IPC, and TUI.
type Findings struct {
	Items          []Finding      `json:"findings"`
	Summary        string         `json:"summary"`
	Tested         []string       `json:"tested,omitempty"`
	TestingSummary string         `json:"testing_summary,omitempty"`
	Artifacts      []TestArtifact `json:"artifacts,omitempty"`
	RiskLevel      string         `json:"risk_level"`
	RiskRationale  string         `json:"risk_rationale"`
}

type findingsWire struct {
	Items          []Finding      `json:"findings"`
	Legacy         []Finding      `json:"items"`
	Summary        string         `json:"summary"`
	Tested         []string       `json:"tested"`
	TestingSummary string         `json:"testing_summary"`
	Artifacts      []TestArtifact `json:"artifacts"`
	RiskLevel      string         `json:"risk_level"`
	RiskRationale  string         `json:"risk_rationale"`
}

// ParseFindingsJSON decodes findings JSON, accepting current and legacy item
// keys plus legacy requires_human_review fields.
func ParseFindingsJSON(raw string) (Findings, error) {
	var wire findingsWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return Findings{}, err
	}
	items := wire.Items
	if len(items) == 0 && len(wire.Legacy) > 0 {
		items = wire.Legacy
	}
	return Findings{Items: items, Summary: wire.Summary, Tested: wire.Tested, TestingSummary: wire.TestingSummary, Artifacts: wire.Artifacts, RiskLevel: wire.RiskLevel, RiskRationale: wire.RiskRationale}, nil
}

// NormalizeFindings assigns deterministic IDs to findings that do not have one yet.
func NormalizeFindings(findings Findings, prefix string) Findings {
	for i := range findings.Items {
		if findings.Items[i].ID != "" {
			continue
		}
		findings.Items[i].ID = prefix + "-" + itoa(i+1)
	}
	return findings
}

// FilterFindings keeps only findings whose IDs are included in ids.
func FilterFindings(findings Findings, ids []string) Findings {
	if len(ids) == 0 {
		return findings
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	filtered := Findings{Summary: findings.Summary, Tested: findings.Tested, TestingSummary: findings.TestingSummary, Artifacts: findings.Artifacts, RiskLevel: findings.RiskLevel, RiskRationale: findings.RiskRationale}
	for _, item := range findings.Items {
		if selected[item.ID] {
			filtered.Items = append(filtered.Items, item)
		}
	}
	if len(filtered.Items) != len(findings.Items) {
		filtered.Summary = summarizeSelectedFindings(len(filtered.Items))
	}
	return filtered
}

// ExcludeFindings keeps only findings whose IDs are NOT in the excluded set.
func ExcludeFindings(findings Findings, ids []string) Findings {
	if len(ids) == 0 {
		return findings
	}
	excluded := make(map[string]bool, len(ids))
	for _, id := range ids {
		excluded[id] = true
	}
	result := Findings{Summary: findings.Summary, Tested: findings.Tested, TestingSummary: findings.TestingSummary, Artifacts: findings.Artifacts, RiskLevel: findings.RiskLevel, RiskRationale: findings.RiskRationale}
	for _, item := range findings.Items {
		if !excluded[item.ID] {
			result.Items = append(result.Items, item)
		}
	}
	return result
}

// AutoFixableFindings returns a new Findings containing only items where
// Action is "auto-fix". These are safe for automatic fixing without
// user involvement.
func AutoFixableFindings(findings Findings) Findings {
	result := Findings{Summary: findings.Summary, Tested: findings.Tested, TestingSummary: findings.TestingSummary, Artifacts: findings.Artifacts, RiskLevel: findings.RiskLevel, RiskRationale: findings.RiskRationale}
	for _, item := range findings.Items {
		if item.actionOrDefault() == ActionAutoFix {
			result.Items = append(result.Items, item)
		}
	}
	return result
}

// MergeUserOverrides applies per-finding user instructions to existing agent
// findings and appends user-added findings at the end. Added findings have
// Source stamped to FindingSourceUser and receive deterministic "user-N" IDs
// if they do not carry an ID. The original Findings is not mutated.
func MergeUserOverrides(findings Findings, instructions map[string]string, added []Finding) Findings {
	result := Findings{
		Summary:        findings.Summary,
		Tested:         findings.Tested,
		TestingSummary: findings.TestingSummary,
		Artifacts:      findings.Artifacts,
		RiskLevel:      findings.RiskLevel,
		RiskRationale:  findings.RiskRationale,
	}
	if len(findings.Items) > 0 {
		result.Items = make([]Finding, len(findings.Items))
		copy(result.Items, findings.Items)
	}
	for i := range result.Items {
		if note, ok := instructions[result.Items[i].ID]; ok {
			result.Items[i].UserInstructions = note
		}
	}
	used := make(map[string]bool, len(result.Items)+len(added))
	for _, item := range result.Items {
		if item.ID != "" {
			used[item.ID] = true
		}
	}
	counter := 0
	appended := false
	for _, item := range added {
		item.Source = FindingSourceUser
		if item.Action == "" {
			item.Action = ActionAutoFix
		}
		if item.ID == "" || used[item.ID] {
			item.ID, counter = nextUserFindingID(used, counter)
		} else {
			used[item.ID] = true
		}
		result.Items = append(result.Items, item)
		appended = true
	}
	if appended && isSelectedFindingsSummary(result.Summary) {
		result.Summary = summarizeSelectedFindings(len(result.Items))
	}
	return result
}

// HasAskUserFindings returns true if any finding has an effective action of
// "ask-user". It uses actionOrDefault so an empty/missing action (which now
// defaults to ask-user) parks for a human, keeping this in agreement with
// AutoFixableFindings: an unclassified finding is never auto-fixed and is
// always caught here as ask-user.
func HasAskUserFindings(findings Findings) bool {
	for _, item := range findings.Items {
		if item.actionOrDefault() == ActionAskUser {
			return true
		}
	}
	return false
}

// HasActionableFindings reports whether any finding warrants a fix - that is,
// any finding whose effective action is not "no-op". Findings that are purely
// informational ("no-op") are not actionable and need no fix, so a step whose
// findings are all no-op (or that has no findings) returns false. This is what
// yolo / auto-resolve uses to decide whether to fix a gate's findings or just
// accept the step as-is.
func HasActionableFindings(findings Findings) bool {
	for _, item := range findings.Items {
		if item.actionOrDefault() != ActionNoOp {
			return true
		}
	}
	return false
}

func summarizeSelectedFindings(count int) string {
	switch count {
	case 0:
		return "0 selected findings"
	case 1:
		return "1 selected finding"
	default:
		return fmt.Sprintf("%d selected findings", count)
	}
}

func nextUserFindingID(used map[string]bool, counter int) (string, int) {
	for {
		counter++
		candidate := "user-" + itoa(counter)
		if used[candidate] {
			continue
		}
		used[candidate] = true
		return candidate, counter
	}
}

func isSelectedFindingsSummary(summary string) bool {
	if summary == "0 selected findings" || summary == "1 selected finding" {
		return true
	}
	if !strings.HasSuffix(summary, " selected findings") {
		return false
	}
	count := strings.TrimSuffix(summary, " selected findings")
	if count == "" {
		return false
	}
	for _, r := range count {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MarshalFindingsJSON encodes findings using the current wire shape.
func MarshalFindingsJSON(findings Findings) (string, error) {
	raw, err := json.Marshal(findings)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func (f *Finding) UnmarshalJSON(data []byte) error {
	var wire findingWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	f.ID = wire.ID
	f.Severity = wire.Severity
	f.File = wire.File
	f.Line = wire.Line
	f.Description = wire.Description
	f.Action = wire.Action
	f.Source = wire.Source
	f.UserInstructions = wire.UserInstructions
	f.Category = wire.Category
	if f.Action == "" && wire.RequiresHumanReview != nil {
		if *wire.RequiresHumanReview {
			f.Action = ActionAskUser
		} else {
			f.Action = ActionAutoFix
		}
	}
	return nil
}

// actionOrDefault resolves a finding's effective action, defaulting an
// empty/missing action to ask-user (park), not auto-fix. This closes a
// fail-open hole: an unclassified finding on a non-schema path (a legacy
// requires_human_review omission, an IPC- or user-supplied finding) must
// route to a human rather than be silently auto-applied. It also matches the
// review prompt's own "When in doubt, default to ask-user" instruction.
// (MergeUserOverrides still stamps user-*added* findings auto-fix explicitly -
// a user who hand-adds a finding is asking for a fix.)
func (f Finding) actionOrDefault() string {
	if f.Action == "" {
		return ActionAskUser
	}
	return f.Action
}
