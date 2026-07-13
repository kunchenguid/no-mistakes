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
	lintCmd := sctx.Config.Commands.Lint

	if lintCmd == "" {
		// The combined document+lint housekeeping pass already performed the
		// agent-driven lint duty for this round; consume its result instead
		// of paying a second cold agent invocation. Fix rounds and any round
		// without a stashed result fall through to a full agent pass, so the
		// lint responsibility is never silently skipped.
		if !sctx.Fixing {
			if stash, ok := sctx.Shared.TakeHousekeepingLint(); ok {
				return lintOutcomeFromHousekeeping(sctx, stash)
			}
		}
		sctx.Log("no lint command configured, asking agent to lint and fix...")
		reassessHistory := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		prompt := fmt.Sprintf(
			`Detect the linting and formatting tools for this project, run the relevant checks yourself, apply safe fixes, and verify the result.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
- Discover the configured linters and formatters for this repository.
- Only lint or format the relevant changed files when possible.
- Apply safe formatter, linter, and static-analysis fixes yourself.
- Re-run the relevant checks after fixing.
- Report only unresolved lint, format, or static-analysis issues as structured findings.
- If everything is clean or fixed, return an empty findings array.

Rules:
- Do not run tests or broader behavioral validation.
- Focus on lint, format, and static-analysis issues only.
- Do not report issues you already fixed.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reassessHistory,
		)
		if sctx.PreviousFindings != "" {
			prompt += `

Previous lint findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt:     prompt,
			CWD:        sctx.WorkDir,
			JSONSchema: findingsSchema,
			OnChunk:    sctx.LogChunk,
			Purpose:    "lint",
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
		summary, err := extractCommitSummary(result)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not parse lint summary: %v", err))
		}
		if err := commitAgentFixes(sctx, s.Name(), summary, "fix lint issues"); err != nil {
			return nil, err
		}

		needsApproval := hasBlockingFindings(findings.Items)
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: needsApproval,
			AutoFixable:   false,
			Findings:      string(findingsJSON),
			FixSummary:    summary,
		}, nil
	}

	// In fix mode, ask agent to fix lint issues first
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		fixPrompt := fmt.Sprintf(
			`Fix the lint issues in this repository. Run the linter, identify all issues, and fix them.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- Do not run tests or broader behavioral validation.
- Re-run the relevant lint or format commands before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			historySection,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous lint findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix lint issues...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix lint",
			FallbackSummary: "fix lint issues",
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	// Run configured lint command
	sctx.Log(fmt.Sprintf("running linter: %s", lintCmd))
	output, exitCode, err := runStepShellCommand(sctx, lintCmd)
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
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
			FixSummary:    fixSummary,
		}, nil
	}

	sctx.Log("lint passed")
	return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
}

// lintOutcomeFromHousekeeping reports the lint findings the combined
// document+lint pass produced, with the same gate semantics as the lint
// step's own agent path: blocking (error/warning) findings park for a
// decision, info findings pass through.
func lintOutcomeFromHousekeeping(sctx *pipeline.StepContext, stash pipeline.HousekeepingLintResult) (*pipeline.StepOutcome, error) {
	findings, err := types.ParseFindingsJSON(stash.FindingsJSON)
	if err != nil {
		// A malformed stash means the combined result cannot be trusted;
		// this should be unreachable (the document step marshalled it), but
		// fail safe by parking for a human rather than passing silently.
		sctx.Log("could not parse combined housekeeping lint result, requiring approval")
		return documentApprovalOutcome("combined housekeeping lint result unreadable"), nil
	}
	sctx.Log(fmt.Sprintf("lint assessed in the combined document+lint housekeeping pass: %d unresolved items", len(findings.Items)))
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(findings.Items),
		AutoFixable:   false,
		Findings:      stash.FindingsJSON,
		FixSummary:    stash.Summary,
	}, nil
}
