package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRStep creates or updates a pull request via gh CLI.
type PRStep struct{}

type prContent struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var prContentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string"},
		"body": {"type": "string"}
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
	if provider == scm.ProviderUnknown || provider == scm.ProviderBitbucket {
		sctx.Log(fmt.Sprintf("skipping PR creation: provider %s is not supported yet", provider))
		return &pipeline.StepOutcome{}, nil
	}
	if !scm.CLIAvailable(provider) {
		sctx.Log(fmt.Sprintf("skipping PR creation: %s CLI is not installed", provider.CLIName()))
		return &pipeline.StepOutcome{}, nil
	}
	if !scm.AuthConfigured(ctx, provider, sctx.WorkDir) {
		sctx.Log(fmt.Sprintf("skipping PR creation: %s CLI is not authenticated", provider.CLIName()))
		return &pipeline.StepOutcome{}, nil
	}

	// Resolve the branch base so PR summaries cover the full branch delta.
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	content, err := s.buildPRContent(sctx, branch, baseSHA)
	if err != nil {
		return nil, err
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
	ctx := sctx.Ctx

	// Check if PR already exists for this branch
	sctx.Log(fmt.Sprintf("checking for existing PR on branch %s...", branch))
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch, "--json", "url", "--jq", ".url")
	cmd.Dir = sctx.WorkDir
	out, err := cmd.Output()
	if err == nil {
		prURL := strings.TrimSpace(string(out))
		if prURL != "" {
			sctx.Log(fmt.Sprintf("PR already exists: %s, updating...", prURL))

			editCmd := exec.CommandContext(ctx, "gh", "pr", "edit", branch, "--title", content.Title, "--body", content.Body)
			editCmd.Dir = sctx.WorkDir
			if editOut, editErr := editCmd.CombinedOutput(); editErr != nil {
				sctx.Log(fmt.Sprintf("warning: failed to update PR body: %s: %v", strings.TrimSpace(string(editOut)), editErr))
			}

			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", prURL, "err", err)
			}
			return &pipeline.StepOutcome{}, nil
		}
	}

	// Create PR
	sctx.Log("creating pull request...")

	cmd = exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", branch,
		"--base", sctx.Repo.DefaultBranch,
		"--title", content.Title,
		"--body", content.Body,
	)
	cmd.Dir = sctx.WorkDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}

	prURL := strings.TrimSpace(string(out))
	sctx.Log(fmt.Sprintf("created PR: %s", prURL))
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
		slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", prURL, "err", err)
	}

	return &pipeline.StepOutcome{}, nil
}

func (s *PRStep) executeGitLabMR(sctx *pipeline.StepContext, branch string, content prContent) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	sctx.Log(fmt.Sprintf("checking for existing merge request on branch %s...", branch))
	cmd := exec.CommandContext(ctx, "glab", "mr", "view", branch, "--output", "json")
	cmd.Dir = sctx.WorkDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		mrURL := extractSCMURL(out)
		if mrURL != "" {
			sctx.Log(fmt.Sprintf("merge request already exists: %s, updating...", mrURL))
			updateCmd := exec.CommandContext(ctx, "glab", "mr", "update", branch, "--title", content.Title, "--description", content.Body, "--yes")
			updateCmd.Dir = sctx.WorkDir
			if updateOut, updateErr := updateCmd.CombinedOutput(); updateErr != nil {
				sctx.Log(fmt.Sprintf("warning: failed to update merge request: %s: %v", strings.TrimSpace(string(updateOut)), updateErr))
			}
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, mrURL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", mrURL, "err", err)
			}
			return &pipeline.StepOutcome{}, nil
		}
	}

	sctx.Log("creating merge request...")
	cmd = exec.CommandContext(ctx, "glab", "mr", "create",
		"--source-branch", branch,
		"--target-branch", sctx.Repo.DefaultBranch,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	)
	cmd.Dir = sctx.WorkDir
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
	return &pipeline.StepOutcome{}, nil
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

	prompt := fmt.Sprintf(`Draft a pull request title and description for the full branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s

Rules:
- Cover the full branch delta, not just the latest commit.
- Title: concise and specific. Do not use the raw branch name.
- Body: GitHub-flavored markdown with a short summary and testing section.
- Do not invent tests or behavior.

Commit history:
%s

Diff stat:
%s`, branch, baseSHA, sctx.Run.HeadSHA, sctx.Repo.DefaultBranch, commitLog, diffStat)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentSchema,
		OnChunk:    sctx.Log,
	})
	if err != nil {
		slog.Warn("agent failed for PR content, using fallback", "error", err)
		return fallbackPRContent(branch, commitLog), nil
	}

	var content prContent
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &content); err == nil {
			content.Title = strings.TrimSpace(content.Title)
			content.Body = strings.TrimSpace(content.Body)
			if content.Title != "" && content.Body != "" {
				return content, nil
			}
		}
	}

	return fallbackPRContent(branch, commitLog), nil
}

func fallbackPRContent(branch, commitLog string) prContent {
	title := strings.TrimSpace(branch)
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
		title = "Update pull request"
	}
	body := strings.TrimSpace(commitLog)
	if body == "" {
		body = "Not run"
	}
	return prContent{
		Title: title,
		Body:  fmt.Sprintf("## Summary\n\n- %s\n\n## Testing\n\n- %s", title, body),
	}
}
