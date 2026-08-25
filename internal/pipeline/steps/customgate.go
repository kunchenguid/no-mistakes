package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CustomGateStep runs one repository-declared extra check immediately after
// its anchor core step. It can only add a verdict to a run: the executor places
// it after the anchor and no core step consults it, so a gate that fails, or
// that the operator declines, stops the run without ever having been able to
// weaken what the core steps already decided.
type CustomGateStep struct {
	Gate config.Gate
}

func (s *CustomGateStep) Name() types.StepName { return s.Gate.StepName() }

func (s *CustomGateStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	if s.Gate.IsAgent() {
		return s.executeAgent(sctx)
	}
	return s.executeCommand(sctx)
}

func (s *CustomGateStep) executeCommand(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	command := strings.TrimSpace(s.Gate.Command)
	sctx.Log(fmt.Sprintf("running gate %q: %s", s.Gate.Name, command))
	output, exitCode, err := runStepShellCommand(sctx, command)
	if err != nil {
		return nil, fmt.Errorf("run gate %q command: %w", s.Gate.Name, err)
	}
	if exitCode == 0 {
		return &pipeline.StepOutcome{}, nil
	}

	projectedOutput := logConfiguredCommandOutput(sctx, output, s.Name())
	findings := Findings{
		Items: []Finding{{
			Severity:    "error",
			Description: fmt.Sprintf("gate %q failed with exit code %d", s.Gate.Name, exitCode),
			Action:      types.ActionAskUser,
		}},
		Summary: projectedOutput,
		Tested:  []string{command},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		// A repository-declared gate has no auto-fix budget of its own, and the
		// pipeline cannot know what fixing an arbitrary check means. Parking for
		// a human decision is the fail-closed direction: an unclassifiable
		// finding is the author's call, never the gate's.
		AutoFixable: false,
		Findings:    string(findingsJSON),
		ExitCode:    exitCode,
	}, nil
}

func (s *CustomGateStep) executeAgent(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	sctx.Log(fmt.Sprintf("running agent gate %q...", s.Gate.Name))

	prompt := fmt.Sprintf(
		`You are running one repository-declared validation gate named %q against a code change. Report only what this gate is responsible for.

Context:
- branch: %s
- base commit: %s
- target commit: %s

The repository's rule for this gate:
%s

Task:
- Inspect the change between the base commit and the target commit.
- Judge it against the repository's rule above and nothing else.
- Report each violation as a structured finding.
- If the change satisfies the rule, return an empty findings array.

Rules:
- Do not fix anything. This gate reports; it does not modify the worktree.
- Do not report issues that fall outside the rule above, however valid they may be.
- The summary must be one concise sentence fragment.%s`,
		s.Gate.Name,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		config.RenderedInstructions(s.Gate.Instructions),
		userIntentPromptSection(sctx),
	)

	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "gate:" + s.Gate.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("agent gate %q: %w", s.Gate.Name, err)
	}

	findings, err := parseGateFindings(string(result.Output))
	if err != nil {
		return nil, fmt.Errorf("agent gate %q: %w", s.Gate.Name, err)
	}
	if len(findings.Items) == 0 {
		return &pipeline.StepOutcome{}, nil
	}

	// Every finding an extra gate raises is the author's call: the gate states
	// a repository rule, and only a human can accept a change that breaks it.
	for i := range findings.Items {
		findings.Items[i].Action = types.ActionAskUser
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
	}, nil
}

func parseGateFindings(output string) (Findings, error) {
	var findings Findings
	if strings.TrimSpace(output) == "" {
		return findings, fmt.Errorf("agent returned no findings payload")
	}
	if err := json.Unmarshal([]byte(output), &findings); err != nil {
		return findings, fmt.Errorf("parse findings: %w", err)
	}
	return findings, nil
}
