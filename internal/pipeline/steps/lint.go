package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// LintStep runs linters and asks the agent to fix issues.
type LintStep struct{}

func (s *LintStep) Name() types.StepName { return types.StepLint }

func (s *LintStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// In fix mode, ask agent to fix lint issues first
	if sctx.Fixing {
		sctx.Log("asking agent to fix lint issues...")
		fixPrompt := fmt.Sprintf(
			`Fix the lint issues in this repository. Run the linter, identify all issues, and fix them.

Context:
- branch: %s
- base commit: %s
- target commit: %s

			Rules:
			- Make the minimal change needed.
			- Do not refactor beyond what is needed.
			- Do not run tests or broader behavioral validation.
			- Re-run the relevant lint or format commands before finishing.`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous lint findings to address:
` + sctx.PreviousFindings
		}
		_, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt:  fixPrompt,
			CWD:     sctx.WorkDir,
			OnChunk: sctx.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("agent fix lint: %w", err)
		}
	}

	lintCmd := sctx.Config.Commands.Lint
	if lintCmd == "" {
		// No lint command configured — ask agent to detect and run linter
		sctx.Log("no lint command configured, asking agent to lint...")
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt: fmt.Sprintf(
				`Detect the linting and formatting tools for this project and run the relevant checks yourself.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
- Discover the configured linters and formatters for this repository.
- Only lint or format the relevant changed files when possible.
- Report any issues found as structured findings.

			Rules:
			- Do not run tests or broader behavioral validation.
			- Focus on lint, format, and static-analysis issues only.`,
				sctx.Run.Branch,
				baseSHA,
				sctx.Run.HeadSHA,
			),
			CWD:        sctx.WorkDir,
			JSONSchema: findingsSchema,
			OnChunk:    sctx.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("agent lint: %w", err)
		}

		var findings Findings
		if result.Output != nil {
			if err := json.Unmarshal(result.Output, &findings); err != nil {
				sctx.Log("could not parse structured output, using text response")
				findings = Findings{Summary: result.Text}
			}
		}

		needsApproval := hasBlockingFindings(findings.Items)
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: needsApproval,
			Findings:      string(findingsJSON),
		}, nil
	}

	// Run configured lint command
	sctx.Log(fmt.Sprintf("running linter: %s", lintCmd))
	output, exitCode, err := runShellCommand(ctx, sctx.WorkDir, lintCmd)
	if err != nil {
		return nil, fmt.Errorf("run lint command: %w", err)
	}

	sctx.Log(output)

	if exitCode != 0 {
		findings := Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: fmt.Sprintf("linter found issues (exit code %d)", exitCode),
			}},
			Summary: output,
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
		}, nil
	}

	sctx.Log("lint passed")
	return &pipeline.StepOutcome{}, nil
}
