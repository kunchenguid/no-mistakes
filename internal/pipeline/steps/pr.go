package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var conventionalTitleRe = regexp.MustCompile(
	`^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([^)]+\))?!?: .+`,
)

func isConventionalTitle(title string) bool {
	return conventionalTitleRe.MatchString(title)
}

// PRStep creates or updates a pull request via gh CLI.
type PRStep struct{}

type prContent struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var prContentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "Conventional commit PR title, e.g. fix(scope): short description"},
		"body": {"type": "string", "description": "GitHub-flavored markdown body starting with ## Summary. Plain text, NOT JSON."}
	},
	"required": ["title", "body"]
}`)

func (s *PRStep) Name() types.StepName { return types.StepPR }

func (s *PRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	branch := sctx.Run.Branch
	if strings.HasPrefix(branch, "refs/heads/") {
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}
	if branch == sctx.Repo.DefaultBranch {
		sctx.Log(fmt.Sprintf("skipping PR creation on default branch %s", branch))
		return &pipeline.StepOutcome{}, nil
	}
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown {
		sctx.Log(fmt.Sprintf("skipping PR creation: provider %s is not supported yet", provider))
		return &pipeline.StepOutcome{}, nil
	}
	if provider != scm.ProviderBitbucket {
		if !stepCLIAvailable(sctx, provider) {
			sctx.Log(fmt.Sprintf("skipping PR creation: %s CLI is not installed", provider.CLIName()))
			return &pipeline.StepOutcome{}, nil
		}
		if !stepAuthConfigured(sctx, provider) {
			sctx.Log(fmt.Sprintf("skipping PR creation: %s CLI is not authenticated", provider.CLIName()))
			return &pipeline.StepOutcome{}, nil
		}
	} else {
		if _, err := bitbucket.NewClientFromEnv(sctx.Env); err != nil {
			sctx.Log(fmt.Sprintf("skipping PR creation: %v", err))
			return &pipeline.StepOutcome{}, nil
		}
	}

	// Resolve the branch base so PR summaries cover the full branch delta.
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	content, err := s.buildPRContent(sctx, branch, baseSHA)
	if err != nil {
		return nil, err
	}
	if provider == scm.ProviderBitbucket {
		return s.executeBitbucketPR(sctx, branch, content)
	}

	switch provider {
	case scm.ProviderGitHub:
		return s.executeGitHubPR(sctx, branch, content)
	case scm.ProviderGitLab:
		return s.executeGitLabMR(sctx, branch, content)
	default:
		sctx.Log(fmt.Sprintf("skipping PR creation: provider %s is not supported yet", provider))
		return &pipeline.StepOutcome{}, nil
	}
}

func (s *PRStep) executeGitHubPR(sctx *pipeline.StepContext, branch string, content prContent) (*pipeline.StepOutcome, error) {
	// Check if PR already exists for this branch
	sctx.Log(fmt.Sprintf("checking for existing PR on branch %s...", branch))
	cmd := stepCmd(sctx, "gh", "pr", "view", branch, "--json", "url", "--jq", ".url")
	out, err := cmd.Output()
	if err == nil {
		prURL := strings.TrimSpace(string(out))
		if prURL != "" {
			sctx.Log(fmt.Sprintf("PR already exists: %s, updating...", prURL))

			editCmd := stepCmd(sctx, "gh", "pr", "edit", branch, "--title", content.Title, "--body", content.Body)
			if editOut, editErr := editCmd.CombinedOutput(); editErr != nil {
				sctx.Log(fmt.Sprintf("warning: failed to update PR body: %s: %v", strings.TrimSpace(string(editOut)), editErr))
			}

			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", prURL, "err", err)
			}
			return &pipeline.StepOutcome{PRURL: prURL}, nil
		}
	}

	// Create PR
	sctx.Log("creating pull request...")

	cmd = stepCmd(sctx, "gh", "pr", "create",
		"--head", branch,
		"--base", sctx.Repo.DefaultBranch,
		"--title", content.Title,
		"--body", content.Body,
	)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}

	prURL := strings.TrimSpace(string(out))
	sctx.Log(fmt.Sprintf("created PR: %s", prURL))
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
		slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", prURL, "err", err)
	}

	return &pipeline.StepOutcome{PRURL: prURL}, nil
}

func (s *PRStep) executeBitbucketPR(sctx *pipeline.StepContext, branch string, content prContent) (*pipeline.StepOutcome, error) {
	client, err := bitbucket.NewClientFromEnv(sctx.Env)
	if err != nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %v", err))
		return &pipeline.StepOutcome{}, nil
	}
	repo, err := bitbucket.ParseRepoRef(sctx.Repo.UpstreamURL)
	if err != nil {
		return nil, err
	}

	sctx.Log(fmt.Sprintf("checking for existing pull request on branch %s...", branch))
	existingPR, err := client.FindOpenPRBySourceBranch(sctx.Ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	if existingPR != nil {
		if existingPR.URL != "" {
			sctx.Log(fmt.Sprintf("pull request already exists: %s, updating...", existingPR.URL))
		} else {
			sctx.Log(fmt.Sprintf("pull request already exists: #%d, updating...", existingPR.ID))
		}
		updatedPR, err := client.UpdatePR(sctx.Ctx, repo, existingPR.ID, content.Title, content.Body)
		if err != nil {
			return nil, err
		}
		prURL := existingPR.URL
		if updatedPR != nil && updatedPR.URL != "" {
			prURL = updatedPR.URL
		}
		if prURL != "" {
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", prURL, "err", err)
			}
			return &pipeline.StepOutcome{PRURL: prURL}, nil
		}
		return &pipeline.StepOutcome{}, nil
	}

	sctx.Log("creating pull request...")
	createdPR, err := client.CreatePR(sctx.Ctx, repo, branch, sctx.Repo.DefaultBranch, content.Title, content.Body)
	if err != nil {
		return nil, err
	}
	if createdPR != nil && createdPR.URL != "" {
		sctx.Log(fmt.Sprintf("created pull request: %s", createdPR.URL))
		if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, createdPR.URL); err != nil {
			slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", createdPR.URL, "err", err)
		}
		return &pipeline.StepOutcome{PRURL: createdPR.URL}, nil
	}
	return &pipeline.StepOutcome{}, nil
}

func (s *PRStep) executeGitLabMR(sctx *pipeline.StepContext, branch string, content prContent) (*pipeline.StepOutcome, error) {
	sctx.Log(fmt.Sprintf("checking for existing merge request on branch %s...", branch))
	cmd := stepCmd(sctx, "glab", "mr", "view", branch, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err == nil {
		mrURL := extractSCMURL(out)
		if mrURL != "" {
			sctx.Log(fmt.Sprintf("merge request already exists: %s, updating...", mrURL))
			updateCmd := stepCmd(sctx, "glab", "mr", "update", branch, "--title", content.Title, "--description", content.Body, "--yes")
			if updateOut, updateErr := updateCmd.CombinedOutput(); updateErr != nil {
				sctx.Log(fmt.Sprintf("warning: failed to update merge request: %s: %v", strings.TrimSpace(string(updateOut)), updateErr))
			}
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, mrURL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", mrURL, "err", err)
			}
			return &pipeline.StepOutcome{PRURL: mrURL}, nil
		}
	}

	sctx.Log("creating merge request...")
	cmd = stepCmd(sctx, "glab", "mr", "create",
		"--source-branch", branch,
		"--target-branch", sctx.Repo.DefaultBranch,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab mr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	mrURL := extractSCMURL(out)
	if mrURL != "" {
		if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, mrURL); err != nil {
			slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", mrURL, "err", err)
		}
	}
	sctx.Log(fmt.Sprintf("created merge request: %s", mrURL))
	return &pipeline.StepOutcome{PRURL: mrURL}, nil
}

func extractSCMURL(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"url", "web_url", "webUrl"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *PRStep) buildPRContent(sctx *pipeline.StepContext, branch, baseSHA string) (prContent, error) {
	ctx := sctx.Ctx
	commitLog, _ := git.Log(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA)
	diffStat, _ := git.Run(ctx, sctx.WorkDir, "diff", "--stat", baseSHA+".."+sctx.Run.HeadSHA)

	// Build the deterministic sections from step rounds.
	pipelineMD, riskLine, testingMD := s.buildPipelineSection(sctx)

	// Build pipeline context for the agent prompt so it can reference findings in the summary.
	pipelineContext := ""
	if pipelineMD != "" {
		pipelineContext = fmt.Sprintf(`
