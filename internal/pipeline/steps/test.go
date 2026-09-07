package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStep runs baseline tests, gathers evidence for user intent, and optionally asks the agent to fix failures.
type TestStep struct{}

func (s *TestStep) Name() types.StepName { return types.StepTest }

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
	// Captured inside the fix turn, before commitAgentFixes stages and commits:
	// detectNewTestFiles reads uncommitted status, so the evidence turn that
	// follows can no longer see a test file the fixer already committed.
	var newTestsFromFix []string
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + testguidance.Rule
		fixPrompt := fmt.Sprintf(
			`Fix the failing tests in this repository. Reproduce the specific failure, identify the root cause, and fix either the tests or the code so that failure passes.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- Do NOT run linters, formatters, or static analysis tools.
- Reproduce the specific failing case first (the exact test, package, script, or check named in the findings), then re-run only that focused verification after the fix.
- Do NOT run the complete repository test suite. Local Test is targeted validation of the failure and the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary. Keep verification focused on the failure and intent.
- Never treat "do not run everything" as permission to run nothing: if you cannot reproduce or re-verify with a targeted check, report that honestly in the summary rather than inventing a full-suite pass.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes. Do not remove dependencies materialized by commands.prepare; later configured commands share them.
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
		fixCtx, cancelFix, fixTimeout := testAgentContext(sctx)
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix test failures...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix tests",
			FallbackSummary: "fix test failures",
			AgentContext:    fixCtx,
			AfterAgentRun: func(*agent.Result) error {
				newTestsFromFix = detectNewTestFiles(ctx, sctx.WorkDir)
				return nil
			},
		})
		cancelFix()
		if err != nil {
			return nil, testAgentError(fixCtx, fixTimeout, "agent fix tests", err)
		}
		fixSummary = summary
	}

	testCmd := sctx.Config.Commands.Test
	tested := []string{}
	var baselineFindings []Finding
	var baselineSummary string
	var baselineExitCode int
	if testCmd != "" {
		if err := ensurePrepared(sctx, s.Name()); err != nil {
			return nil, fmt.Errorf("prepare test dependencies: %w", err)
		}
		sctx.Log(fmt.Sprintf("running tests: %s", testCmd))
		output, exitCode, err := runStepShellCommand(sctx, testCmd)
		if err != nil {
			return nil, fmt.Errorf("run test command: %w", err)
		}
		tested = append(tested, testCmd)

		projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepTest)
		if exitCode != 0 {
			baselineFindings = []Finding{{
				Severity:    "error",
				Description: fmt.Sprintf("tests failed with exit code %d", exitCode),
			}}
			baselineSummary = projectedOutput
			baselineExitCode = exitCode
		}
	}

	evidenceDir := testEvidenceDir(sctx)
	if evidenceDir == "" {
		return nil, fmt.Errorf("test evidence dir is not configured for this run")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return nil, fmt.Errorf("create test evidence dir: %w", err)
	}
	if testCmd == "" {
		sctx.Log("no test command configured, asking agent to run tests...")
	} else if baselineExitCode != 0 {
		sctx.Log("baseline tests failed, asking agent to gather live evidence...")
	} else {
		sctx.Log("baseline tests passed, asking agent to gather live evidence...")
	}
	reassessHistory := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + testguidance.Rule
	evidenceGuidance := fmt.Sprintf("- Write new evidence files into this evidence directory, never into the worktree: %s", evidenceDir)
	if sctx.Config.Test.Evidence.StoreInRepo {
		evidenceGuidance = fmt.Sprintf("- Write new evidence files into this evidence directory, never into the worktree; they are published to the repository's %s branch automatically and linked from the PR: %s", sctx.Config.Test.Evidence.Branch, evidenceDir)
	}
	configuredTestCommand := ""
	if testCmd != "" {
		if baselineExitCode == 0 {
			configuredTestCommand = fmt.Sprintf("\nConfigured test command already ran successfully as baseline: `%s`\n", testCmd)
		} else {
			configuredTestCommand = fmt.Sprintf("\nConfigured test command ran as baseline and failed with exit code %d: `%s`\n", baselineExitCode, testCmd)
		}
	}
	trustedRunbook := trustedTestInstructionsSection(sctx)
	evidenceCtx, cancelEvidence, evidenceTimeout := testAgentContext(sctx)
	result, err := sctx.RunAgentContext(evidenceCtx, agent.RunOpts{
		Prompt: fmt.Sprintf(
			`You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product.

Context:
- branch: %s
- base commit: %s
- target commit: %s
%s%s

Derive the scenarios:
- Understand the user intent before testing it. If extracted user intent is present, use it as the primary hint for what success means; otherwise derive the intent from the change itself.
- Turn that intent into a short list of named scenarios. Each scenario is one concrete thing an end user does and one observable result that proves it, named so a reviewer who never saw this change can tell what was exercised.
- Where the intent names a failure mode, a guard, or a boundary, add an adversarial scenario that actively tries to break it rather than only confirming the happy path.
- Keep the list proportionate to the change: cover what this change actually alters, not the whole product.

Drive each scenario:
- Stand the product up the way an end user runs it, in an isolated environment, and drive each scenario end-to-end against that running product.
- Mark a scenario "live": true ONLY when you drove it against the real product in this run. A unit test, a stub, a mock, a recorded fixture, or reading the code is NOT live.
- When a scenario cannot be driven live here, return it with result "untested" and a reason naming the specific tool, credential, permission, or authority that stopped you, and how to provide it. Never guess a pass, and never mark a scenario live because you believe it would work.
- Report every scenario in the "scenarios" array with name, result ("pass", "fail", or "untested"), live, evidence, and reason.
- Return a "verdict": "go" when every scenario you could drive passed and nothing untested puts the intent in doubt, "no-go" when a scenario failed or the change is not safe to ship, "inconclusive" when the change has a live-exercisable product surface but too little could be driven live to judge, "no-surface" when this change has no runtime product surface no-mistakes can drive live (a CI-workflow-only change, a docs-only change, a pure non-runtime refactor, or anything else with no live-exercisable scenario).
- A "no-go" verdict parks this step for a decision. A "no-surface" verdict parks for a human to decide whether to proceed without live validation; mark every scenario untested with a reason naming why there is no live-validatable surface, never mark those as pass, and never use no-surface to skip live validation of a change that does have a product surface you could have driven. Untested scenarios are listed on the pull request and do not park by themselves, so an honest "untested" costs nothing and a guessed "pass" costs everything.
- A single scenario you could not drive live is reported as an untested scenario with its reason, NOT as a finding. Report a finding only when the step as a whole cannot demonstrate the user intent.

Evidence:
- Decide what evidence or artifacts would clearly demonstrate each scenario's result. Unit tests passing is not sufficient evidence by itself.
- Prefer product-level artifacts: screenshots, GIFs, videos, rendered UI, CLI transcripts, API responses, persisted database state, generated PR markdown, logs, or other outputs that directly show the intended behavior working.
- For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence.
- Prefer screenshots, images, videos, GIFs, or rendered HTML artifacts that show the actual end-user surface.
- DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available.
- If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary.
%s
- Do not move, commit, or modify source files only to make evidence linkable. Record local evidence file paths exactly where you created them.
- Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior. Generic pass/fail, coverage, or clean-worktree output is not sufficient evidence.
- If an existing automated test already drives a scenario end-to-end, run that test as the scenario and cite it as the evidence.
- Do NOT run the complete repository test suite. Local Test is targeted validation of the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- Never treat "do not run everything" as permission to run nothing: if no existing check drives a scenario, write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- If sufficient evidence is not possible, report a warning finding explaining what evidence is missing and why the user needs to decide what to do. When the blocker is a host capability or OS permission the agent's own process lacks (for example, the Screen Recording permission macOS requires to capture a native GUI application), name the specific capability or permission and how to grant it so the user can enable it and re-run, instead of retrying blindly or failing opaquely.
- Include a concise "testing_summary" sentence describing what you exercised and the overall result.
- The "testing_summary" must account for the complete test step: baseline commands that already ran, scenarios driven, manual or evidence-producing checks, artifacts gathered, and the overall result.
- Record the exact tests, manual checks, and evidence-producing steps you ran in a "tested" array. Prefer concrete commands or test selectors wrapped in backticks.
- Always include an "artifacts" array. Leave it empty when you produced no reviewer-visible evidence artifacts. Use artifact path for file artifacts, artifact url for externally visible artifacts, and artifact content for short logs or command output that should be shown directly in the PR.
- If a scenario fails, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- If the issue is setup-related and fixable, fix it and re-drive that scenario.

Rules:
- Do NOT run linters, formatters, or static analysis tools.
- Focus on testing and test-related fixes only.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes, leave evidence files in the dedicated evidence directory untouched, and do not remove dependencies materialized by commands.prepare because later configured commands share them.
- Keep "testing_summary" high-signal and natural language. Avoid raw logs and noisy counts.
- Always return a non-empty "tested" array describing what you exercised, even when every scenario passes.
- Only report actionable findings: scenario or test failures, unfixable setup issues, flaky tests you identified, or missing evidence that prevents you from demonstrating the user intent at all.
- Do NOT report passing tests (whether existing or new), test counts, coverage summaries, or other non-actionable information.
- If every scenario passes and there are no issues, return an empty findings array.
- Set action to "ask-user" when a test failure seems desired and you question the author's intent of having the test in the first place. Set action to "auto-fix" for objective failures that can be safely fixed. Set action to "no-op" for informational notes.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			configuredTestCommand,
			trustedRunbook,
			evidenceGuidance,
			reassessHistory,
		),
		CWD:        sctx.WorkDir,
		JSONSchema: testFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	runErr := testAgentError(evidenceCtx, evidenceTimeout, "agent run tests", err)
	cancelEvidence()
	if runErr != nil {
		return nil, runErr
	}

	var findings Findings
	if result.Output == nil {
		return nil, errors.New("test analyzer returned no structured findings")
	}
	if err := unmarshalRequiredTestFindings(result.Output, &findings); err != nil {
		return nil, fmt.Errorf("validate test analyzer findings: %w", err)
	}
	if len(tested) > 0 {
		findings.Tested = append(append([]string{}, tested...), findings.Tested...)
	}
	findings.TestedHeadSHA = sctx.Run.HeadSHA
	findings.Items = append(baselineFindings, findings.Items...)
	if baselineSummary != "" {
		findings.Summary = strings.TrimSpace(strings.Join([]string{baselineSummary, findings.Summary}, "\n"))
	}

	findings.Items = append(findings.Items, verdictFindings(findings)...)

	needsApproval := hasBlockingFindings(findings.Items)
	autoFixable := needsApproval

	// Record any new test files the agent wrote as informational (no-op)
	// findings. Their presence alone is not an actionable problem, so they
	// must not force the test step into approval when tests pass (issue #140).
	newTests := mergeNewTestFiles(newTestsFromFix, detectNewTestFiles(ctx, sctx.WorkDir))
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
		ExitCode:      baselineExitCode,
		FixSummary:    fixSummary,
	}, nil
}

