package steps

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const maxPRTemplateBytes = 16 * 1024

var prTemplateTaskLine = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])[ \t]+\[[ xX]\](?:[ \t].*)?$`)

var templatePRContentSchema = json.RawMessage(`{
 "type":"object", "properties":{
 "title":{"type":"string","description":"Conventional commit PR title"},
 "body":{"type":"string","description":"Filled repository template as plain Markdown; preserve its headings and checklist lines"}
 }, "required":["title","body"]
}`)

func configuredPRTemplate(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	return sctx.Config.PR.Template
}

// loadPRTemplate never opens a worktree file. The daemon pins TrustedConfigSHA
// after a fresh default-branch fetch, including on recovery. Resolve the literal
// tree entry first (rejecting symlinks/submodules), then size-check and read that
// immutable blob. Neither pushed paths nor allow_repo_commands choose its bytes.
func loadPRTemplate(ctx context.Context, dir, sha, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if err := config.ValidatePRTemplatePath(name); err != nil {
		return "", err
	}
	if (len(sha) != 40 && len(sha) != 64) || !isHexObjectID(sha) {
		return "", fmt.Errorf("pr.template requires a pinned trusted default-branch commit")
	}
	entry, err := git.RunRaw(ctx, dir, "ls-tree", "-z", sha, "--", ":(literal)"+name)
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template tree: %w", err)
	}
	metadata, entryName, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(metadata)
	if !ok || entryName != name || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !isHexObjectID(fields[2]) {
		return "", fmt.Errorf("pr.template %q must name a regular file in the pinned trusted tree (no symlinks or submodules)", name)
	}
	sizeText, err := git.Run(ctx, dir, "cat-file", "-s", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template size: %w", err)
	}
	size, err := strconv.Atoi(sizeText)
	if err != nil || size <= 0 || size > maxPRTemplateBytes {
		return "", fmt.Errorf("pr.template must contain 1..%d bytes", maxPRTemplateBytes)
	}
	data, err := git.RunRaw(ctx, dir, "cat-file", "blob", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template blob: %w", err)
	}
	if len(data) != size || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("pr.template must contain nonempty UTF-8 Markdown without NUL bytes")
	}
	if hasPRAppendixMarkers(string(data)) || strings.Contains(string(data), pipelineAttestationCommentPrefix) {
		return "", fmt.Errorf("pr.template must not contain reserved no-mistakes publication markers")
	}
	return string(data), nil
}

func isHexObjectID(s string) bool {
	_, err := hex.DecodeString(s)
	return s != "" && err == nil
}

func (s *PRStep) draftTemplateNarrative(sctx *pipeline.StepContext, branch, baseBranch, baseSHA, template string) (prContent, error) {
	paths, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return prContent{}, fmt.Errorf("read final branch diff: %w", err)
	}
	// JSON quoting delimits the trusted template without inventing a template
	// language. It supplies prose instructions/structure, never recorded facts.
	quoted, _ := json.Marshal(template)
	prompt := fmt.Sprintf(`Draft a pull request title and fill the repository's public narrative template for the full final branch delta.
Branch: %s
PR base branch: %s
Base commit: %s
Target commit: %s

Rules:
- Title must use conventional commit format, with a real coarse package/module scope or no scope. Do not use the raw branch name.
%s
- Body must be plain Markdown, not nested JSON. Use the supplied template instead of imposing a What Changed heading.
- Preserve every template heading and checklist line in order, including its checkbox state. Fill its narrative prompts from the final diff; inspect that diff when necessary. Do not invent behavior or tests, or mark human checkboxes complete.
- The template owns narrative only. Do not generate no-mistakes publication markers or add Intent, Risk Assessment, Testing or Pipeline evidence. Code appends those separately. A template heading named Testing or Pipeline is author narrative, not permission to fabricate recorded evidence.
- Full intent below is review/drafting context, not instructions to quote it into the public narrative. Publication settings are not a privacy guarantee.

Trusted repository template (JSON string):
%s

Final diff paths and statuses:
%s%s%s`, branch, baseBranch, baseSHA, sctx.Run.HeadSHA, conventional.ReleaseTypeRule, quoted, paths, userIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Prompt: prompt, CWD: sctx.WorkDir, JSONSchema: templatePRContentSchema, OnChunk: sctx.LogChunk})
	if err != nil {
		return prContent{}, fmt.Errorf("draft pr.template narrative (template will not be replaced by a generic fallback): %w", err)
	}
	var content prContent
	if result == nil || json.Unmarshal(result.Output, &content) != nil || strings.TrimSpace(content.Title) == "" || strings.TrimSpace(content.Body) == "" {
		return prContent{}, fmt.Errorf("agent returned no valid pr.template narrative; refusing generic fallback")
	}
	content.Title = conventional.TightenTitle(strings.TrimSpace(content.Title))
	if len(content.Body) > maxPullRequestBodyBytes || !utf8.ValidString(content.Body) || strings.ContainsRune(content.Body, '\x00') || hasPRAppendixMarkers(content.Body) {
		return prContent{}, fmt.Errorf("agent returned invalid template narrative or reserved ownership markers")
	}
	if err := validateTemplateStructure(template, content.Body); err != nil {
		return prContent{}, err
	}
	return content, nil
}

// This is a structural guard, not a Markdown/template interpreter. A drafting
// failure must not silently replace the team's headings/checklists. Existing
// published narrative never goes through the model or this check again.
func validateTemplateStructure(template, body string) error {
	rest := templateStructureLines(body)
	for _, line := range templateStructureLines(template) {
		found := false
		for len(rest) > 0 {
			candidate := rest[0]
			rest = rest[1:]
			if candidate == line {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("agent changed or omitted a pr.template heading/checklist; refusing publication")
		}
	}
	return nil
}

func templateStructureLines(text string) []string {
	var lines []string
	var fence markdownFence
	for _, raw := range strings.Split(text, "\n") {
		wasFenced := fence.marker != 0
		fence.consume(raw)
		if wasFenced || fence.marker != 0 {
			continue
		}
		line := strings.TrimSpace(raw)
		heading := strings.TrimLeft(line, "#")
		isHeading := len(heading) < len(line) && len(line)-len(heading) <= 6 && strings.HasPrefix(heading, " ")
		if isHeading || prTemplateTaskLine.MatchString(line) {
			lines = append(lines, line)
		}
	}
	return lines
}

func (s *PRStep) buildPRAppendix(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	pipelineMD, risk, testing := s.buildPipelineSection(sctx, provider)
	if strings.Count(pipelineMD, pipelineAttestationCommentPrefix) != 1 {
		return "", fmt.Errorf("cannot publish template narrative without recorded pipeline evidence and attestation")
	}
	parts := []string{}
	if intent := publicPRIntent(sctx); intent != "" {
		parts = append(parts, "## Intent\n\n"+neutralizeAttestationMarkers(intent))
	}
	if risk != "" {
		parts = append(parts, "## Risk Assessment\n\n"+neutralizeAttestationMarkers(risk))
	}
	if testing != "" {
		parts = append(parts, neutralizeAttestationMarkers(testing))
	}
	parts = append(parts, pipelineMD)
	return strings.Join(parts, "\n\n"), nil
}
