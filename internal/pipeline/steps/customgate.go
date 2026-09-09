package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CustomGateStep runs one repository-declared extra check immediately after
// its anchor core step. It can only add a verdict to a run: the executor places
// it after the anchor and no core step consults it. A failed check parks for an
// operator decision, so the gate cannot weaken what the core steps decided.
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
// the command that must exit 0, because a bare finding does not say what
// passing would mean.
func (s *CustomGateStep) runFixTurn(sctx *pipeline.StepContext) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	requirement := fmt.Sprintf("This gate passes only when the following command exits 0:\n%s", strings.TrimSpace(s.Gate.Command))

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
		executionContextPromptSection(sctx.WorkDir)+roundHistoryPromptSection(sctx)+userIntentPromptSection(sctx),
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