func trustedTestInstructionsSection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.Test.Instructions)
	if instructions == "" {
		return ""
	}
	return "\nRepository live-validation runbook (trusted, from the default branch):\n" +
		sanitizePromptMultilineText(instructions) + "\n"
}

// verdictFindings turns the evidence turn's own verdict into findings, which
// is what stops a verdict from being decoration on a green step.
//
// The policy is deliberately asymmetric (captain's call C2 = a, plus the
// 2026-09-07 no-surface ask-user decision):
//
//   - "no-go" is an error finding, so hasBlockingFindings parks the step for a
//     decision. It is auto-fixable because a failed scenario is a defect the
//     fix round can attack, exactly like a failed configured test command;
//     escalating every failed scenario to a human instead would make the
//     contract too expensive to keep switched on.
//   - "inconclusive" is a warning finding: the change has a live-exercisable
//     surface but too little could be driven live to judge, which is a
//     question for the human rather than something a fix round can repair,
//     so it parks and asks.
//   - "no-surface" is a warning finding: the change itself has nothing
//     no-mistakes can drive live, so it parks and asks whether proceeding
//     without live validation is acceptable. It is not a silent pass and
//     not a hard fail. A change that claimed a pass/fail or drove anything
//     live cannot reach this branch (unmarshalRequiredTestFindings rejects
//     that masquerade).
//   - "go" adds nothing.
//
// Untested scenarios never produce a finding at any verdict. They are listed
// on the pull request (see the Testing section's scenario table) precisely so
// that reporting one honestly costs a contributor nothing; making them park
// would push the agent back towards guessing a pass.
func verdictFindings(findings Findings) []Finding {
	live, total := types.LiveScenarioCounts(findings.Scenarios)
	coverage := fmt.Sprintf("%d of %d scenarios were driven live against the product", live, total)
	switch findings.Verdict {
	case types.TestVerdictNoGo:
		return []Finding{{
			Severity:    types.FindingSeverityError,
			Action:      types.ActionAutoFix,
			Description: fmt.Sprintf("live validation verdict: no-go (%s)%s", coverage, failedScenarioSuffix(findings.Scenarios)),
		}}
	case types.TestVerdictInconclusive:
		return []Finding{{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Description: fmt.Sprintf("live validation verdict: inconclusive (%s)%s", coverage, untestedScenarioSuffix(findings.Scenarios)),
		}}
	case types.TestVerdictNoSurface:
		return []Finding{{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Description: fmt.Sprintf("this change has no live-validatable surface; proceed without live validation? (%s)%s", coverage, untestedScenarioReasonSuffix(findings.Scenarios)),
		}}
	default:
		return nil
	}
}

