package steps

import (
	"encoding/json"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Finding represents a single code review or lint finding.
type Finding = types.Finding

// Findings is the structured output from a pipeline step agent call.
type Findings = types.Findings

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
		}
	},
	"required": ["findings", "summary", "tested", "testing_summary", "artifacts"]
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
	return AllStepsForConfig(nil)
}

// AllStepsForConfig constructs the fixed pipeline step sequence for a resolved
// run configuration. A trusted no_ci declaration removes CI at this boundary,
// before a monitor or any of its forge dependencies can be constructed.
func AllStepsForConfig(cfg *config.Config) []pipeline.Step {
	if IsDemoMode() {
		return demoSteps(cfg == nil || !cfg.NoCI)
	}
	return allStepsForConfig(cfg, func() pipeline.Step { return &CIStep{} })
}

func RecoverySteps(cfg *config.Config, recordedCI bool) []pipeline.Step {
	if IsDemoMode() {
		if !recordedCI {
			return demoSteps(false)
		}
		if cfg == nil || !cfg.NoCI {
			return demoSteps(true)
		}
		result := demoSteps(false)
		result = append(result, &omittedCIStep{})
		return result
	}
	newCI := func() pipeline.Step { return &CIStep{} }
	if cfg != nil && cfg.NoCI {
		newCI = func() pipeline.Step { return &omittedCIStep{} }
	}
	return pipelineSteps(recordedCI, newCI)
}

func allStepsForConfig(cfg *config.Config, newCI func() pipeline.Step) []pipeline.Step {
	return pipelineSteps(cfg == nil || !cfg.NoCI, newCI)
}

func pipelineSteps(includeCI bool, newCI func() pipeline.Step) []pipeline.Step {
	result := []pipeline.Step{
		&IntentStep{},
		&RebaseStep{},
		&ReviewStep{},
		&TestStep{},
		&DocumentStep{},
		&LintStep{},
		&PushStep{},
		&PRStep{},
	}
	if includeCI {
		result = append(result, newCI())
	}
	return result
}

type omittedCIStep struct{}

func (*omittedCIStep) Name() types.StepName { return types.StepCI }

func (*omittedCIStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{Skipped: true}, nil
}
