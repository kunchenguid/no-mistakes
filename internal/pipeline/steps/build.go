package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BuildStep verifies that the change compiles before review and testing.
type BuildStep struct{}

func (s *BuildStep) Name() types.StepName { return types.StepBuild }

func (s *BuildStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}

	fixSummary := ""
	if sctx.Fixing {
		var err error
		fixSummary, err = s.executeFix(sctx)
		if err != nil {
			return nil, err
		}
	}

	command := strings.TrimSpace(sctx.Config.Commands.Build)
	source := "configured build command"
	if command == "" {
		source = "agent-driven build"
	}
	before, err := snapshotCleanBuildWorktree(sctx, source)
	if err != nil {
		return nil, err
	}
	if command == "" {
		outcome, err := s.runAgentBuild(sctx, before)
		if outcome != nil {
			outcome.FixSummary = fixSummary
		}
		return outcome, err
	}

	outcome, err := runBuildCommand(sctx, command, source, before, runStepShellCommand)
	if outcome != nil {
		outcome.FixSummary = fixSummary
	}
	return outcome, err
}

type buildWorktreeSnapshot struct {
	head   string
	status string
}

func snapshotCleanBuildWorktree(sctx *pipeline.StepContext, source string) (buildWorktreeSnapshot, error) {
	snapshot, err := snapshotBuildWorktree(sctx)
	if err != nil {
		return buildWorktreeSnapshot{}, fmt.Errorf("snapshot worktree before %s: %w", source, err)
	}
	if snapshot.status != "" {
		return buildWorktreeSnapshot{}, fmt.Errorf("Build requires a clean worktree before %s; found:\n%s", source, printableBuildStatus(snapshot.status))
	}
	return snapshot, nil
}

func snapshotBuildWorktree(sctx *pipeline.StepContext) (buildWorktreeSnapshot, error) {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return buildWorktreeSnapshot{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	status, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return buildWorktreeSnapshot{}, fmt.Errorf("read worktree status: %w", err)
	}
	return buildWorktreeSnapshot{head: head, status: string(status)}, nil
}

func assertBuildWorktreeUnchanged(sctx *pipeline.StepContext, source string, before buildWorktreeSnapshot) error {
	after, err := snapshotBuildWorktree(sctx)
	if err != nil {
		return fmt.Errorf("snapshot worktree after %s: %w", source, err)
	}
	if after == before {
		return nil
	}
	return fmt.Errorf("Build verification must be side-effect free; worktree changed after %s (before HEAD %s, after HEAD %s; status after:\n%s)",
		source, before.head, after.head, printableBuildStatus(after.status))
}

func printableBuildStatus(status string) string {
	if status == "" {
		return "(clean)"
	}
	const limit = 4096
	status = strings.ReplaceAll(status, "\x00", "\n")
	if len(status) > limit {
		return status[:limit] + "\n[status truncated]"
	}
	return status
}

type buildCommandRunner func(*pipeline.StepContext, string) (string, int, error)

func runBuildCommand(sctx *pipeline.StepContext, command, source string, before buildWorktreeSnapshot, runner buildCommandRunner) (*pipeline.StepOutcome, error) {
	sctx.Log(fmt.Sprintf("running build: %s", command))
	output, exitCode, err := runner(sctx, command)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", source, err)
	}
	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepBuild)
	if sideEffectErr := assertBuildWorktreeUnchanged(sctx, source, before); sideEffectErr != nil {
		if exitCode != 0 {
			return nil, fmt.Errorf("build failed with exit code %d: %s: %w", exitCode, projectedOutput, sideEffectErr)
		}
		return nil, sideEffectErr
	}
	if exitCode == 0 {
		return &pipeline.StepOutcome{}, nil
	}

	findings := Findings{
		Items: []Finding{{
			Severity:    "error",
			Description: fmt.Sprintf("build failed with exit code %d", exitCode),
			Action:      types.ActionAutoFix,
		}},
		Summary: projectedOutput,
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   true,
		Findings:      string(findingsJSON),
		ExitCode:      exitCode,
	}, nil
}

