package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReviewStep reviews the diff for bugs, security issues, and doc gaps.
type ReviewStep struct{}

func (s *ReviewStep) Name() types.StepName { return types.StepReview }

func (s *ReviewStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	branch := sctx.Run.Branch
	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	reviewScope := fmt.Sprintf("branch changes between %s and %s", baseSHA, sctx.Run.HeadSHA)
	if sctx.Fixing {
		reviewScope = fmt.Sprintf("current worktree and HEAD changes relative to base commit %s (starting head %s)", baseSHA, sctx.Run.HeadSHA)
	}

	// In fix mode, ask the agent to fix issues first
	var fixSummary string
	if sctx.Fixing {
		previousFindings := sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		fixPrompt := fmt.Sprintf(
			`Investigate previous review findings and address legitimate ones.

Examine the relevant code yourself and apply fixes directly.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: %s
- default branch: %s
- ignore patterns: %s

Rules:
- Always start with double checking whether the findings are legitimate.
- Before changing code, identify whether each finding is a local defect or a symptom of a deeper design, abstraction, validation, ownership, or test-coverage flaw. Prefer the smallest correct root-cause fix within the changed area over patching only the reported line.
- If a narrow fix would leave the same class of bug likely elsewhere, fix the deepest practical cause instead.
- Avoid resolving a finding by removing or reverting the author's intentional code in their original 1st commit. If the original change introduced something on purpose, fix it forward (e.g. add validation, handle edge cases, tighten logic) rather than deleting it. Similarly, if the original change intentionally deleted or simplified code, do not restore or re-add the removed code unless the finding is a legitimate correctness, reliability, or security issue and the smallest reasonable fix happens to reintroduce a small amount of previously deleted logic. When in doubt about whether code is intentional, leave it and report the finding as unresolved.
- Do not add code comments explaining your fixes.
- Verify that the issues are resolved before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s

Previous review findings to address:
%s`,
			branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reviewScope,
			sctx.Repo.DefaultBranch,
			ignorePatterns,
			historySection,
			previousFindings,
		)
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			RequirePreviousFindings: true,
			MissingFindingsError:    "review fix requires previous review findings",
			LogMessage:              "asking agent to fix identified issues...",
			Prompt:                  fixPrompt,
			ErrorPrefix:             "agent fix",
			FallbackSummary:         "address review findings",
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	// Check whether there are any reviewable changed files after applying ignore patterns.
	var args []string
	if sctx.Fixing {
		args = []string{"diff", "--name-only", baseSHA}
	} else {
		args = []string{"diff", "--name-only", baseSHA + ".." + sctx.Run.HeadSHA}
	}
	changedFiles, err := git.Run(ctx, sctx.WorkDir, args...)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}

	hasReviewableChanges := false
	for _, path := range strings.Split(changedFiles, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		ignored := false
		for _, pattern := range sctx.Config.IgnorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			hasReviewableChanges = true
			break
		}
	}

	if !hasReviewableChanges {
		sctx.Log("no changes to review")
		noChangeFindings := Findings{
			RiskLevel:     "low",
			RiskRationale: "no reviewable changes",
		}
		findingsJSON, _ := json.Marshal(noChangeFindings)
		return &pipeline.StepOutcome{
			Findings:   string(findingsJSON),
			FixSummary: fixSummary,
		}, nil
	}

	sctx.Log("reviewing changes...")

	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	var findings Findings
	if sctx.Config.ReviewBackend == "autoreview" {
		reviewContext := fmt.Sprintf(`no-mistakes review context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: %s
- default branch: %s
- ignore patterns: %s%s`,
			branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reviewScope,
			sctx.Repo.DefaultBranch,
			ignorePatterns,
			historySection,
		)

		var err error
		findings, err = runAutoreview(sctx, baseSHA, reviewContext)
		if err != nil {
			return nil, fmt.Errorf("autoreview: %w", err)
		}
	} else {
		var err error
		findings, err = runAgentReview(sctx, agentReviewPrompt(branch, baseSHA, sctx.Run.HeadSHA, reviewScope, sctx.Repo.DefaultBranch, ignorePatterns, historySection))
		if err != nil {
			return nil, err
		}
	}

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, _ := json.Marshal(findings)

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}, nil
}

