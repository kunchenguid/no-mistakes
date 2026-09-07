package types

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Finding action constants.
const (
	ActionNoOp    = "no-op"
	ActionAutoFix = "auto-fix"
	ActionAskUser = "ask-user"
)

// Finding severity constants: the vocabulary the review prompt instructs
// agents to use, ordered most to least severe.
const (
	FindingSeverityError   = "error"
	FindingSeverityWarning = "warning"
	FindingSeverityInfo    = "info"
)

// This package owns the finding severity and action vocabularies. Callers that
// accept a severity or action from outside a pipeline agent - a hand-written
// eval miss, an IPC payload - validate against these rather than keeping their
// own copy of the list.
var (
	knownFindingSeverities = []string{FindingSeverityError, FindingSeverityWarning, FindingSeverityInfo}
	knownFindingActions    = []string{ActionAutoFix, ActionAskUser, ActionNoOp}
)

// NormalizeFindingSeverity trims and lower-cases one severity so equivalent
// spellings compare equal. It does not check membership; see
// IsKnownFindingSeverity.
func NormalizeFindingSeverity(severity string) string {
	return strings.ToLower(strings.TrimSpace(severity))
}

// NormalizeFindingAction trims and lower-cases one action. It does not check
// membership; see IsKnownFindingAction.
func NormalizeFindingAction(action string) string {
	return strings.ToLower(strings.TrimSpace(action))
}

// IsKnownFindingSeverity reports whether severity, once normalized, is part of
// the review severity vocabulary.
func IsKnownFindingSeverity(severity string) bool {
	return slices.Contains(knownFindingSeverities, NormalizeFindingSeverity(severity))
}

// IsKnownFindingAction reports whether action, once normalized, is part of the
// finding action vocabulary.
func IsKnownFindingAction(action string) bool {
	return slices.Contains(knownFindingActions, NormalizeFindingAction(action))
}

// KnownFindingSeverities returns the severity vocabulary, for error messages
// that have to name what they accept.
func KnownFindingSeverities() []string { return slices.Clone(knownFindingSeverities) }

// KnownFindingActions returns the action vocabulary, for error messages that
// have to name what they accept.
func KnownFindingActions() []string { return slices.Clone(knownFindingActions) }

// Finding source constants. An empty Source is treated as agent-produced.
const (
	FindingSourceAgent = "agent"
	FindingSourceUser  = "user"
)

const (
	FindingReviewScopeSource                = "source"
	FindingReviewScopePipelineOwnedDelivery = "pipeline-owned-delivery"
	FindingReviewScopeExternalDelivery      = "external-delivery"
)

const (
	FindingsRiskScopeSourceOrExternal      = "source-or-external"
	FindingsRiskScopePipelineOwnedDelivery = "pipeline-owned-delivery"
)

// Finding category constants for the combined document+lint housekeeping
// pass. An empty Category on a housekeeping finding is treated as
// documentation (the stricter gate).
const (
	FindingCategoryDocumentation = "documentation"
	FindingCategoryLint          = "lint"
)

// Finding category constants for the CI step's check findings. The CI step
// turns each settled issue on the pull request into one finding and the fix
// half routes by this category: a check finding names its provider check in
// Finding.Check, a merge-conflict finding asks for a rebase, a transient
// finding is a provider-attributed outcome no code change can clear, and a
// review-bot finding carries one unresolved comment from a third-party
// review bot's check.
const (
	FindingCategoryCICheck         = "ci-check"
	FindingCategoryCIMergeConflict = "ci-merge-conflict"
	FindingCategoryCITransient     = "ci-transient"
	FindingCategoryCIReviewBot     = "ci-review-bot"
)

// Test scenario result constants: the vocabulary the test step's evidence
// prompt instructs the agent to use for each derived scenario.
//
// ScenarioResultUntested is the honest answer for a scenario this machine
// could not drive against the real product - either the change has no live
// product surface, or a required tool, credential, permission, or authority
// is unavailable. It is reported on the pull request and never blocks by
// itself; the run's verdict determines whether that scenario coverage parks
// the step.
const (
	ScenarioResultPass     = "pass"
	ScenarioResultFail     = "fail"
	ScenarioResultUntested = "untested"
)