func (s *BuildStep) runAgentBuild(sctx *pipeline.StepContext, before buildWorktreeSnapshot) (*pipeline.StepOutcome, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	sctx.Log("no build command configured, asking agent to run the relevant build...")
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt: fmt.Sprintf(`Build or compile this repository's changed production code. Discover and run the smallest relevant build commands yourself.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Inspect the build metadata and changed production files, then run the appropriate focused build or compile commands.
- Do not run tests, linters, formatters, or static analysis.
- Do not modify source files, documentation, dependencies, or Git state. Remove transient build outputs before finishing.
- Always return the exact commands you ran in a non-empty "tested" array, even when the build passes.
- Report objective build failures with action "auto-fix". If no suitable build can be identified or run, report one "ask-user" finding explaining what is missing.
- If the build passes, return an empty findings array. Do not report successful checks as findings.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			historySection,
		),
		CWD:        sctx.WorkDir,
		JSONSchema: buildFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("agent run build: %w", err)
	}
	if unchangedErr := assertBuildWorktreeUnchanged(sctx, "agent-driven build", before); unchangedErr != nil {
		return nil, unchangedErr
	}
	if result.Output == nil {
		return buildEvidenceMissingOutcome("build agent returned no structured result"), nil
	}

	var findings Findings
	if err := json.Unmarshal(result.Output, &findings); err != nil {
		return buildEvidenceMissingOutcome("build agent returned an invalid structured result"), nil
	}
	if !hasExecutedBuildEvidence(findings.Tested) {
		for i := range findings.Items {
			findings.Items[i].Action = types.ActionAskUser
		}
		findings.Items = append(findings.Items, Finding{
			Severity:    "error",
			Description: "build agent did not report any executed build or compile commands",
			Action:      types.ActionAskUser,
		})
		if strings.TrimSpace(findings.Summary) == "" {
			findings.Summary = "build was not established"
		}
	}

	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(findings.Items),
		AutoFixable:   len(types.AutoFixableFindings(findings).Items) > 0,
		Findings:      string(findingsJSON),
	}, nil
}

// hasExecutedBuildEvidence reports whether the agent named at least one
// concretely executed build command. A blank-only "tested" array (e.g. [""] or
// ["   "]) carries no evidence that a build ran, so it is treated the same as an
// empty array and downgrades the build to "not established".
func hasExecutedBuildEvidence(tested []string) bool {
	for _, command := range tested {
		if strings.TrimSpace(command) != "" {
			return true
		}
	}
	return false
}

func buildEvidenceMissingOutcome(description string) *pipeline.StepOutcome {
	findings := Findings{
		Items: []Finding{{
			Severity:    "error",
			Description: description,
			Action:      types.ActionAskUser,
		}},
		Summary: description,
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, AutoFixable: false, Findings: string(findingsJSON)}
}

func (s *BuildStep) executeFix(sctx *pipeline.StepContext) (string, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	configuredCommand := strings.TrimSpace(sctx.Config.Commands.Build)
	if configuredCommand == "" {
		configuredCommand = "not configured; the Build agent will discover and run the relevant build after the repair"
	}
	prompt := fmt.Sprintf(`Fix the unresolved build or compile failure with the smallest root-cause change.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- build command: %s

Rules:
- Avoid unrelated refactors.
- Do NOT run build, test, lint, format, or static-analysis commands, and do NOT update documentation. The pipeline reruns Build after the repair.
- Remove transient build outputs or caches before finishing.
- Return JSON with one concise "summary" field suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		configuredCommand,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious build findings to address:\n" + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      "asking agent to fix build failure...",
		Prompt:          prompt,
		ErrorPrefix:     "agent fix build",
		FallbackSummary: "fix build failure",
	})
}