func agentReviewPrompt(branch, baseSHA, headSHA, reviewScope, defaultBranch, ignorePatterns, historySection string) string {
	return fmt.Sprintf(
		`Review the code changes and return structured findings with a risk assessment.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: %s
- default branch: %s
- ignore patterns: %s

Task:
- Read the relevant history and diff yourself.
- Focus findings on risks introduced by changed code, but inspect surrounding code, call sites, shared helpers, tests, and invariants when needed to understand root cause.
- Do NOT run tests during review. The pipeline has a dedicated test step after review.
- Analyze for bugs, risks, and code simplification opportunities.
- "Simplification" means reducing code complexity through non-functional refactoring (e.g. deduplication, clearer control flow). It does NOT mean removing features, changing product behavior, or stripping intentional user-facing output.
- Treat security issues, performance regressions, breaking changes, and insufficient error handling as risks.
- Do a full review pass before returning. Do not stop after the first valid finding. Continue inspecting the rest of the changed code until you have enumerated all material issues you can substantiate.

Rules:
- Anchor every finding to a specific file and one-indexed line number in the changed code when possible.
- Use severity "error" for problems that should absolutely not get merged, "warning" for things that are worth addressing but can be done in a follow up, and "info" for things that are nice to have.
- Be concise and actionable. No generic advice like "add more tests".
- Only comment on things that genuinely matter.
- Do NOT report styling, formatting, linting, compilation, or type-checking issues.
- If the change is clean, return an empty findings array.
- For each finding, set the action field to one of:
  - "ask-user": the finding is about functional requirements or product behavior, or otherwise challenges the author's deliberate intent. Even if it seems obviously wrong, we should ask the user for review. Examples: "this feature seems unnecessary", "this hardcoded value should be configurable", "this deletion looks wrong". When in doubt, default to "ask-user".
  - "auto-fix": the finding is a non-functional, non user-visible issue (correctness, error handling, security, performance, mechanical code quality) that can be safely fixed without any discussion about the author's intent.
  - "no-op": the finding is informational and does not require any action (e.g. noting a pattern, acknowledging a tradeoff).

Risk assessment (after listing all findings):
- Set risk_level to "low" if the change is well-bounded, mostly cosmetic, or straightforward with little ambiguity.
- Set risk_level to "medium" if the change has room to improve but is safe to merge first with concerns addressed as follow-ups.
- Set risk_level to "high" if the change should not be merged without explicit human approval - it is fundamental, risky, ambiguous, or has strong negative signals.
- Provide a one-sentence risk_rationale explaining why you chose that risk level.%s`,
		branch,
		baseSHA,
		headSHA,
		reviewScope,
		defaultBranch,
		ignorePatterns,
		historySection,
	)
}

func runAgentReview(sctx *pipeline.StepContext, prompt string) (Findings, error) {
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: reviewFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return Findings{}, fmt.Errorf("agent review: %w", err)
	}
	var findings Findings
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &findings); err != nil {
			sctx.Log("could not parse structured output, using text response")
			findings = Findings{Summary: result.Text}
		}
	}
	return findings, nil
}

type autoreviewReport struct {
	Findings           []autoreviewFinding `json:"findings"`
	OverallCorrectness string              `json:"overall_correctness"`
	OverallExplanation string              `json:"overall_explanation"`
	OverallConfidence  float64             `json:"overall_confidence"`
}

type autoreviewFinding struct {
	Title        string                 `json:"title"`
	Body         string                 `json:"body"`
	Priority     string                 `json:"priority"`
	Confidence   float64                `json:"confidence"`
	Category     string                 `json:"category"`
	CodeLocation autoreviewCodeLocation `json:"code_location"`
}

type autoreviewCodeLocation struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
}

func runAutoreview(sctx *pipeline.StepContext, baseSHA, reviewContext string) (Findings, error) {
	outputFile, err := os.CreateTemp("", "no-mistakes-autoreview-*.json")
	if err != nil {
		return Findings{}, fmt.Errorf("create output file: %w", err)
	}
	outputPath := outputFile.Name()
	if err := outputFile.Close(); err != nil {
		return Findings{}, fmt.Errorf("close output file: %w", err)
	}
	defer os.Remove(outputPath)

	args := []string{
		"--mode", "branch",
		"--base", baseSHA,
		"--engine", "codex",
		"--model", "gpt-5.5",
		"--thinking", "medium",
		"--json-output", outputPath,
	}
	if strings.TrimSpace(reviewContext) != "" {
		args = append(args, "--prompt", reviewContext)
	}

	cmd := stepCmd(sctx, autoreviewBinary(sctx), args...)
	shellenv.ConfigureShellCommand(cmd)
	out, runErr := cmd.CombinedOutput()
	if len(out) > 0 && sctx.LogChunk != nil {
		sctx.LogChunk(string(out))
	}

	raw, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		if runErr != nil {
			return Findings{}, fmt.Errorf("%w: %s", runErr, strings.TrimSpace(string(out)))
		}
		return Findings{}, fmt.Errorf("read output file: %w", readErr)
	}
	if strings.TrimSpace(string(raw)) == "" {
		if runErr != nil {
			return Findings{}, fmt.Errorf("%w: %s", runErr, strings.TrimSpace(string(out)))
		}
		return Findings{}, fmt.Errorf("empty output file")
	}

	var report autoreviewReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return Findings{}, fmt.Errorf("parse output JSON: %w", err)
	}
	if runErr != nil && !isAutoreviewPatchIncorrect(report) {
		return Findings{}, autoreviewRunError(runErr, out)
	}
	return convertAutoreviewReport(report), nil
}

func autoreviewRunError(runErr error, out []byte) error {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return fmt.Errorf("autoreview exited non-zero: %w", runErr)
	}
	return fmt.Errorf("autoreview exited non-zero: %w: %s", runErr, detail)
}