Pipeline results (reference these naturally in the summary if relevant):
%s`, pipelineMD)
	}

	prompt := fmt.Sprintf(`Draft a pull request title and summary for the full branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s

Rules:
- Cover the full branch delta, not just the latest commit.
- Title must use conventional commit format: "type(scope): description" or "type: description". Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert. Scope is optional. Do not capitalize the type. Do not use the raw branch name.
- Body: a "## Summary" section in GitHub-flavored markdown. 1-3 concise bullet points describing what changed and why. Do not include Risk Assessment, Testing, or Pipeline sections - those are appended separately. The body value must be plain markdown text, never a JSON object or serialized JSON string.
- Do not invent tests or behavior.

Commit history:
%s

Diff stat:
%s%s`, branch, baseSHA, sctx.Run.HeadSHA, sctx.Repo.DefaultBranch, commitLog, diffStat, pipelineContext)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		slog.Warn("agent failed for PR content, using fallback", "error", err)
		return fallbackPRContent(branch, commitLog, riskLine, testingMD, pipelineMD), nil
	}

	var content prContent
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &content); err == nil {
			content.Title = strings.TrimSpace(content.Title)
			content.Body = strings.TrimSpace(content.Body)
			content.Body = unwrapNestedPRBody(content.Body)
			content.Body = stripGeneratedSections(content.Body)
			if content.Title != "" && content.Body != "" {
				if !isConventionalTitle(content.Title) {
					slog.Warn("agent PR title is not conventional commit format, prepending chore:", "title", content.Title)
					content.Title = "chore: " + content.Title
				}
				content.Body = appendGeneratedSections(content.Body, riskLine, testingMD, pipelineMD)
				return content, nil
			}
		}
	}

	return fallbackPRContent(branch, commitLog, riskLine, testingMD, pipelineMD), nil
}

// buildPipelineSection queries step results and rounds from the DB and
// produces the deterministic pipeline, risk, and testing sections.
func (s *PRStep) buildPipelineSection(sctx *pipeline.StepContext) (string, string, string) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		slog.Warn("failed to query step results for pipeline summary", "error", err)
		return "", "", ""
	}

	rounds := make(map[string][]*db.StepRound, len(steps))
	for _, sr := range steps {
		r, err := sctx.DB.GetRoundsByStep(sr.ID)
		if err != nil {
			slog.Warn("failed to query rounds for step", "step", sr.StepName, "error", err)
			continue
		}
		rounds[sr.ID] = r
	}

	pipelineMD, riskLine := BuildPipelineSummary(steps, rounds)
	testingMD := BuildTestingSummary(steps, rounds)
	return pipelineMD, riskLine, testingMD
}

// unwrapNestedPRBody detects when the agent returned the body as a
// serialized prContent JSON string and extracts the real markdown body.
func unwrapNestedPRBody(body string) string {
	if len(body) == 0 || body[0] != '{' {
		return body
	}
	var nested prContent
	if err := json.Unmarshal([]byte(body), &nested); err != nil {
		return body
	}
	if strings.TrimSpace(nested.Body) != "" {
		slog.Warn("agent returned nested JSON in PR body, unwrapping")
		return strings.TrimSpace(nested.Body)
	}
	return body
}

// appendGeneratedSections appends deterministic sections after the agent's body.
func appendGeneratedSections(body, riskLine, testingMD, pipelineMD string) string {
	body = stripGeneratedSections(body)
	if riskLine != "" {
		body += "\n\n## Risk Assessment\n\n" + riskLine
	}
	if testingMD != "" {
		body += "\n\n" + testingMD
	}
	if pipelineMD != "" {
		body += "\n\n" + pipelineMD
	}
	return body
}

func stripGeneratedSections(body string) string {
	if body == "" {
		return ""
	}

	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	skipping := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)

		if skipping {
			if strings.HasPrefix(line, "## ") {
				if isGeneratedSectionHeading(line) {
					continue
				}
				skipping = false
			} else {
				continue
			}
		}

		if isGeneratedSectionHeading(line) {
			skipping = true
			continue
		}

		out = append(out, raw)
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isGeneratedSectionHeading(line string) bool {
	if !strings.HasPrefix(strings.TrimSpace(line), "##") {
		return false
	}

	heading := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "##"))
	heading = strings.TrimRight(heading, ":.!? ")
	heading = strings.ToLower(heading)

	switch heading {
	case "risk assessment", "testing", "tests", "pipeline":
		return true
	default:
		return false
	}
}

func fallbackPRContent(branch, commitLog, riskLine, testingMD, pipelineMD string) prContent {
	title := ""
	for _, line := range strings.Split(commitLog, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.IndexByte(line, ' '); idx >= 0 && idx+1 < len(line) {
			title = strings.TrimSpace(line[idx+1:])
		}
		break
	}
	if title == "" {
		title = strings.TrimSpace(branch)
	}
	if title == "" {
		title = "chore: update pull request"
	} else if !isConventionalTitle(title) {
		title = "chore: " + title
	}
	body := fmt.Sprintf("## Summary\n\n%s", strings.TrimSpace(commitLog))
	if body == "## Summary\n\n" {
		body = fmt.Sprintf("## Summary\n\n- %s", title)
	}
	body = appendGeneratedSections(body, riskLine, testingMD, pipelineMD)
	return prContent{
		Title: title,
		Body:  body,
	}
}