// Test verdict constants: the test step's own conclusion about whether the
// change is safe to ship, independent of individual findings.
//
// TestVerdictNoSurface is the honest answer when the change itself has no
// runtime product no-mistakes can drive live - a CI-workflow-only change, a
// docs-only change, a pure non-runtime refactor, or anything else with no
// live-exercisable scenario. It is not a silent skip: the Test step parks
// for a human to decide whether proceeding without live validation is
// acceptable. A change that has a live surface and was not driven still
// uses pass/fail/untested plus go/no-go/inconclusive; claiming no-surface
// while any scenario is live or pass/fail is a contract violation.
const (
	TestVerdictGo           = "go"
	TestVerdictNoGo         = "no-go"
	TestVerdictInconclusive = "inconclusive"
	TestVerdictNoSurface    = "no-surface"
)

var (
	knownScenarioResults = []string{ScenarioResultPass, ScenarioResultFail, ScenarioResultUntested}
	knownTestVerdicts    = []string{TestVerdictGo, TestVerdictNoGo, TestVerdictInconclusive, TestVerdictNoSurface}
)

// IsKnownScenarioResult reports whether result is part of the scenario result
// vocabulary.
func IsKnownScenarioResult(result string) bool {
	return slices.Contains(knownScenarioResults, result)
}

// IsKnownTestVerdict reports whether verdict is part of the verdict vocabulary.
func IsKnownTestVerdict(verdict string) bool {
	return slices.Contains(knownTestVerdicts, verdict)
}

// KnownScenarioResults returns the scenario result vocabulary, for error
// messages that have to name what they accept.
func KnownScenarioResults() []string { return slices.Clone(knownScenarioResults) }

// KnownTestVerdicts returns the verdict vocabulary, for error messages that
// have to name what they accept.
func KnownTestVerdicts() []string { return slices.Clone(knownTestVerdicts) }

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
	ReviewScope      string `json:"review_scope,omitempty"`
	// Category separates the combined document+lint housekeeping pass's
	// findings into their owning gates and the CI step's findings by kind
	// (see the FindingCategoryCI* constants). Empty everywhere else.
	Category string `json:"category,omitempty"`
	// Check is the provider check name a CI finding was derived from. CheckID
	// is the provider's opaque identity for that exact check, so same-named
	// checks remain distinct through selection and repair. Both are empty on
	// every non-CI finding.
	Check   string `json:"check,omitempty"`
	CheckID string `json:"check_id,omitempty"`
}

// TestScenario is one named end-to-end scenario the test step derived from the
// user intent and the change, and the result of driving it.
//
// Live is the whole point of the record: it is true ONLY when the scenario was
// driven against the real product in this run. A unit test, a stub, a recorded
// fixture, or reading the code is not live, and a scenario that could not be
// driven here is reported with Result ScenarioResultUntested plus a Reason
// explaining the unavailable capability or absence of a live product surface,
// rather than being guessed at.
type TestScenario struct {
	Name     string `json:"name"`
	Result   string `json:"result"`
	Live     bool   `json:"live"`
	Evidence string `json:"evidence"`
	Reason   string `json:"reason"`
}

// LiveScenarioCounts returns how many of scenarios were driven live against
// the real product, and how many there are in total.
func LiveScenarioCounts(scenarios []TestScenario) (live, total int) {
	for _, s := range scenarios {
		total++
		if s.Live {
			live++
		}
	}
	return live, total
}

// NoLiveExercisableScenarios reports whether every scenario is untested and
// none were driven live. That is the only shape the Test step will accept as
// "this change has no live-validatable surface": a pass or fail, or any live
// mark, means there was something to exercise and no-surface must not cover
// it.
func NoLiveExercisableScenarios(scenarios []TestScenario) bool {
	if len(scenarios) == 0 {
		return false
	}
	for _, s := range scenarios {
		if s.Live || s.Result != ScenarioResultUntested {
			return false
		}
	}
	return true
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
	ReviewScope         string `json:"review_scope,omitempty"`
	Category            string `json:"category,omitempty"`
	Check               string `json:"check,omitempty"`
	CheckID             string `json:"check_id,omitempty"`
	RequiresHumanReview *bool  `json:"requires_human_review,omitempty"`
}