func autoreviewBinary(sctx *pipeline.StepContext) string {
	if value, ok := envValue(sctx.Env, "NM_AUTOREVIEW_BIN"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if value := strings.TrimSpace(os.Getenv("NM_AUTOREVIEW_BIN")); value != "" {
		return value
	}
	if _, err := exec.LookPath("autoreview"); err == nil {
		return "autoreview"
	}
	return "autoreview"
}

func convertAutoreviewReport(report autoreviewReport) Findings {
	findings := Findings{
		Summary:       summarizeAutoreviewReport(report),
		RiskLevel:     riskLevelForAutoreviewReport(report),
		RiskRationale: strings.TrimSpace(report.OverallExplanation),
	}
	if findings.RiskRationale == "" {
		findings.RiskRationale = "autoreview did not provide a rationale"
	}
	for i, item := range report.Findings {
		findings.Items = append(findings.Items, Finding{
			ID:          fmt.Sprintf("autoreview-%d", i+1),
			Severity:    autoreviewSeverity(item.Priority),
			File:        item.CodeLocation.FilePath,
			Line:        item.CodeLocation.Line,
			Description: autoreviewDescription(item),
			Action:      autoreviewAction(item.Priority),
		})
	}
	if isAutoreviewPatchIncorrect(report) && !hasBlockingFindings(findings.Items) {
		findings.Items = append(findings.Items, Finding{
			ID:          "autoreview-overall",
			Severity:    "error",
			Description: autoreviewOverallDescription(report),
			Action:      types.ActionAutoFix,
		})
	}
	return findings
}

func summarizeAutoreviewReport(report autoreviewReport) string {
	if len(report.Findings) == 0 && isAutoreviewPatchIncorrect(report) {
		return "autoreview reported patch is incorrect"
	}
	if len(report.Findings) == 0 {
		return "autoreview found no actionable issues"
	}
	return fmt.Sprintf("autoreview found %d actionable issue(s)", len(report.Findings))
}

func riskLevelForAutoreviewReport(report autoreviewReport) string {
	high := isAutoreviewPatchIncorrect(report)
	medium := false
	for _, item := range report.Findings {
		switch item.Priority {
		case "P0", "P1":
			high = true
		case "P2":
			medium = true
		}
	}
	if high {
		return "high"
	}
	if medium {
		return "medium"
	}
	return "low"
}

func isAutoreviewPatchIncorrect(report autoreviewReport) bool {
	return strings.EqualFold(strings.TrimSpace(report.OverallCorrectness), "patch is incorrect")
}

func autoreviewOverallDescription(report autoreviewReport) string {
	parts := []string{"patch is incorrect"}
	if explanation := strings.TrimSpace(report.OverallExplanation); explanation != "" {
		parts = append(parts, explanation)
	}
	return strings.Join(parts, "\n\n")
}

func autoreviewSeverity(priority string) string {
	switch priority {
	case "P0", "P1":
		return "error"
	case "P2":
		return "warning"
	default:
		return "info"
	}
}

func autoreviewAction(priority string) string {
	if priority == "P3" {
		return types.ActionNoOp
	}
	return types.ActionAutoFix
}

func autoreviewDescription(item autoreviewFinding) string {
	parts := []string{}
	if title := strings.TrimSpace(item.Title); title != "" {
		parts = append(parts, title)
	}
	if body := strings.TrimSpace(item.Body); body != "" {
		parts = append(parts, body)
	}
	if item.Category != "" {
		parts = append(parts, "category: "+item.Category)
	}
	return strings.Join(parts, "\n\n")
}

func sanitizedPreviousFindingsForPrompt(raw string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return sanitizePromptMultilineText(raw)
	}
	for i := range findings.Items {
		findings.Items[i].ID = sanitizePromptText(findings.Items[i].ID)
		findings.Items[i].Severity = sanitizePromptText(findings.Items[i].Severity)
		findings.Items[i].File = sanitizePromptText(findings.Items[i].File)
		findings.Items[i].Description = sanitizePromptMultilineText(findings.Items[i].Description)
		findings.Items[i].Source = sanitizePromptText(findings.Items[i].Source)
		findings.Items[i].UserInstructions = sanitizePromptMultilineText(findings.Items[i].UserInstructions)
	}
	findings.Summary = sanitizePromptMultilineText(findings.Summary)
	findings.RiskLevel = sanitizePromptText(findings.RiskLevel)
	findings.RiskRationale = sanitizePromptMultilineText(findings.RiskRationale)
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return sanitizePromptMultilineText(raw)
	}
	return encoded
}

func sanitizePromptText(text string) string {
	return strings.Join(strings.Fields(sanitizePromptMultilineText(text)), " ")
}

func sanitizePromptMultilineText(text string) string {
	text = strings.NewReplacer("<<<<<<<", " ", "=======", " ", ">>>>>>>", " ").Replace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.Join(strings.Fields(lines[i]), " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
