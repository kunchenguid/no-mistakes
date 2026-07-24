package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
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

// localFastFixerToolingRules broadens the Test-repair tooling contract for
// split certification. In that mode the local-fast lint, typecheck, and
// focused-test commands are the failure surface this step reports, and lint and
// typecheck are static analysis by definition. A blanket "no static analysis"
// rule would forbid the fixer from reproducing and re-verifying the exact
// command that failed, so the ban narrows to tools outside the configured set.
func localFastFixerToolingRules(local config.LocalFastCommands) string {
	return fmt.Sprintf(
		"- The configured local-fast certification commands are the pre-publication gate for this repository: lint `%s`, typecheck `%s`, focused tests `%s`.\n"+
			"- Run the exact configured command for the failing check named in the findings to reproduce it, and re-run that same command to verify your fix, including when it is the lint or typecheck static-analysis command.\n"+
			"- Do NOT run linters, formatters, or static analysis tools other than those configured commands.",
		strings.TrimSpace(local.Lint),
		strings.TrimSpace(local.Typecheck),
		strings.TrimSpace(local.Test),
	)
}

func (s *TestStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// In fix mode, ask agent to fix test failures first.
	//
	// Targeted-validation rules (reproduce the specific failure, focused
	// re-verification only, never a complete repository suite) are a product
	// contract: local Test proves the requested intent, while remote CI owns
	// broad regression and remains mandatory before a PR is ready. A forensic
	// audit measured ~82 minutes of local complete-suite walks on one repair
	// path when prompts only said "run the tests" / "relevant". This is a
	// prompt contract, not an enforced sandbox - the agent has free shell
	// access - so the pinned regression tests guard the wording, not the
	// runtime. Process-group reaping on clean exit (#357) remains the lifecycle
	// safety net when agents do spawn test workers; it is not a reason to force
	// a deterministic full-suite commands.test override.
	testCmd := sctx.Config.Commands.Test
	commands := []struct {
		label string
		value string
	}{{label: "tests", value: testCmd}}
	splitCertification := sctx.Config.Certification.IsCIAuthoritative()
	if splitCertification {
		// Explicit split mode replaces (rather than supplements) commands.test.
		// This is the smallest counterfactual that prevents a legacy full-suite
		// or build command from contending with parallel hosted jobs.
		testCmd = sctx.Config.Certification.LocalFast.Test
		commands = []struct {
			label string
			value string
		}{
			{label: "lint", value: sctx.Config.Certification.LocalFast.Lint},
			{label: "typecheck", value: sctx.Config.Certification.LocalFast.Typecheck},
			{label: "focused tests", value: testCmd},
		}
	}

	var newTestsFromFix []string
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		fixOpening := "Fix the failing tests in this repository. Reproduce the specific failure, identify the root cause, and fix either the tests or the code so that failure passes."
		failureClassificationRule := "- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue."
		toolingRule := "- Do NOT run linters, formatters, or static analysis tools."
		if splitCertification {
			fixOpening = "Fix the failing local-fast certification check in this repository. Reproduce the specific failure, identify the root cause, and fix either the checks or the code so that failure passes."
			failureClassificationRule = "- If a check fails, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue."
			toolingRule = localFastFixerToolingRules(sctx.Config.Certification.LocalFast)
		}
		fixPrompt := fmt.Sprintf(
			`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
%s
%s
- Reproduce the specific failing case first (the exact test, package, script, or check named in the findings), then re-run only that focused verification after the fix.
- Do NOT run the complete repository test suite. Local Test is targeted validation of the failure and the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary. Keep verification focused on the failure and intent.
- Never treat "do not run everything" as permission to run nothing: if you cannot reproduce or re-verify with a targeted check, report that honestly in the summary rather than inventing a full-suite pass.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			fixOpening,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			failureClassificationRule,
			toolingRule,
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

	tested := []string{}
	for _, command := range commands {
		if command.value == "" {
			continue
		}
		sctx.Log(fmt.Sprintf("running %s: %s", command.label, command.value))
		output, exitCode, err := runStepShellCommand(sctx, command.value)
		if err != nil {
			return nil, fmt.Errorf("run %s command: %w", command.label, err)
		}
		tested = append(tested, command.value)

		projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepTest)
		if exitCode != 0 {
			findings := Findings{
				Items: []Finding{{
					Severity:    "error",
					Description: fmt.Sprintf("%s failed with exit code %d", command.label, exitCode),
				}},
				Summary: projectedOutput,
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
		reassessHistory := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
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
				`You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself.

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
- DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available.
- If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary.
%s
- Do not move, commit, or modify source files only to make evidence linkable. Record local evidence file paths exactly where you created them.
- Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior. Generic pass/fail, coverage, or clean-worktree output is not sufficient evidence.
- Look for existing tests that would generate sufficient evidence. If they exist, run the smallest relevant set that proves the requested intent.
- Do NOT run the complete repository test suite. Local Test is targeted validation of the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- Never treat "do not run everything" as permission to run nothing: if no targeted automated test can establish the intent, write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- If no existing test produces sufficient evidence, write or improve a focused test so that it does.
- If automated testing cannot produce the needed evidence, execute manual verification steps and record the evidence-producing steps you performed.
- If sufficient evidence is not possible, report a warning finding explaining what evidence is missing and why the user needs to decide what to do.
- Include a concise "testing_summary" sentence describing what you exercised and the overall result.
- The "testing_summary" must account for the complete test step: baseline commands that already ran, automated tests, manual or evidence-producing checks, artifacts gathered, and the overall result.
- Record the exact tests, manual checks, and evidence-producing steps you ran in a "tested" array. Prefer concrete commands or test selectors wrapped in backticks.
- Always include an "artifacts" array. Leave it empty when you produced no reviewer-visible evidence artifacts. Use artifact path for file artifacts, artifact url for externally visible artifacts, and artifact content for short logs or command output that should be shown directly in the PR.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- If the issue is setup-related and fixable, fix it and retry the focused tests.

Rules:
- Do NOT run linters, formatters, or static analysis tools.
- Focus on testing and test-related fixes only.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary.
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

		// Record any new test files the agent wrote as informational (no-op)
		// findings. Their presence alone is not an actionable problem, so they
		// must not force the test step into approval when tests pass (issue #140).
		newTests := detectNewTestFiles(ctx, sctx.WorkDir)
		for _, f := range newTests {
			findings.Items = append(findings.Items, Finding{
				Severity:    "info",
				Action:      types.ActionNoOp,
				File:        f,
				Description: fmt.Sprintf("new test file written by agent: %s", f),
			})
		}

		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: needsApproval,
			AutoFixable:   autoFixable,
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	// In fix mode the agent may add new test files while making tests pass.
	// Record them as informational (no-op) findings but do not gate on them:
	// passing tests with only informational findings proceed automatically (issue #140).
	if sctx.Fixing && len(newTestsFromFix) > 0 {
		findings := Findings{
			Summary: "tests passed, but agent wrote new test files",
			Tested:  tested,
		}
		for _, f := range newTestsFromFix {
			findings.Items = append(findings.Items, Finding{
				Severity:    "info",
				Action:      types.ActionNoOp,
				File:        f,
				Description: fmt.Sprintf("new test file written by agent: %s", f),
			})
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: false,
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	sctx.Log("all tests passed")
	findingsJSON, _ := json.Marshal(Findings{Tested: tested})
	return &pipeline.StepOutcome{Findings: string(findingsJSON), FixSummary: fixSummary}, nil
}
