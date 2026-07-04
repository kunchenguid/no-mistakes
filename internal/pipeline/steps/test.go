package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStep runs baseline tests, gathers evidence for user intent, and optionally asks the agent to fix failures.
type TestStep struct{}

func (s *TestStep) Name() types.StepName { return types.StepTest }

func gitIgnoresPath(ctx context.Context, workDir, target string) bool {
	rel, err := filepath.Rel(workDir, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	_, err = git.Run(ctx, workDir, "check-ignore", "--quiet", "--", filepath.ToSlash(rel))
	return err == nil
}

func (s *TestStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// In fix mode, ask agent to fix test failures first
	var newTestsFromFix []string
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + stepInstructionsPromptSection(sctx)
		fixPrompt := fmt.Sprintf(
			`Fix the failing tests in this repository. Run the tests, identify failures, and fix either the tests or the code to make them pass.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- Do NOT run linters, formatters, or static analysis tools.
- Re-run the relevant tests before finishing.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes.
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

Previous test findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix test failures...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix tests",
			FallbackSummary: "fix test failures",
			AfterAgentRun: func(*agent.Result) error {
				newTestsFromFix = detectNewTestFiles(ctx, sctx.WorkDir)
				return nil
			},
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	testCmd := sctx.Config.Commands.Test
	tested := []string{}
	if testCmd != "" {
		sctx.Log(fmt.Sprintf("running tests: %s", testCmd))
		output, exitCode, err := runStepShellCommand(sctx, testCmd)
		if err != nil {
			return nil, fmt.Errorf("run test command: %w", err)
		}
		tested = append(tested, testCmd)

		sctx.Log(output)

		if exitCode != 0 {
			findings := Findings{
				Items: []Finding{{
					Severity:    "error",
					Description: fmt.Sprintf("tests failed with exit code %d", exitCode),
				}},
				Summary: output,
				Tested:  tested,
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
	}

	useEvidenceAgent := testCmd == "" || cleanedUserIntent(sctx) != ""
	if useEvidenceAgent {
		evidenceLocation := resolveTestEvidenceLocation(sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
		evidenceDir := evidenceLocation.Dir
		if evidenceLocation.StoreInRepo && gitIgnoresPath(ctx, sctx.WorkDir, evidenceDir) {
			evidenceLocation = testEvidenceLocation{Dir: testEvidenceDir(sctx.Run.ID)}
			evidenceDir = evidenceLocation.Dir
		}
		if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
			return nil, fmt.Errorf("create test evidence dir: %w", err)
		}
		if testCmd == "" {
			sctx.Log("no test command configured, asking agent to run tests...")
		} else {
			sctx.Log("user intent available, asking agent to gather test evidence...")
		}
		reassessHistory := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + stepInstructionsPromptSection(sctx)
		evidenceGuidance := fmt.Sprintf("- Write new evidence files into this temporary evidence directory: %s", evidenceDir)
		if evidenceLocation.StoreInRepo {
			evidenceGuidance = fmt.Sprintf("- Write new evidence files into this in-repo evidence directory; it is committed and pushed automatically, so artifacts render directly on the PR: %s", evidenceDir)
		}
		configuredTestCommand := ""
		if testCmd != "" {
			configuredTestCommand = fmt.Sprintf("\nConfigured test command already ran successfully as baseline: `%s`\n", testCmd)
		}
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt: fmt.Sprintf(
				`You are validating a code change by testing it. Examine the repository and run the appropriate tests yourself.

Context:
- branch: %s
- base commit: %s
- target commit: %s
%s

Task:
- Understand the user intent before testing it. If extracted user intent is present, use it as the primary hint for what success means.
- Decide what evidence or artifacts would clearly demonstrate the user intent is satisfied. Unit tests passing is not sufficient evidence by itself.
- Demonstrate the user intent working end-to-end in a way consistent with how an end user would actually experience it.
- Prefer product-level artifacts: screenshots, GIFs, videos, rendered UI, CLI transcripts, API responses, persisted database state, generated PR markdown, logs, or other outputs that directly show the intended behavior working.
- For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence.
- Prefer screenshots, images, videos, GIFs, or rendered HTML artifacts that show the actual end-user surface.
- If the test target is a simulator or emulator build (e.g. an iOS Simulator via xcodebuild/xcrun simctl, or an Android emulator), capture simulator screenshots or recordings of the running app as evidence (for iOS, xcrun simctl io booted screenshot), since the simulated end-user surface is the reviewer-visible artifact.
- DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available.
- If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary.
%s
- Do not move, commit, or modify source files only to make evidence linkable. Record local evidence file paths exactly where you created them.
- Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior. Generic pass/fail, coverage, or clean-worktree output is not sufficient evidence.
- Look for existing tests that would generate sufficient evidence. If they exist, run the smallest relevant set.
- If no existing test produces sufficient evidence, write or improve a test so that it does.
- If automated testing cannot produce the needed evidence, execute manual verification steps and record the evidence-producing steps you performed.
- If sufficient evidence is not possible, report a warning finding explaining what evidence is missing and why the user needs to decide what to do.
- Include a concise "testing_summary" sentence describing what you exercised and the overall result.
- The "testing_summary" must account for the complete test step: baseline commands that already ran, automated tests, manual or evidence-producing checks, artifacts gathered, and the overall result.
- Record the exact tests, manual checks, and evidence-producing steps you ran in a "tested" array. Prefer concrete commands or test selectors wrapped in backticks.
- Always include an "artifacts" array. Leave it empty when you produced no reviewer-visible evidence artifacts. Use artifact path for file artifacts, artifact url for externally visible artifacts, and artifact content for short logs or command output that should be shown directly in the PR.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- If the issue is setup-related and fixable, fix it and retry the tests.

Rules:
- Do NOT run linters, formatters, or static analysis tools.
- Focus on testing and test-related fixes only.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes, and leave evidence files in the dedicated evidence directory untouched.
- Keep "testing_summary" high-signal and natural language. Avoid raw logs and noisy counts.
- Always return a non-empty "tested" array describing what you exercised, even when all tests pass.
- Only report actionable findings: test failures, unfixable setup issues, flaky tests you identified, or missing evidence that prevents you from demonstrating the user intent.
- Do NOT report passing tests (whether existing or new), test counts, coverage summaries, or other non-actionable information.
- If all tests pass and there are no issues, return an empty findings array.
- Set action to "ask-user" for missing-evidence warning findings and only otherwise when a test failure seems desired and you question the author's intent of having the test in the first place. Set action to "auto-fix" for objective test failures that can be safely fixed. Set action to "no-op" for informational notes.%s`,
				sctx.Run.Branch,
				baseSHA,
				sctx.Run.HeadSHA,
				configuredTestCommand,
				evidenceGuidance,
				reassessHistory,
			),
			CWD:        sctx.WorkDir,
			JSONSchema: testFindingsSchema,
			OnChunk:    sctx.LogChunk,
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
		if len(tested) > 0 {
			findings.Tested = append(append([]string{}, tested...), findings.Tested...)
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
			FixSummary:    fixSummary,
		}, nil
	}

	// Check if agent wrote new test files (fix mode uses agent before running tests)
	if sctx.Fixing && len(newTestsFromFix) > 0 {
		findings := Findings{
			Summary: "tests passed, but agent wrote new test files",
			Tested:  tested,
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
			FixSummary:    fixSummary,
		}, nil
	}

	sctx.Log("all tests passed")
	findingsJSON, _ := json.Marshal(Findings{Tested: tested})
	return &pipeline.StepOutcome{Findings: string(findingsJSON), FixSummary: fixSummary}, nil
}