func failedScenarioSuffix(scenarios []types.TestScenario) string {
	return scenarioNameSuffix(scenarios, types.ScenarioResultFail, "failed")
}

func untestedScenarioSuffix(scenarios []types.TestScenario) string {
	return scenarioNameSuffix(scenarios, types.ScenarioResultUntested, "untested")
}

// untestedScenarioReasonSuffix names each untested scenario together with the
// reason it could not be driven, so a no-surface park carries why there is
// nothing to validate rather than only the scenario titles.
func untestedScenarioReasonSuffix(scenarios []types.TestScenario) string {
	var parts []string
	for _, scenario := range scenarios {
		if scenario.Result != types.ScenarioResultUntested {
			continue
		}
		name := strings.TrimSpace(scenario.Name)
		reason := strings.TrimSpace(scenario.Reason)
		switch {
		case name != "" && reason != "":
			parts = append(parts, name+": "+reason)
		case name != "":
			parts = append(parts, name)
		case reason != "":
			parts = append(parts, reason)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

func scenarioNameSuffix(scenarios []types.TestScenario, result, label string) string {
	var names []string
	for _, scenario := range scenarios {
		if scenario.Result == result {
			names = append(names, strings.TrimSpace(scenario.Name))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "; " + label + ": " + strings.Join(names, ", ")
}

// mergeNewTestFiles unions the test files a fix turn created with the ones
// still uncommitted at the evidence turn, preserving order and dropping
// duplicates. Both halves are needed: a fix turn's files are already committed
// by the time the evidence turn looks (detectNewTestFiles reads uncommitted
// status), and the evidence turn's own files were never in the fix turn's view.
func mergeNewTestFiles(fromFix, fromEvidence []string) []string {
	seen := make(map[string]bool, len(fromFix)+len(fromEvidence))
	var merged []string
	for _, group := range [][]string{fromFix, fromEvidence} {
		for _, f := range group {
			if seen[f] {
				continue
			}
			seen[f] = true
			merged = append(merged, f)
		}
	}
	return merged
}

func testAgentContext(sctx *pipeline.StepContext) (context.Context, context.CancelFunc, time.Duration) {
	timeout := config.DefaultTestAgentTimeout
	if sctx != nil && sctx.Config != nil && sctx.Config.TestAgentTimeout > 0 {
		timeout = sctx.Config.TestAgentTimeout
	}
	ctx, cancel := context.WithTimeoutCause(sctx.Ctx, timeout, errTestAgentTimeout)
	return ctx, cancel, timeout
}

var errTestAgentTimeout = errors.New("test agent timeout")

// testAgentError renders a Test-invocation budget expiry. It keeps the agent's
// own error rather than replacing it with the bare context cause: for a native
// agent that error carries the killed subprocess's exit status and stderr, and
// is the only account of what the process was doing when the budget ran out.
func testAgentError(ctx context.Context, timeout time.Duration, prefix string, err error) error {
	if timeout > 0 && errors.Is(context.Cause(ctx), errTestAgentTimeout) {
		if err == nil {
			err = context.Cause(ctx)
		}
		return fmt.Errorf("%s timed out after %s: %w", prefix, timeout, err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}
