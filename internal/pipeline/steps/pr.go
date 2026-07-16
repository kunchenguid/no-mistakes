package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRStep creates or updates a pull request via the provider CLI or API.
type PRStep struct{}

type prContent struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var prContentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "Conventional commit PR title, e.g. fix(scope): short description"},
		"body": {"type": "string", "description": "GitHub-flavored markdown body starting with ## What Changed. Plain text, NOT JSON."}
	},
	"required": ["title", "body"]
}`)

const (
	githubPullRequestBodyHardLimitChars = 65536
	// Count bytes, not runes, so multi-byte markdown still stays under
	// GitHub's character limit with room for provider-side formatting drift.
	pullRequestBodySafetyBufferBytes = 2048
	maxPullRequestBodyBytes          = githubPullRequestBodyHardLimitChars - pullRequestBodySafetyBufferBytes
)

func (s *PRStep) Name() types.StepName { return types.StepPR }

func (s *PRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	branch := sctx.Run.Branch
	if strings.HasPrefix(branch, "refs/heads/") {
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}
	if branch == sctx.Repo.DefaultBranch {
		sctx.Log(fmt.Sprintf("skipping PR creation on default branch %s", branch))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if err := host.Available(ctx); err != nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %v", err))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	// Resolve the branch base so PR summaries cover the full branch delta.
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	content, err := s.buildPRContent(sctx, branch, baseSHA, scm.MaxPRBodyChars(provider))
	if err != nil {
		return nil, err
	}

	sctx.Log(fmt.Sprintf("checking for existing pull request on branch %s...", branch))
	existing, err := host.FindPR(ctx, branch, sctx.Repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		sctx.Log(fmt.Sprintf("pull request already exists: %s, updating...", describePR(existing)))
		updated, err := host.UpdatePR(ctx, existing, scm.PRContent(content))
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: failed to update PR: %v", err))
			updated = existing
		}
		if updated != nil && updated.URL != "" {
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, updated.URL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", updated.URL, "err", err)
			}
			return &pipeline.StepOutcome{PRURL: updated.URL}, nil
		}
		return &pipeline.StepOutcome{}, nil
	}

	sctx.Log("creating pull request...")
	created, err := host.CreatePR(ctx, branch, sctx.Repo.DefaultBranch, scm.PRContent(content))
	if err != nil {
		return nil, err
	}
	if created == nil || strings.TrimSpace(created.URL) == "" {
		return &pipeline.StepOutcome{}, nil
	}
	sctx.Log(fmt.Sprintf("created pull request: %s", created.URL))
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, created.URL); err != nil {
		slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", created.URL, "err", err)
	}
	return &pipeline.StepOutcome{PRURL: created.URL}, nil
}

func describePR(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if pr.URL != "" {
		return pr.URL
	}
	if pr.Number != "" {
		return "#" + pr.Number
	}
	return ""
}

func (s *PRStep) buildPRContent(sctx *pipeline.StepContext, branch, baseSHA string, bodyLimit int) (prContent, error) {
	ctx := sctx.Ctx
	commitLog, _ := git.Log(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA)
	diffStat, _ := git.Run(ctx, sctx.WorkDir, "diff", "--stat", baseSHA+".."+sctx.Run.HeadSHA)

	// Build the deterministic sections from step rounds.
	riskLine, testingMD := s.buildPipelineSection(sctx)

	prompt := fmt.Sprintf(`Draft a pull request title and summary for the full branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s

Rules:
- Cover the full branch delta, not just the latest commit.
- Title must use conventional commit format: "type(scope): description" or "type: description". Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert. Scope is optional. Do not capitalize the type. Do not use the raw branch name.
%s
- When including a scope, it MUST be a real package/module name that exists in the codebase (for example, a directory under internal/, cmd/, or the equivalent top-level grouping for this project), identified by inspecting the changed paths. Pick the primary module affected by the change, not a secondary or incidental one.
- Keep the scope at a coarse level, not too granular: a codebase typically has fewer than 10 distinct scopes in use across its history. Prefer a broad module name (e.g. "daemon", "pipeline", "cli") over a narrow file or sub-feature name. If you cannot confidently identify a real primary module, omit the scope and use "type: description".
- Body: a "## What Changed" section in GitHub-flavored markdown. 1-3 concise bullet points describing the concrete changes in this branch (what code/behavior shifted), not the user's motivation. Do not include Intent, Risk Assessment, or Testing sections - those are prepended/appended separately. The body value must be plain markdown text, never a JSON object or serialized JSON string.
- Do not invent tests or behavior.

Commit history:
%s

