package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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

type pipelineUpdateGroup struct {
	header string
	units  []string
	footer string
}

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
	content, err := s.buildPRContent(sctx, branch, baseSHA)
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
%s
- When including a scope, it MUST be a real package/module name that exists in the codebase (for example, a directory under internal/, cmd/, or the equivalent top-level grouping for this project), identified by inspecting the changed paths. Pick the primary module affected by the change, not a secondary or incidental one.
- Keep the scope at a coarse level, not too granular: a codebase typically has fewer than 10 distinct scopes in use across its history. Prefer a broad module name (e.g. "daemon", "pipeline", "cli") over a narrow file or sub-feature name. If you cannot confidently identify a real primary module, omit the scope and use "type: description".
- Body: a "## What Changed" section in GitHub-flavored markdown. 1-3 concise bullet points describing the concrete changes in this branch (what code/behavior shifted), not the user's motivation. Do not include Intent, Risk Assessment, Testing, or Pipeline sections - those are prepended/appended separately. The body value must be plain markdown text, never a JSON object or serialized JSON string.
- Do not invent tests or behavior.

Commit history:
%s

Diff stat:
%s%s%s%s`, branch, baseSHA, sctx.Run.HeadSHA, sctx.Repo.DefaultBranch, conventional.ReleaseTypeRule, commitLog, diffStat, pipelineContext, userIntentPromptSection(sctx), executionContextPromptSection())

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		slog.Warn("agent failed for PR content, using fallback", "error", err)
		return fallbackPRContent(sctx, branch, commitLog, riskLine, testingMD, pipelineMD), nil
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
				content.Body = buildPRBody(content.Body, riskLine, testingMD, pipelineMD, sctx)
				return content, nil
			}
		}
	}

	return fallbackPRContent(sctx, branch, commitLog, riskLine, testingMD, pipelineMD), nil
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
	testingMD := BuildTestingSummaryForPR(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir)
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
	return appendGeneratedSectionsToCleanBody(body, riskLine, testingMD, pipelineMD)
}

func buildPRBody(body, riskLine, testingMD, pipelineMD string, sctx *pipeline.StepContext) string {
	body = stripGeneratedSections(body)
	body = prependIntentSection(body, sctx)
	return appendGeneratedSectionsToCleanBody(body, riskLine, testingMD, pipelineMD)
}

func appendGeneratedSectionsToCleanBody(body, riskLine, testingMD, pipelineMD string) string {
	generatedSections := generatedEssentialSections(riskLine, testingMD)
	prefix := body + generatedSections
	if pipelineMD == "" {
		return essentialPRBodyWithinLimit(body, generatedSections)
	}

	separator := ""
	if prefix != "" {
		separator = "\n\n"
	}
	if len(prefix+separator+pipelineMD) <= maxPullRequestBodyBytes {
		return prefix + separator + pipelineMD
	}

	prefix = essentialPRBodyWithinPipelineBudget(body, generatedSections, pipelineMD)
	return appendPipelineSectionWithinLimit(prefix, pipelineMD)
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

func essentialPRBodyWithinPipelineBudget(body, generatedSections, pipelineMD string) string {
	minPipeline := minimumPipelineOmissionSection(pipelineMD)
	if minPipeline == "" {
		return essentialPRBodyWithinLimit(body, generatedSections)
	}

	prefixBudget := maxPullRequestBodyBytes - len(minPipeline)
	if body != "" || generatedSections != "" {
		prefixBudget -= len("\n\n")
	}
	if prefixBudget <= 0 || len(generatedSections) > prefixBudget {
		return essentialPRBodyWithinLimit(body, generatedSections)
	}
	return essentialPRBodyWithinBudget(body, generatedSections, prefixBudget)
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

func appendPipelineSectionWithinLimit(prefix, pipelineMD string) string {
	separator := ""
	if prefix != "" {
		separator = "\n\n"
	}
	full := prefix + separator + pipelineMD
	if len(full) <= maxPullRequestBodyBytes {
		return full
	}

	pipelineBudget := maxPullRequestBodyBytes - len(prefix) - len(separator)
	if pipelineBudget <= 0 {
		return truncateEssentialPRBodyIfNeeded(prefix)
	}

	truncatedPipeline := truncatePipelineSection(pipelineMD, pipelineBudget)
	if truncatedPipeline == "" {
		return prefix
	}
	candidate := prefix + separator + truncatedPipeline
	if len(candidate) <= maxPullRequestBodyBytes {
		return candidate
	}
	if len(prefix) <= maxPullRequestBodyBytes {
		return prefix
	}
	return truncateEssentialPRBodyIfNeeded(prefix)
}

func truncatePipelineSection(pipelineMD string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(pipelineMD) <= maxBytes {
		return pipelineMD
	}

	header, updates := splitPipelineSectionHeader(pipelineMD)
	groups := parsePipelineUpdateGroups(updates)
	totalUnits := countPipelineUpdateUnits(groups)
	if totalUnits == 0 {
		return pipelineOmissionSectionWithinLimit(header, 0, maxBytes)
	}

	for omitted := 1; omitted < totalUnits; omitted++ {
		candidate := renderPipelineWithOmittedUpdates(header, groups, omitted)
		if len(candidate) <= maxBytes {
			return candidate
		}
	}

	if candidate := renderPipelineWithTruncatedLatestUpdate(header, groups, maxBytes); candidate != "" {
		return candidate
	}

	return pipelineOmissionSectionWithinLimit(header, totalUnits, maxBytes)
}

func minimumPipelineOmissionSection(pipelineMD string) string {
	header, updates := splitPipelineSectionHeader(pipelineMD)
	totalUnits := countPipelineUpdateUnits(parsePipelineUpdateGroups(updates))
	return header + pipelineUpdatesOmissionMarker(totalUnits) + "\n"
}

func pipelineOmissionSectionWithinLimit(header string, omitted, maxBytes int) string {
	markerOnly := header + pipelineUpdatesOmissionMarker(omitted) + "\n"
	if len(markerOnly) <= maxBytes {
		return markerOnly
	}
	return ""
}

func splitPipelineSectionHeader(pipelineMD string) (string, string) {
	const heading = "## Pipeline\n\n"
	if !strings.HasPrefix(pipelineMD, heading) {
		return "", pipelineMD
	}

	rest := pipelineMD[len(heading):]
	introEnd := strings.Index(rest, "\n\n")
	if introEnd < 0 {
		return heading, rest
	}

	headerEnd := len(heading) + introEnd + len("\n\n")
	return pipelineMD[:headerEnd], pipelineMD[headerEnd:]
}

func parsePipelineUpdateGroups(updates string) []pipelineUpdateGroup {
	var groups []pipelineUpdateGroup
	rest := updates
	for strings.TrimSpace(rest) != "" {
		rest = strings.TrimLeft(rest, "\n")
		if strings.HasPrefix(rest, "<details>") {
			end := strings.Index(rest, "</details>")
			if end >= 0 {
				end += len("</details>")
				if end < len(rest) && rest[end] == '\n' {
					end++
				}
				groups = append(groups, parsePipelineDetailsGroup(rest[:end]))
				rest = rest[end:]
				continue
			}
		}

		nextDetails := strings.Index(rest, "\n<details>")
		raw := rest
		if nextDetails >= 0 {
			raw = rest[:nextDetails]
			rest = rest[nextDetails+1:]
		} else {
			rest = ""
		}
		units := splitPipelineUpdateUnits(raw)
		if len(units) > 0 {
			groups = append(groups, pipelineUpdateGroup{units: units})
		}
	}
	return groups
}

func parsePipelineDetailsGroup(raw string) pipelineUpdateGroup {
	footerStart := strings.LastIndex(raw, "</details>")
	summaryEnd := strings.Index(raw, "</summary>")
	if footerStart < 0 || summaryEnd < 0 || summaryEnd > footerStart {
		return pipelineUpdateGroup{units: splitPipelineUpdateUnits(raw)}
	}

	contentStart := summaryEnd + len("</summary>")
	if strings.HasPrefix(raw[contentStart:], "\n\n") {
		contentStart += len("\n\n")
	} else if strings.HasPrefix(raw[contentStart:], "\n") {
		contentStart++
	}

	footerEnd := footerStart + len("</details>")
	if footerEnd < len(raw) && raw[footerEnd] == '\n' {
		footerEnd++
	}

	return pipelineUpdateGroup{
		header: raw[:contentStart],
		units:  splitPipelineUpdateUnits(raw[contentStart:footerStart]),
		footer: raw[footerStart:footerEnd],
	}
}

func splitPipelineUpdateUnits(content string) []string {
	var units []string
	var b strings.Builder
	for _, line := range strings.SplitAfter(content, "\n") {
		b.WriteString(line)
		if strings.TrimSpace(line) != "" {
			continue
		}
		if strings.TrimSpace(b.String()) == "" {
			b.Reset()
			continue
		}
		units = append(units, b.String())
		b.Reset()
	}
	if strings.TrimSpace(b.String()) != "" {
		units = append(units, b.String())
	}
	return units
}

func countPipelineUpdateUnits(groups []pipelineUpdateGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.units)
	}
	return total
}

func renderPipelineWithOmittedUpdates(header string, groups []pipelineUpdateGroup, omitted int) string {
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}

	remainingOmitted := omitted
	wroteGroup := false
	for _, group := range groups {
		if remainingOmitted >= len(group.units) {
			remainingOmitted -= len(group.units)
			continue
		}

		start := remainingOmitted
		remainingOmitted = 0
		units := group.units[start:]
		if len(units) == 0 {
			continue
		}
		if wroteGroup {
			b.WriteString("\n")
		}
		b.WriteString(group.header)
		for _, unit := range units {
			b.WriteString(unit)
		}
		if group.footer != "" {
			last := units[len(units)-1]
			if !strings.HasSuffix(last, "\n\n") {
				if !strings.HasSuffix(last, "\n") {
					b.WriteString("\n")
				}
				b.WriteString("\n")
			}
		}
		b.WriteString(group.footer)
		wroteGroup = true
	}

	return b.String()
}

func renderPipelineWithTruncatedLatestUpdate(header string, groups []pipelineUpdateGroup, maxBytes int) string {
	group, unit, ok := latestPipelineUpdateUnit(groups)
	if !ok {
		return ""
	}

	totalUnits := countPipelineUpdateUnits(groups)
	omitted := totalUnits - 1
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}
	b.WriteString(group.header)
	prefix := b.String()

	footerSeparatorBytes := 0
	if group.footer != "" {
		footerSeparatorBytes = len("\n\n")
	}
	unitBudget := maxBytes - len(prefix) - len(group.footer) - footerSeparatorBytes
	if unitBudget <= 0 {
		return ""
	}

	marker := pipelineLatestUpdateTruncationMarker()
	truncatedUnit := truncatePipelineUpdateAtLineBoundary(unit, unitBudget, marker)
	if strings.TrimSpace(strings.Replace(truncatedUnit, marker, "", 1)) == "" {
		return ""
	}

	candidate := prefix + truncatedUnit
	if group.footer != "" {
		if !strings.HasSuffix(truncatedUnit, "\n\n") {
			if !strings.HasSuffix(truncatedUnit, "\n") {
				candidate += "\n"
			}
			candidate += "\n"
		}
		candidate += group.footer
	}
	if len(candidate) <= maxBytes {
		return candidate
	}
	return ""
}

func latestPipelineUpdateUnit(groups []pipelineUpdateGroup) (pipelineUpdateGroup, string, bool) {
	for i := len(groups) - 1; i >= 0; i-- {
		group := groups[i]
		for j := len(group.units) - 1; j >= 0; j-- {
			if strings.TrimSpace(group.units[j]) == "" {
				continue
			}
			return group, group.units[j], true
		}
	}
	return pipelineUpdateGroup{}, "", false
}

func pipelineUpdatesOmissionMarker(omitted int) string {
	rounds := "rounds"
	if omitted == 1 {
		rounds = "round"
	}
	return fmt.Sprintf("_... (%d earlier update %s omitted to keep the PR body within GitHub's %d-char limit; full history is in the run log.)_", omitted, rounds, githubPullRequestBodyHardLimitChars)
}

func pipelineLatestUpdateTruncationMarker() string {
	return fmt.Sprintf("_... (latest pipeline update truncated to keep the PR body within GitHub's %d-char limit; full history is in the run log.)_", githubPullRequestBodyHardLimitChars)
}

func truncateEssentialPRBodyIfNeeded(body string) string {
	if len(body) <= maxPullRequestBodyBytes {
		return body
	}
	return truncateTextAtLineBoundary(body, maxPullRequestBodyBytes, essentialPRBodyTruncationMarker())
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

		changed := false
		for i, section := range sections {
			otherSections := append([]string{}, sections[:i]...)
			otherSections = append(otherSections, sections[i+1:]...)
			otherBytes := len(joinPRBodySections(otherSections))
			sectionBudget := maxBytes - otherBytes
			if sectionBudget >= len(section) {
				continue
			}
			truncated := truncateTextAtLineBoundary(section, sectionBudget, marker)
			if len(truncated) >= len(section) {
				continue
			}
			sections[i] = truncated
			changed = true
			break
		}
		if !changed {
			return truncateTextAtLineBoundary(joined, maxBytes, marker)
		}
	}
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

	cut := strings.LastIndex(text[:available], "\n")
	if cut <= 0 {
		cut = available
	}
	return strings.TrimRight(text[:cut], "\n") + marker
}

func truncatePipelineUpdateAtLineBoundary(text string, maxBytes int, marker string) string {
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

	searchEnd := available
	if searchEnd < len(text) && text[searchEnd] == '\n' {
		searchEnd++
	}
	cut := strings.LastIndex(text[:searchEnd], "\n")
	if cut <= 0 {
		if len(marker) <= maxBytes {
			return strings.TrimLeft(marker, "\n")
		}
		return ""
	}
	return strings.TrimRight(text[:cut], "\n") + marker
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

func fallbackPRContent(sctx *pipeline.StepContext, branch, commitLog, riskLine, testingMD, pipelineMD string) prContent {
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
	body = buildPRBody(body, riskLine, testingMD, pipelineMD, sctx)
	return prContent{
		Title: title,
		Body:  body,
	}
}
