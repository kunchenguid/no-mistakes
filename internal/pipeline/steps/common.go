package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Finding represents a single code review or lint finding.
type Finding = types.Finding

// Findings is the structured output from a pipeline step agent call.
type Findings = types.Findings

func unmarshalRequiredFindings(raw []byte, findings *Findings, requireNonEmptySummary bool) error {
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
		Scenarios *[]struct {
			Name     *string `json:"name"`
			Result   *string `json:"result"`
			Live     *bool   `json:"live"`
			Evidence *string `json:"evidence"`
			Reason   *string `json:"reason"`
		} `json:"scenarios"`
		Verdict *string `json:"verdict"`
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
		return fmt.Errorf("empty scenarios array")
	}
	for i, scenario := range *payload.Scenarios {
		if scenario.Name == nil || strings.TrimSpace(*scenario.Name) == "" {
			return fmt.Errorf("scenario %d missing name", i)
		}
		if scenario.Result == nil {
			return fmt.Errorf("scenario %d missing result", i)
		}
		if !types.IsKnownScenarioResult(*scenario.Result) {
			return fmt.Errorf("scenario %d result %q is not one of %s", i, *scenario.Result, strings.Join(types.KnownScenarioResults(), ", "))
		}
		if scenario.Live == nil {
			return fmt.Errorf("scenario %d missing live", i)
		}
		if scenario.Evidence == nil {
			return fmt.Errorf("scenario %d missing evidence", i)
		}
		if scenario.Reason == nil {
			return fmt.Errorf("scenario %d missing reason", i)
		}
		if *scenario.Result != types.ScenarioResultUntested && strings.TrimSpace(*scenario.Evidence) == "" {
			return fmt.Errorf("scenario %d result %q missing evidence", i, *scenario.Result)
		}
		if *scenario.Result == types.ScenarioResultUntested && *scenario.Live {
			return fmt.Errorf("scenario %d is untested but marked live", i)
		}
		if *scenario.Result != types.ScenarioResultUntested && !*scenario.Live {
			return fmt.Errorf("scenario %d result %q requires live validation", i, *scenario.Result)
		}
		if *scenario.Result == types.ScenarioResultUntested && strings.TrimSpace(*scenario.Reason) == "" {
			return fmt.Errorf("scenario %d untested without a reason", i)
		}
	}
	if payload.Verdict == nil {
		return fmt.Errorf("missing verdict")
	}
	if !types.IsKnownTestVerdict(*payload.Verdict) {
		return fmt.Errorf("verdict %q is not one of %s", *payload.Verdict, strings.Join(types.KnownTestVerdicts(), ", "))
	}
	if *payload.Verdict != types.TestVerdictNoGo {
		for i, scenario := range *payload.Scenarios {
			if *scenario.Result == types.ScenarioResultFail {
				return fmt.Errorf("verdict %q contradicts failed scenario %d", *payload.Verdict, i)
			}
		}
	}
	return nil
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
				"required": ["name", "result", "live", "evidence", "reason"]
			}
		},
		"verdict": {
			"type": "string",
			"enum": ["go", "no-go", "inconclusive"],
			"description": "go when every scenario that could be driven passed and nothing untested puts the intent in doubt; no-go when a scenario failed or the change is not safe to ship; inconclusive when too little could be driven live to judge"
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
