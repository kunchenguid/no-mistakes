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
	fixSummary, err := s.runFixTurn(sctx)
	if err != nil {
		return nil, err
	}
	if s.Gate.IsAgent() {
		return s.executeAgent(sctx, fixSummary)
	}
	return s.executeCommand(sctx, fixSummary)
}

// runFixTurn repairs the worktree when the gate's park was answered with `fix`,
// and returns the agent's commit summary. The caller then re-runs the gate's
// own check, so a re-parked verdict describes the repaired worktree rather than
// the unchanged one that produced the previous findings.
//
// This is the same fix protocol Test and Lint use, and a gate needs it for the
// same reason: without it, answering `fix` costs a full extra execution that
// provably cannot change the verdict. The gate's identity carries the intent -
// the command that must exit 0, or the repository rule an agent gate states -
// because a bare finding does not say what passing would mean.
func (s *CustomGateStep) runFixTurn(sctx *pipeline.StepContext) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	requirement := fmt.Sprintf("This gate passes only when the following command exits 0:\n%s", strings.TrimSpace(s.Gate.Command))
	if s.Gate.IsAgent() {
		requirement = fmt.Sprintf("This gate passes only when the change satisfies the repository's rule below:\n%s", config.RenderedInstructions(s.Gate.Instructions))
	}

	prompt := fmt.Sprintf(
		`Fix the violations reported by the repository gate %q.

Context:
- branch: %s
- base commit: %s
- target commit: %s

%s

Rules:
- Make the smallest correct root-cause fix that satisfies the gate.
- Do not refactor beyond what is needed for that root-cause fix.
- Do not weaken, disable, or narrow the gate itself to make it pass.
- Re-run or re-check the gate's own requirement above before finishing, and nothing broader.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		s.Gate.Name,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		requirement,
		executionContextPromptSection()+roundHistoryPromptSection(sctx)+userIntentPromptSection(sctx),
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous gate findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}

	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      fmt.Sprintf("asking agent to satisfy gate %q...", s.Gate.Name),
		Prompt:          prompt,
		ErrorPrefix:     fmt.Sprintf("agent fix gate %q", s.Gate.Name),
		FallbackSummary: fmt.Sprintf("satisfy %s gate", s.Gate.Name),
	})
}

func (s *CustomGateStep) executeCommand(sctx *pipeline.StepContext, fixSummary string) (*pipeline.StepOutcome, error) {
	command := strings.TrimSpace(s.Gate.Command)
	sctx.Log(fmt.Sprintf("running gate %q: %s", s.Gate.Name, command))
	output, exitCode, err := runStepShellCommand(sctx, command)
	if err != nil {
		return nil, fmt.Errorf("run gate %q command: %w", s.Gate.Name, err)
	}
	if exitCode == 0 {
		return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
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
		// A gate must never repair on the pipeline's own initiative: it states a
		// repository rule, so deciding that the change should be altered to
		// satisfy it is the author's call, not the pipeline's. Answering the park
		// with `fix` IS that authorization, and runFixTurn services it - being
		// non-auto-fixable is about who decides, not about whether a repair is
		// possible.
		AutoFixable: false,
		Findings:    string(findingsJSON),
		ExitCode:    exitCode,
		FixSummary:  fixSummary,
	}, nil
}

func (s *CustomGateStep) executeAgent(sctx *pipeline.StepContext, fixSummary string) (*pipeline.StepOutcome, error) {
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
		Purpose:    string(s.Name()),
	})
	if err != nil {
		return nil, fmt.Errorf("agent gate %q: %w", s.Gate.Name, err)
	}

	findings, err := parseGateFindings(string(result.Output))
	if err != nil {
		return nil, fmt.Errorf("agent gate %q: %w", s.Gate.Name, err)
	}
	if len(findings.Items) == 0 {
		return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
	}

	// Every finding an extra gate raises is the author's call: the gate states
	// a repository rule, and only a human can accept a change that breaks it.
	for i := range findings.Items {
		findings.Items[i].Action = types.ActionAskUser
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		// See executeCommand: not auto-fixable is about who authorizes a repair,
		// and runFixTurn services an authorized one.
		AutoFixable: false,
		Findings:    string(findingsJSON),
		FixSummary:  fixSummary,
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