// Findings is the structured findings payload exchanged across pipeline, IPC, and TUI.
//
// Scenarios and Verdict are the test step's live-validation contract. Both are
// omitempty and both decode as their zero values from every findings payload
// written before the contract existed, so an older recorded run still parses
// and simply renders no scenario table.
type Findings struct {
	Items          []Finding      `json:"findings"`
	Summary        string         `json:"summary"`
	Tested         []string       `json:"tested,omitempty"`
	TestingSummary string         `json:"testing_summary,omitempty"`
	Artifacts      []TestArtifact `json:"artifacts,omitempty"`
	Scenarios      []TestScenario `json:"scenarios,omitempty"`
	Verdict        string         `json:"verdict,omitempty"`
	TestedHeadSHA  string         `json:"tested_head_sha,omitempty"`
	RiskLevel      string         `json:"risk_level"`
	RiskRationale  string         `json:"risk_rationale"`
	RiskScope      string         `json:"risk_scope,omitempty"`
}

type findingsWire struct {
	Items          []Finding      `json:"findings"`
	Legacy         []Finding      `json:"items"`
	Summary        string         `json:"summary"`
	Tested         []string       `json:"tested"`
	TestingSummary string         `json:"testing_summary"`
	Artifacts      []TestArtifact `json:"artifacts"`
	Scenarios      []TestScenario `json:"scenarios"`
	Verdict        string         `json:"verdict"`
	TestedHeadSHA  string         `json:"tested_head_sha"`
	RiskLevel      string         `json:"risk_level"`
	RiskRationale  string         `json:"risk_rationale"`
	RiskScope      string         `json:"risk_scope"`
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
	return Findings{
		Items:          items,
		Summary:        wire.Summary,
		Tested:         wire.Tested,
		TestingSummary: wire.TestingSummary,
		Artifacts:      wire.Artifacts,
		Scenarios:      wire.Scenarios,
		Verdict:        wire.Verdict,
		TestedHeadSHA:  wire.TestedHeadSHA,
		RiskLevel:      wire.RiskLevel,
		RiskRationale:  wire.RiskRationale,
		RiskScope:      wire.RiskScope,
	}, nil
}

// FindingsMetadata returns findings with its items dropped, so every helper
// that re-selects items keeps the whole evidence payload - tested commands,
// artifacts, scenarios, verdict, risk - without re-enumerating those fields at
// each call site. Enumerating them is how a new evidence field gets silently
// dropped by a filter written before it existed; the artifact list was already
// being lost that way by the pipeline's own merge and filter helpers.
//
// It is exported because those helpers live in internal/pipeline rather than
// here: this package owns what the payload contains, so it owns the answer to
// "keep everything except the items".
func FindingsMetadata(findings Findings) Findings {
	findings.Items = nil
	return findings
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
	filtered := FindingsMetadata(findings)
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
	result := FindingsMetadata(findings)
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
	result := FindingsMetadata(findings)
	for _, item := range findings.Items {
		if item.ActionOrDefault() == ActionAutoFix {
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
	result := FindingsMetadata(findings)
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
// "ask-user". It uses ActionOrDefault so an empty/missing action (which now
// defaults to ask-user) parks for a human, keeping this in agreement with
// AutoFixableFindings: an unclassified finding is never auto-fixed and is
// always caught here as ask-user.
func HasAskUserFindings(findings Findings) bool {
	for _, item := range findings.Items {
		if item.ActionOrDefault() == ActionAskUser {
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
		if item.ActionOrDefault() != ActionNoOp {
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
	f.ReviewScope = wire.ReviewScope
	f.Category = wire.Category
	f.Check = wire.Check
	f.CheckID = wire.CheckID
	if f.Action == "" && wire.RequiresHumanReview != nil {
		if *wire.RequiresHumanReview {
			f.Action = ActionAskUser
		} else {
			f.Action = ActionAutoFix
		}
	}
	return nil
}

// ActionOrDefault resolves a finding's effective action, defaulting an
// empty/missing action to ask-user (park), not auto-fix. This closes a
// fail-open hole: an unclassified finding on a non-schema path (a legacy
// requires_human_review omission, an IPC- or user-supplied finding) must
// route to a human rather than be silently auto-applied. It also matches the
// review prompt's own "When in doubt, default to ask-user" instruction.
// (MergeUserOverrides still stamps user-*added* findings auto-fix explicitly -
// a user who hand-adds a finding is asking for a fix.)
func (f Finding) ActionOrDefault() string {
	if f.Action == "" {
		return ActionAskUser
	}
	return f.Action
}