Diff stat:
%s%s%s`, branch, baseSHA, sctx.Run.HeadSHA, sctx.Repo.DefaultBranch, conventional.ReleaseTypeRule, commitLog, diffStat, userIntentPromptSection(sctx), executionContextPromptSection())

	prompt += prBodyBudgetPromptSection(bodyLimit)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		slog.Warn("agent failed for PR content, using fallback", "error", err)
		return fallbackPRContent(sctx, branch, commitLog, riskLine, testingMD, bodyLimit), nil
	}

	var content prContent
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &content); err == nil {
			content.Title = strings.TrimSpace(content.Title)
			content.Body = strings.TrimSpace(content.Body)
			content.Body = unwrapNestedPRBody(content.Body)
			content.Body = stripGeneratedSections(content.Body)
			if content.Title != "" && content.Body != "" {
				originalTitle := content.Title
				content.Title = conventional.TightenTitle(content.Title)
				if content.Title != originalTitle {
					slog.Warn("tightened agent PR title type", "from", originalTitle, "to", content.Title)
				}
				if bodyLimit > 0 {
					content.Body = assemblePRBody(sctx, content.Body, riskLine, testingMD, bodyLimit)
				} else {
					content.Body = buildPRBody(content.Body, riskLine, testingMD, sctx)
				}
				return content, nil
			}
		}
	}

	return fallbackPRContent(sctx, branch, commitLog, riskLine, testingMD, bodyLimit), nil
}

// buildPipelineSection queries step results and rounds from the DB and
// produces the deterministic risk and testing sections.
func (s *PRStep) buildPipelineSection(sctx *pipeline.StepContext) (string, string) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		slog.Warn("failed to query step results for pipeline summary", "error", err)
		return "", ""
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

	riskLine := BuildRiskLine(steps, rounds)
	testingMD := BuildTestingSummaryForPR(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir)
	return riskLine, testingMD
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

// appendGeneratedSections appends deterministic sections after the agent's body
// and applies the PR body length guard.
// prBodyBudgetPromptSection tells the drafting agent about a host's PR-body
// character cap so it keeps its "## What Changed" section short. The Intent,
// Risk, and Testing sections are appended deterministically, so the
// agent only controls a slice of the budget; this nudge keeps that slice small.
// Returns "" when the provider has no practical limit (bodyLimit <= 0).
func prBodyBudgetPromptSection(bodyLimit int) string {
	if bodyLimit <= 0 {
		return ""
	}
	return fmt.Sprintf("\n\n- This repository's host caps the entire PR description at %d characters. The Intent and Risk Assessment sections are appended automatically; a Testing section is included when budget allows. Keep the \"## What Changed\" section to a few short bullet points.", bodyLimit)
}

// assemblePRBody composes the final PR body from its sections and keeps it
// within bodyLimit (0 = unlimited). When the full body overruns the cap it
// first drops the Testing section - the only one that embeds artifact and log
// file contents and is therefore effectively unbounded - so an Azure DevOps PR
// sheds log dumps while keeping its Intent, What Changed, and Risk
// narrative intact. ClampPRBody is the final backstop when even that core
// overruns (e.g. an unusually long Intent).
func assemblePRBody(sctx *pipeline.StepContext, whatChanged, riskLine, testingMD string, bodyLimit int) string {
	full := prependIntentSection(appendGeneratedSections(whatChanged, riskLine, testingMD), sctx)
	if bodyLimit <= 0 || scm.PRBodyLen(full) <= bodyLimit {
		return full
	}
	if testingMD != "" {
		core := prependIntentSection(appendGeneratedSections(whatChanged, riskLine, ""), sctx)
		if scm.PRBodyLen(core) <= bodyLimit {
			return core
		}
		return scm.ClampPRBody(core, bodyLimit)
	}
	return scm.ClampPRBody(full, bodyLimit)
}

func appendGeneratedSections(body, riskLine, testingMD string) string {
	body = stripGeneratedSections(body)
	return appendGeneratedSectionsToCleanBody(body, riskLine, testingMD)
}

func buildPRBody(body, riskLine, testingMD string, sctx *pipeline.StepContext) string {
	body = stripGeneratedSections(body)
	body = prependIntentSection(body, sctx)
	return appendGeneratedSectionsToCleanBody(body, riskLine, testingMD)
}

func appendGeneratedSectionsToCleanBody(body, riskLine, testingMD string) string {
	generatedSections := generatedEssentialSections(riskLine, testingMD)
	return essentialPRBodyWithinLimit(body, generatedSections)
}

func generatedEssentialSections(riskLine, testingMD string) string {
	var b strings.Builder
	if riskLine != "" {
		b.WriteString("\n\n## Risk Assessment\n\n")
		b.WriteString(riskLine)
	}
	if testingMD != "" {
		b.WriteString("\n\n")
		b.WriteString(testingMD)
	}
	return b.String()
}

func essentialPRBodyWithinLimit(body, generatedSections string) string {
	return essentialPRBodyWithinBudget(body, generatedSections, maxPullRequestBodyBytes)
}

func essentialPRBodyWithinBudget(body, generatedSections string, maxBytes int) string {
	full := body + generatedSections
	if len(full) <= maxBytes {
		return full
	}
	if generatedSections == "" {
		return truncateTextAtLineBoundary(body, maxBytes, essentialPRBodyTruncationMarker())
	}

	bodyBudget := maxBytes - len(generatedSections)
	if bodyBudget <= 0 {
		return truncateTextAtLineBoundary(generatedSections, maxBytes, essentialPRBodyTruncationMarker())
	}
	return truncatePRBodySections(body, bodyBudget, essentialPRBodyTruncationMarker()) + generatedSections
}

func essentialPRBodyTruncationMarker() string {
	return fmt.Sprintf("_... (body truncated to keep the PR body within GitHub's %d-char limit.)_", githubPullRequestBodyHardLimitChars)
}

func truncatePRBodySections(body string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(body) <= maxBytes {
		return body
	}

	sections := splitPRBodySections(body)
	if len(sections) <= 1 {
		return truncateTextAtLineBoundary(body, maxBytes, marker)
	}

	for {
		joined := joinPRBodySections(sections)
		if len(joined) <= maxBytes {
			return joined
		}

		i := largestPRBodySectionIndex(sections)
		if i < 0 {
			return truncateTextAtLineBoundary(joined, maxBytes, marker)
		}
		sectionBudget := len(sections[i]) - (len(joined) - maxBytes)
		truncated := truncateTextAtLineBoundary(sections[i], sectionBudget, marker)
		if len(truncated) >= len(sections[i]) {
			return truncateTextAtLineBoundary(joined, maxBytes, marker)
		}
		sections[i] = truncated
	}
}

func largestPRBodySectionIndex(sections []string) int {
	index := -1
	length := 0
	for i, section := range sections {
		if len(section) <= length {
			continue
		}
		index = i
		length = len(section)
	}
	return index
}

func splitPRBodySections(body string) []string {
	if body == "" {
		return nil
	}

	var starts []int
	for start := 0; start < len(body); {
		end := strings.IndexByte(body[start:], '\n')
		lineEnd := len(body)
		next := len(body)
		if end >= 0 {
			lineEnd = start + end
			next = lineEnd + 1
		}
		if isPRBodySectionHeading(body[start:lineEnd]) {
			starts = append(starts, start)
		}
		start = next
	}
	if len(starts) == 0 || starts[0] != 0 {
		starts = append([]int{0}, starts...)
	}

	sections := make([]string, 0, len(starts))
	for i, start := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		sections = append(sections, body[start:end])
	}
	return sections
}

func isPRBodySectionHeading(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "### ")
}

func joinPRBodySections(sections []string) string {
	var b strings.Builder
	for _, section := range sections {
		if section == "" {
			continue
		}
		if b.Len() > 0 {
			current := b.String()
			if !strings.HasSuffix(current, "\n") {
				b.WriteString("\n")
			}
			current = b.String()
			if !strings.HasSuffix(current, "\n\n") {
				b.WriteString("\n")
			}
			section = strings.TrimLeft(section, "\n")
		}
		b.WriteString(section)
	}
	return b.String()
}

func truncateTextAtLineBoundary(text string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	if marker != "" {
		marker = "\n\n" + marker
	}
	available := maxBytes - len(marker)
	if available <= 0 {
		if len(marker) <= maxBytes {
			return strings.TrimLeft(marker, "\n")
		}
		return ""
	}

	available = utf8BoundaryBefore(text, available)
	cut := strings.LastIndex(text[:available], "\n")
	if cut <= 0 {
		cut = available
	}
	return strings.TrimRight(text[:cut], "\n") + marker
}

func utf8BoundaryBefore(text string, n int) int {
	if n >= len(text) {
		return len(text)
	}
	if n <= 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return n
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
	case "intent", "risk assessment", "testing", "tests", "pipeline":
		return true
	default:
		return false
	}
}

// prependIntentSection prepends a "## Intent" section sourced from the
// already-extracted user intent. The intent text is reused verbatim (after
// the same secret/adversarial scrubbing the agent prompt path applies)
// rather than being paraphrased by the agent. Returns body unchanged when
// no intent is available.
func prependIntentSection(body string, sctx *pipeline.StepContext) string {
	cleaned := cleanedUserIntent(sctx)
	if cleaned == "" {
		return body
	}
	section := "## Intent\n\n" + cleaned
	if strings.TrimSpace(body) == "" {
		return section
	}
	return section + "\n\n" + body
}

func fallbackPRContent(sctx *pipeline.StepContext, branch, commitLog, riskLine, testingMD string, bodyLimit int) prContent {
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
	} else {
		title = conventional.TightenTitle(title)
	}
	body := fmt.Sprintf("## What Changed\n\n%s", strings.TrimSpace(commitLog))
	if body == "## What Changed\n\n" {
		body = fmt.Sprintf("## What Changed\n\n- %s", title)
	}
	if bodyLimit > 0 {
		body = assemblePRBody(sctx, body, riskLine, testingMD, bodyLimit)
	} else {
		body = buildPRBody(body, riskLine, testingMD, sctx)
	}
	return prContent{
		Title: title,
		Body:  body,
	}
}
