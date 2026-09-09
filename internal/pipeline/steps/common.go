package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Finding represents a single code review or lint finding.
type Finding = types.Finding

// Findings is the structured output from a pipeline step agent call.
type Findings = types.Findings

func unmarshalRequiredFindings(raw []byte, findings *Findings, requireNonEmptySummary bool) error {
	// Native structured-output adapters can pass data straight to the step.
	// Apply the same ambiguity guard as text adapters before typed decoding.
	if _, err := agent.ParseStructuredObject(string(raw)); err != nil {
		return err
	}
	parsed, err := types.ParseFindingsJSON(string(raw))
	if err != nil {
		return err
	}
	var payload struct {
		Summary  *string            `json:"summary"`
		Findings *[]json.RawMessage `json:"findings"`
		Items    *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Findings == nil && payload.Items == nil {
		return fmt.Errorf("missing findings array")
	}
	if payload.Summary == nil {
		return fmt.Errorf("missing summary")
	}
	if requireNonEmptySummary && strings.TrimSpace(*payload.Summary) == "" {
		return fmt.Errorf("missing summary")
	}
	for i, item := range parsed.Items {
		switch item.Severity {
		case "error", "warning", "info":
		default:
			return fmt.Errorf("finding %d missing severity", i)
		}
		if strings.TrimSpace(item.Description) == "" {
			return fmt.Errorf("finding %d missing description", i)
		}
		switch item.Action {
		case types.ActionNoOp, types.ActionAutoFix, types.ActionAskUser:
		default:
			return fmt.Errorf("finding %d missing action", i)
		}
	}
	*findings = parsed
	return nil
}

func unmarshalRequiredTestFindings(raw []byte, findings *Findings) error {
	if err := unmarshalRequiredFindings(raw, findings, false); err != nil {
		return err
	}
	var payload struct {
		Tested         *[]string `json:"tested"`
		TestingSummary *string   `json:"testing_summary"`
		Artifacts      *[]struct {
			Label *string `json:"label"`
		} `json:"artifacts"`
		Scenarios *[]testScenarioContractFields `json:"scenarios"`
		Verdict   *string                       `json:"verdict"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Tested == nil {
		return fmt.Errorf("missing tested array")
	}
	if len(*payload.Tested) == 0 {
		return fmt.Errorf("empty tested array")
	}
	hasTestedEvidence := false
	for _, tested := range *payload.Tested {
		if strings.TrimSpace(tested) != "" {
			hasTestedEvidence = true
			break
		}
	}
	if !hasTestedEvidence {
		return fmt.Errorf("empty tested array")
	}
	if payload.TestingSummary == nil {
		return fmt.Errorf("missing testing summary")
	}
	if strings.TrimSpace(*payload.TestingSummary) == "" {
		return fmt.Errorf("empty testing summary")
	}
	if payload.Artifacts == nil {
		return fmt.Errorf("missing artifacts array")
	}
	for i, artifact := range *payload.Artifacts {
		if artifact.Label == nil {
			return fmt.Errorf("artifact %d missing label", i)
		}
	}
	// The scenario list and the verdict are the step's live-validation
	// contract, held exactly as strictly as the evidence fields above: a turn
	// that omits them has not answered the question the step was asked, and
	// accepting the omission would silently restore the pre-contract behaviour
	// where "unit tests passed" reads as "the intent works".
	if payload.Scenarios == nil {
		return fmt.Errorf("missing scenarios array")
	}
	if len(*payload.Scenarios) == 0 {
		return fmt.Errorf("empty scenarios array - report at least one named scenario; if nothing could be driven, return it as untested with a reason")
	}
	var issues []string
	for i, scenario := range *payload.Scenarios {
		issues = append(issues, scenarioContractIssues(i, scenario)...)
	}
	if payload.Verdict == nil {
		issues = append(issues, "missing verdict - set verdict to "+strings.Join(types.KnownTestVerdicts(), ", "))
	} else if !types.IsKnownTestVerdict(*payload.Verdict) {
		issues = append(issues, fmt.Sprintf("verdict %q is not one of %s", *payload.Verdict, strings.Join(types.KnownTestVerdicts(), ", ")))
	} else {
		if *payload.Verdict != types.TestVerdictNoGo {
			for i, scenario := range *payload.Scenarios {
				if scenario.Result != nil && *scenario.Result == types.ScenarioResultFail {
					issues = append(issues, fmt.Sprintf("verdict %q contradicts failed scenario %d - a fail scenario requires verdict %q", *payload.Verdict, i+1, types.TestVerdictNoGo))
				}
			}
		}
		// no-surface is the ask-user park for a change with nothing to drive
		// live. A pass, fail, or live mark means there was a live-exercisable
		// scenario, so treating that as no-surface would let a skipped or
		// failed live validation masquerade as "nothing to test".
		if *payload.Verdict == types.TestVerdictNoSurface && !types.NoLiveExercisableScenarios(findings.Scenarios) {
			contradicted := false
			for i, scenario := range findings.Scenarios {
				if scenario.Live || scenario.Result != types.ScenarioResultUntested {
					issues = append(issues, fmt.Sprintf("verdict %q contradicts live-exercisable scenario %d - no-surface requires every scenario untested and not live; if you drove or claimed a pass/fail, use go, no-go, or inconclusive instead", *payload.Verdict, i+1))
					contradicted = true
				}
			}
			if !contradicted {
				issues = append(issues, fmt.Sprintf("verdict %q contradicts live-exercisable scenario", *payload.Verdict))
			}
		}
	}
	if len(issues) > 0 {
		return fmt.Errorf("%s", strings.Join(issues, "\n"))
	}
	return nil
}

type testScenarioContractFields struct {
	Name     *string `json:"name"`
	Result   *string `json:"result"`
	Live     *bool   `json:"live"`
	Evidence *string `json:"evidence"`
	Reason   *string `json:"reason"`
}

func scenarioContractIssues(i int, scenario testScenarioContractFields) []string {
	n := i + 1
	var issues []string
	if scenario.Name == nil || strings.TrimSpace(*scenario.Name) == "" {
		issues = append(issues, fmt.Sprintf("scenario %d: missing name - every scenario needs a non-empty name describing what an end user does", n))
	}
	knownResult := scenario.Result != nil && types.IsKnownScenarioResult(*scenario.Result)
	if scenario.Result == nil {
		issues = append(issues, fmt.Sprintf("scenario %d: missing result - set result to %s", n, strings.Join(types.KnownScenarioResults(), ", ")))
	} else if !knownResult {
		issues = append(issues, fmt.Sprintf("scenario %d: result %q is not one of %s", n, *scenario.Result, strings.Join(types.KnownScenarioResults(), ", ")))
	}
	if scenario.Live == nil {
		issues = append(issues, fmt.Sprintf("scenario %d: missing live - set live true only when you drove this against the real product in this run", n))
	}
	if scenario.Evidence == nil {
		issues = append(issues, fmt.Sprintf("scenario %d: missing evidence", n))
	}
	if !knownResult {
		return issues
	}
	result := *scenario.Result
	if result != types.ScenarioResultUntested && scenario.Evidence != nil && strings.TrimSpace(*scenario.Evidence) == "" {
		issues = append(issues, fmt.Sprintf("scenario %d: result %q missing evidence - cite the command, artifact, or evidence file that shows this live result", n, result))
	}
	if scenario.Live != nil && result == types.ScenarioResultUntested && *scenario.Live {
		issues = append(issues, fmt.Sprintf("scenario %d: result %q but live=true - untested scenarios must set live=false; live=true is only for scenarios you drove against the real product", n, result))
	}
	if scenario.Live != nil && result != types.ScenarioResultUntested && !*scenario.Live {
		issues = append(issues, fmt.Sprintf("scenario %d: result %q but live=false - if you did not drive this against the live product, mark it result %q with a reason instead of %q", n, result, types.ScenarioResultUntested, result))
	}
	if result == types.ScenarioResultUntested && (scenario.Reason == nil || strings.TrimSpace(*scenario.Reason) == "") {
		issues = append(issues, fmt.Sprintf("scenario %d: result %q without a reason - name the specific tool, credential, permission, or authority that stopped you, and how to provide it", n, result))
	}
	return issues
}

// findingsSchema is the JSON schema for structured findings output.
var findingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"summary": {"type": "string"},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		}
	},
	"required": ["findings", "summary"]
}`)

var testFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"summary": {"type": "string"},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		},
		"artifacts": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"kind": {"type": "string", "description": "artifact type such as screenshot, gif, image, video, log, command-output, or other"},
					"label": {"type": "string"},
					"path": {"type": "string", "description": "artifact file path: repository-relative for a file inside the repository, or the full path to the file in this run's evidence directory for an evidence file. Do not report a path from anywhere else on the machine."},
					"url": {"type": "string", "description": "artifact URL when available"},
					"content": {"type": "string", "description": "short log, command output, or textual artifact content to show inline"}
				},
				"required": ["label"]
			}
		},
		"scenarios": {
			"type": "array",
			"description": "every scenario this change must satisfy, derived from the user intent and the change itself, with the result of driving it",
			"items": {
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "what an end user does and the observable result that proves it"},
					"result": {"type": "string", "enum": ["pass", "fail", "untested"]},
					"live": {"type": "boolean", "description": "true ONLY when this scenario was driven against the real running product in this run; a unit test, stub, recorded fixture, or code reading is not live"},
					"evidence": {"type": "string", "description": "the command, artifact label, or evidence file that shows this result"},
					"reason": {"type": "string", "description": "required for untested: the specific tool, credential, permission, or authority that was missing, and how to provide it"}
				},
				"required": ["name", "result", "live", "evidence"]
			}
		},
		"verdict": {
			"type": "string",
			"enum": ["go", "no-go", "inconclusive", "no-surface"],
			"description": "go when every scenario that could be driven passed and nothing untested puts the intent in doubt; no-go when a scenario failed or the change is not safe to ship; inconclusive when the change has a live-exercisable product surface but too little could be driven live to judge; no-surface when this change has no runtime product surface no-mistakes can drive live"
		}
	},
	"required": ["findings", "summary", "tested", "testing_summary", "artifacts", "scenarios", "verdict"]
}`)

// reviewFindingsSchema is the JSON schema for structured review output with risk assessment.
// Field order matters for chain-of-thought: findings first, then risk level, then rationale.
var reviewFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]},
					"review_scope": {"type": "string", "enum": ["source", "pipeline-owned-delivery", "external-delivery"]}
				},
				"required": ["severity", "description", "action", "review_scope"]
			}
		},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		},
		"risk_level": {"type": "string", "enum": ["low", "medium", "high"]},
		"risk_rationale": {"type": "string"},
		"risk_scope": {"type": "string", "enum": ["source-or-external", "pipeline-owned-delivery"]}
	},
	"required": ["findings", "risk_level", "risk_rationale", "risk_scope"]
}`)

// AllSteps returns the fixed pipeline step sequence.
// When NM_DEMO=1, it returns mock steps for demo recordings.
func AllSteps() []pipeline.Step {
	if IsDemoMode() {
		return DemoSteps()
	}
	return []pipeline.Step{
		&IntentStep{},
		&RebaseStep{},
		&ReviewStep{},
		&TestStep{},
		&DocumentStep{},
		&LintStep{},
		&PushStep{},
		&PRStep{},
		&CIStep{},
	}
}
