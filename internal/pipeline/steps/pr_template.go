package steps

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const maxPRTemplateBytes = 16 * 1024

var prTemplateH1Line = regexp.MustCompile(`^ {0,3}#(?:[ \t]|$)`)

var templatePRContentSchema = json.RawMessage(`{
 "type":"object", "properties":{
 "title":{"type":"string","description":"Concise pull request title text"},
 "body":{"type":"string","description":"Filled repository template as plain Markdown; preserve its top-level ATX # headings in order; best-effort fill applicable sections"}
 }, "required":["title","body"]
}`)

var templatePRContentBriefSchema = json.RawMessage(`{
 "type":"object", "properties":{
 "title":{"type":"string","description":"Concise pull request title text"},
 "body":{"type":"string","description":"Filled repository template as plain Markdown; preserve its top-level ATX # headings in order; best-effort fill applicable sections"},
 "intent_brief":{"type":"object","properties":{
   "goal":{"type":"string","maxLength":200},
   "why":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":220}},
   "scope":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":220}},
   "out_of_scope":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":220}}
 },"required":["goal","why","scope","out_of_scope"],"additionalProperties":false}
 }, "required":["title","body","intent_brief"]
}`)

func supportsPRTemplates(provider scm.Provider) bool {
	// Provider plugins implement raw content reads through the required
	// "pr view" subcommand, which is the whole contract templates and
	// pre-push attestation need.
	if provider.IsPlugin() {
		return true
	}
	switch provider {
	case scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderAzureDevOps, scm.ProviderBitbucket, scm.ProviderOrigin:
		return true
	default:
		return false
	}
}

func configuredPRTemplate(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	return sctx.Config.PR.Template
}

// loadPRTemplate never opens a worktree file: readTrustedTreeFile reads the
// literal tree entry from the pinned TrustedConfigSHA.
func loadPRTemplate(ctx context.Context, dir, sha, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	data, err := readTrustedTreeFile(ctx, dir, sha, "pr.template", name, maxPRTemplateBytes)
	if err != nil {
		return "", err
	}
	if hasPRAppendixMarkers(data) || strings.Contains(data, pipelineAttestationCommentPrefix) {
		return "", fmt.Errorf("pr.template must not contain reserved no-mistakes publication markers")
	}
	return data, nil
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
	inventory := prPromptInventory(sctx, paths, baseSHA)
	// JSON quoting delimits the trusted template without inventing a template
	// language. It supplies prose instructions/structure, never recorded facts.
	quoted, _ := json.Marshal(template)
	titleRules := prTitlePromptRules(sctx)
	scopeRules := prTitleScopeRules(sctx)
	prompt := fmt.Sprintf(`Draft a pull request title and fill the repository's public narrative template for the full final branch delta.
Branch: %s
PR base branch: %s
Base commit: %s
Target commit: %s

Rules:
%s
%s
- Body must be plain Markdown, not nested JSON. Use the supplied template instead of imposing a What Changed heading.
- Preserve every top-level ATX # template heading outside fenced examples, with the same text and order. Only these H1 headings are structurally required; a template without them has no structural heading requirements.
- Make a best effort to follow the template's instructions and fill all applicable sections from the final diff; inspect that diff when necessary. Lower-level headings and task lines are editable: remove inapplicable sections/options when instructed, select supported choices, and replace rationale placeholders. Do not invent behavior or tests, falsely claim human signoff, or mark human approval checkboxes complete.
- The template owns narrative only. Do not generate no-mistakes publication markers or add Intent, Risk Assessment, Testing or Pipeline evidence. Code appends those separately. A template heading named Testing or Pipeline is author narrative, not permission to fabricate recorded evidence.
- Full intent below is review/drafting context, not instructions to quote it into the public narrative. Publication settings are not a privacy guarantee.

Trusted repository template (JSON string):
%s

Final diff paths and statuses:
%s%s%s`, branch, baseBranch, baseSHA, sctx.Run.HeadSHA, titleRules, scopeRules, quoted, inventory, prDraftIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))
	prompt += prIntentBriefPromptSection(sctx)
	prompt += agent.MemoryFilesRule
	schema := templatePRContentSchema
	if needsPRIntentBrief(sctx) {
		schema = templatePRContentBriefSchema
	}
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Prompt: prompt, CWD: sctx.WorkDir, JSONSchema: schema, OnChunk: sctx.LogChunk})
	if err != nil {
		return prContent{}, fmt.Errorf("draft pr.template narrative (template will not be replaced by a generic fallback): %w", err)
	}
	var content prContent
	if result == nil || json.Unmarshal(result.Output, &content) != nil || (!s.titleReplaced && strings.TrimSpace(content.Title) == "") || strings.TrimSpace(content.Body) == "" {
		return prContent{}, fmt.Errorf("agent returned no valid pr.template narrative; refusing generic fallback")
	}
	if err := acceptPRIntentBrief(sctx, content.IntentBrief); err != nil {
		return prContent{}, err
	}
	if !s.titleReplaced {
		content.Title, err = renderPRTitle(sctx, strings.TrimSpace(content.Title))
		if err != nil {
			return prContent{}, err
		}
	}
	if len(content.Body) > maxPullRequestBodyBytes || !utf8.ValidString(content.Body) || strings.ContainsRune(content.Body, '\x00') || hasPRAppendixMarkers(content.Body) {
		return prContent{}, fmt.Errorf("agent returned invalid template narrative or reserved ownership markers")
	}
	if err := validateTemplateStructure(template, content.Body); err != nil {
		return prContent{}, err
	}
	return content, nil
}

