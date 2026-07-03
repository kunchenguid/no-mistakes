package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

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
					"path": {"type": "string", "description": "artifact file path, including absolute paths for temporary local evidence files when available"},
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
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
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
		"risk_rationale": {"type": "string"}
	},
	"required": ["findings", "risk_level", "risk_rationale"]
}`)

// builtinStepConstructors maps each built-in step name to its constructor.
// types.AllSteps() is the canonical default sequence over these names.
var builtinStepConstructors = map[types.StepName]func() pipeline.Step{
	types.StepIntent:   func() pipeline.Step { return &IntentStep{} },
	types.StepRebase:   func() pipeline.Step { return &RebaseStep{} },
	types.StepReview:   func() pipeline.Step { return &ReviewStep{} },
	types.StepTest:     func() pipeline.Step { return &TestStep{} },
	types.StepDocument: func() pipeline.Step { return &DocumentStep{} },
	types.StepLint:     func() pipeline.Step { return &LintStep{} },
	types.StepPush:     func() pipeline.Step { return &PushStep{} },
	types.StepPR:       func() pipeline.Step { return &PRStep{} },
	types.StepCI:       func() pipeline.Step { return &CIStep{} },
}

// validStepNames is the user-facing list of accepted `steps:` names, matching
// the wording of the `axi run --skip` help.
const validStepNames = "intent, rebase, review, test, document, lint, push, pr, ci"

// AllSteps returns the fixed default pipeline step sequence.
// When NM_DEMO=1, it returns mock steps for demo recordings.
func AllSteps() []pipeline.Step {
	built, err := BuildPipeline(nil)
	if err != nil {
		// Unreachable: an empty spec list always builds the default pipeline.
		panic(fmt.Sprintf("build default pipeline: %v", err))
	}
	return built
}

// BuildPipeline builds the pipeline step slice a run executes, in list order.
// An empty spec list yields the full default pipeline (identical to AllSteps),
// which is the backward-compatible path for repos with no `steps:` config. A
// spec carrying a `command` becomes a custom CommandStep; a plain-name spec is
// a built-in. Invalid specs return an error listing every problem; ordering
// hazards that are legal but probably unintended are logged as warnings.
// When NM_DEMO=1, mock demo steps are returned regardless of specs.
func BuildPipeline(stepSpecs []config.StepSpec) ([]pipeline.Step, error) {
	if IsDemoMode() {
		return DemoSteps(), nil
	}
	if len(stepSpecs) == 0 {
		built := make([]pipeline.Step, 0, len(types.AllSteps()))
		for _, name := range types.AllSteps() {
			built = append(built, builtinStepConstructors[name]())
		}
		return built, nil
	}

	errs, warns := validateStepSpecs(stepSpecs)
	for _, warn := range warns {
		slog.Warn("steps config: " + warn)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid steps config: %s", strings.Join(errs, "; "))
	}

	built := make([]pipeline.Step, 0, len(stepSpecs))
	for _, spec := range stepSpecs {
		if spec.IsCommand() {
			built = append(built, &CommandStep{
				StepName:     types.StepName(spec.Name),
				Command:      spec.Command,
				FindingsPath: spec.FindingsJSON,
				Timeout:      spec.Timeout,
				AutoFix:      spec.AutoFix,
			})
			continue
		}
		built = append(built, builtinStepConstructors[types.StepName(spec.Name)]())
	}
	return built, nil
}

// customStepNamePattern constrains a custom step name to a filesystem- and
// identifier-safe shape: the name becomes a StepName, a DB key, and a
// "<name>.log" filename, so it must not contain path separators or collide
// with a built-in.
var customStepNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validateStepSpecs checks a full `steps:` selection (built-ins and custom
// command steps). It applies the same per-name and ordering rules as
// validateStepNames, plus custom-step-specific rules: a valid, non-colliding,
// filesystem-safe name. Names must be unique across built-in and custom steps
// alike, since the executor keys step records and log files by name.
func validateStepSpecs(specs []config.StepSpec) (errs, warns []string) {
	pos := make(map[types.StepName]int, len(specs))
	for i, spec := range specs {
		name := types.StepName(spec.Name)
		if spec.Name == "" {
			errs = append(errs, fmt.Sprintf("steps[%d]: empty step name (valid steps: %s; add a command: to define a custom step)", i, validStepNames))
			continue
		}
		if spec.IsCommand() {
			if !customStepNamePattern.MatchString(spec.Name) {
				errs = append(errs, fmt.Sprintf("steps[%d]: invalid custom step name %q (use lowercase letters, digits, '-' and '_', starting with a letter or digit)", i, spec.Name))
			}
			if _, isBuiltin := builtinStepConstructors[name]; isBuiltin {
				errs = append(errs, fmt.Sprintf("steps[%d]: custom step name %q collides with a built-in step; choose another name", i, spec.Name))
			}
		} else if _, ok := builtinStepConstructors[name]; !ok {
			errs = append(errs, fmt.Sprintf("steps[%d]: unknown step %q (valid steps: %s; add a command: to define a custom step)", i, spec.Name, validStepNames))
			continue
		}
		if first, dup := pos[name]; dup {
			errs = append(errs, fmt.Sprintf("steps[%d]: duplicate step %q (first at steps[%d])", i, spec.Name, first))
			continue
		}
		pos[name] = i
	}

	chainErrs, chainWarns := stepChainProblems(pos)
	return append(errs, chainErrs...), append(warns, chainWarns...)
}

// validateStepNames checks a `steps:` selection. Errors block the run: names
// must be known built-ins, non-empty, and unique, and the push chain must
// keep its documented data-loss invariants — the CI monitor needs the PR that
// the pr step opened, pr needs the branch push published, and push's
// force-push lease is anchored by rebase's fetch of the remote tips.
// Warnings flag legal-but-suspicious orderings: intent placed after the steps
// that consume it, or a worktree-mutating step after push (its changes would
// never reach the remote).
func validateStepNames(names []types.StepName) (errs, warns []string) {
	pos := make(map[types.StepName]int, len(names))
	for i, name := range names {
		if name == "" {
			errs = append(errs, fmt.Sprintf("steps[%d]: empty step name (valid steps: %s)", i, validStepNames))
			continue
		}
		if _, ok := builtinStepConstructors[name]; !ok {
			errs = append(errs, fmt.Sprintf("steps[%d]: unknown step %q (valid steps: %s)", i, name, validStepNames))
			continue
		}
		if first, dup := pos[name]; dup {
			errs = append(errs, fmt.Sprintf("steps[%d]: duplicate step %q (first at steps[%d])", i, name, first))
			continue
		}
		pos[name] = i
	}

	chainErrs, chainWarns := stepChainProblems(pos)
	return append(errs, chainErrs...), append(warns, chainWarns...)
}

// stepChainProblems enforces the push-chain data-loss invariants and flags
// legal-but-suspicious orderings, given the position of each built-in step in
// the pipeline. Custom step names never match the built-in constants it checks,
// so they are transparent to these rules.
func stepChainProblems(pos map[types.StepName]int) (errs, warns []string) {
	requiredBefore := []struct {
		step, dep types.StepName
		why       string
	}{
		{types.StepCI, types.StepPR, "the CI monitor babysits the PR the pr step opened"},
		{types.StepPR, types.StepPush, "the pr step needs the branch the push step published"},
		{types.StepPush, types.StepRebase, "the push step's force-push lease is anchored by the rebase step's fetch of the remote tips"},
	}
	for _, r := range requiredBefore {
		i, ok := pos[r.step]
		if !ok {
			continue
		}
		if j, ok := pos[r.dep]; !ok {
			errs = append(errs, fmt.Sprintf("step %q requires %q earlier in the list: %s", r.step, r.dep, r.why))
		} else if j > i {
			errs = append(errs, fmt.Sprintf("step %q must come after %q: %s", r.step, r.dep, r.why))
		}
	}

	if intentPos, ok := pos[types.StepIntent]; ok {
		for _, consumer := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepPR} {
			if i, ok := pos[consumer]; ok && i < intentPos {
				warns = append(warns, fmt.Sprintf("step %q runs after %q, so earlier steps see no user intent", types.StepIntent, consumer))
				break
			}
		}
	}
	if pushPos, ok := pos[types.StepPush]; ok {
		for _, mutating := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
			if i, ok := pos[mutating]; ok && i > pushPos {
				warns = append(warns, fmt.Sprintf("step %q runs after %q, so changes it makes are never pushed", mutating, types.StepPush))
			}
		}
	}
	return errs, warns
}
