package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStep runs tests and optionally asks the agent to fix failures.
type TestStep struct{}

func (s *TestStep) Name() types.StepName { return types.StepTest }

func (s *TestStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// In fix mode, ask agent to fix test failures first
	var newTestsFromFix []string
	if sctx.Fixing {
		sctx.Log("asking agent to fix test failures...")
		fixPrompt := fmt.Sprintf(
			`Fix the failing tests in this repository. Run the tests, identify failures, and fix either the tests or the code to make them pass.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the minimal change needed.
- Do not refactor beyond what is needed.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- Do NOT run linters, formatters, or static analysis tools.
- Re-run the relevant tests before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous test findings to address:
` + sctx.PreviousFindings
		}
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt:     fixPrompt,
			CWD:        sctx.WorkDir,
			JSONSchema: commitSummarySchema,
			OnChunk:    sctx.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("agent fix tests: %w", err)
		}
		newTestsFromFix = detectNewTestFiles(ctx, sctx.WorkDir)
		summary, err := extractCommitSummary(result)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
		}
		if err := commitAgentFixes(sctx, s.Name(), summary, "fix test failures"); err != nil {
			return nil, err
		}
	}

	testCmd := sctx.Config.Commands.Test
	if testCmd == "" {
		// No test command configured — ask agent to detect and run tests
		sctx.Log("no test command configured, asking agent to run tests...")
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt: fmt.Sprintf(
				`You are validating a code change by testing it. Examine the repository and run the appropriate tests yourself.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
- Understand the change before testing it.
- Run existing tests that are relevant to the change.
- Writing and running new tests if coverage is insufficient.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- If the issue is setup-related and fixable, fix it and retry the tests.

Rules:
- Do NOT run linters, formatters, or static analysis tools.
- Focus on testing and test-related fixes only.
- Only report actionable findings: test failures, unfixable setup issues, or flaky tests you identified.
- Do NOT report passing tests (whether existing or new), test counts, coverage summaries, or other non-actionable information.
- If all tests pass and there are no issues, return an empty findings array.`,
				sctx.Run.Branch,
				baseSHA,
				sctx.Run.HeadSHA,
			),
			CWD:        sctx.WorkDir,
			JSONSchema: findingsSchema,
			OnChunk:    sctx.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("agent run tests: %w", err)
		}

		var findings Findings
		if result.Output != nil {
			if err := json.Unmarshal(result.Output, &findings); err != nil {
				sctx.Log("could not parse structured output, using text response")
				findings = Findings{Summary: result.Text}
			}
		}

		needsApproval := hasBlockingFindings(findings.Items)
		autoFixable := needsApproval

		// Check if agent wrote new test files
		newTests := detectNewTestFiles(ctx, sctx.WorkDir)
		if len(newTests) > 0 {
			needsApproval = true
			autoFixable = false
			for _, f := range newTests {
				findings.Items = append(findings.Items, Finding{
					Severity:    "info",
					File:        f,
					Description: fmt.Sprintf("new test file written by agent: %s", f),
				})
			}
		}

		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: needsApproval,
			AutoFixable:   autoFixable,
			Findings:      string(findingsJSON),
		}, nil
	}

	// Run configured test command
	sctx.Log(fmt.Sprintf("running tests: %s", testCmd))
	output, exitCode, err := runShellCommand(ctx, sctx.WorkDir, testCmd)
	if err != nil {
		return nil, fmt.Errorf("run test command: %w", err)
	}

	sctx.Log(output)

	if exitCode != 0 {
		findings := Findings{
			Items: []Finding{{
				Severity:    "error",
				Description: fmt.Sprintf("tests failed with exit code %d", exitCode),
			}},
			Summary: output,
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
		}, nil
	}

	// Check if agent wrote new test files (fix mode uses agent before running tests)
	if sctx.Fixing && len(newTestsFromFix) > 0 {
		findings := Findings{
			Summary: "tests passed, but agent wrote new test files",
		}
		for _, f := range newTestsFromFix {
			findings.Items = append(findings.Items, Finding{
				Severity:    "info",
				File:        f,
				Description: fmt.Sprintf("new test file written by agent: %s", f),
			})
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
	}

	sctx.Log("all tests passed")
	return &pipeline.StepOutcome{}, nil
}