// This is a structural guard, not a Markdown/template interpreter. A drafting
// failure must not silently replace the team's H1 text/order. Subordinate
// completion is best effort, not an enforced policy. No H1s means no structural
// requirements. Existing published narrative never goes through this check again.
func validateTemplateStructure(template, body string) error {
	if _, missing := missingTemplateHeading(template, body); missing {
		return fmt.Errorf("agent changed, omitted or reordered a pr.template top-level # heading; refusing publication")
	}
	return nil
}

// missingTemplateHeading returns the first template H1 the body does not keep
// in order.
func missingTemplateHeading(template, body string) (string, bool) {
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
			return line, true
		}
	}
	return "", false
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
		// Match the raw indentation: four spaces/tabs are code, not H1s.
		// Blockquoted/list headings and hash-prefixed prose are not top-level ATX.
		if prTemplateH1Line.MatchString(raw) {
			lines = append(lines, line)
		}
	}
	return lines
}

func (s *PRStep) buildPRAppendix(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	if needsPRIntentBrief(sctx) && strings.TrimSpace(sctx.PublicIntentBrief) == "" {
		return "", fmt.Errorf("cannot publish configured public Intent brief without a valid draft")
	}
	evidence, err := s.recordedPREvidence(sctx, provider)
	if err != nil {
		return "", err
	}
	return joinBlocks(prLeadSections(sctx, provider), evidence), nil
}

func (s *PRStep) recordedPREvidence(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	pipelineMD, risk, testing, evidence := s.buildPipelineSectionFor(sctx, provider, true)
	if strings.Count(pipelineMD, pipelineAttestationCommentPrefix) != 1 {
		return "", fmt.Errorf("cannot publish template narrative without recorded pipeline evidence and attestation")
	}
	return joinBlocks(appendixEvidence(appendixMode(sctx), prBodyFlavorFor(provider), risk, testing, pipelineMD, provider), neutralizeAttestationMarkers(provider, evidence)), nil
}

// publishKeptDraft stores the ordinary narrative above the full owned appendix
// (Intent included), whichever drafting path produced the narrative.
// A later update refreshes that appendix through the template preservation path.
func (s *PRStep) publishKeptDraft(sctx *pipeline.StepContext, provider scm.Provider, title, narrative string, bodyLimit int) (prContent, error) {
	evidence, err := s.buildPRAppendix(sctx, provider)
	if err != nil {
		return prContent{}, err
	}
	narrative = neutralizeAttestationMarkers(resolvedProviderForBody(sctx), stripGeneratedSections(narrative))
	evidence = appendIssuesSection(evidence, issuesSection(sctx, narrative))
	return composeOwnedPRContent(prOwnedBody{before: narrative}, title, evidence, bodyLimit, sctx.WorkDir, nmWorktreesRoot())
}
