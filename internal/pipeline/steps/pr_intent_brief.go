package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const (
	maxPRIntentGoalRunes   = 200
	maxPRIntentBulletRunes = 220
	maxPRIntentBullets     = 3
	maxPRIntentBriefRunes  = 2500
)

type prIntentBrief struct {
	Goal       string   `json:"goal"`
	Why        []string `json:"why,omitempty"`
	Scope      []string `json:"scope,omitempty"`
	OutOfScope []string `json:"out_of_scope,omitempty"`
}

var prIntentBriefOnlySchema = json.RawMessage(`{
	"type": "object", "properties": {
		"intent_brief": {
			"type": "object", "properties": {
				"goal": {"type": "string", "maxLength": 200},
				"why": {"type": "array", "maxItems": 3, "items": {"type": "string", "maxLength": 220}},
				"scope": {"type": "array", "maxItems": 3, "items": {"type": "string", "maxLength": 220}},
				"out_of_scope": {"type": "array", "maxItems": 3, "items": {"type": "string", "maxLength": 220}}
			}, "required": ["goal"], "additionalProperties": false
		}
	}, "required": ["intent_brief"], "additionalProperties": false
}`)

func (s *PRStep) draftPRIntentBrief(sctx *pipeline.StepContext, branch, baseBranch, baseSHA string) error {
	paths, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return fmt.Errorf("read final branch diff for public Intent brief: %w", err)
	}
	inventory := prPromptInventory(sctx, paths, baseSHA)
	prompt := fmt.Sprintf(`Draft only the structured public Intent brief for the full final branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR base branch: %s

Use the original intent as review criteria, not as prose to copy. Ground claims in the intent and final diff. Return only the configured intent_brief object; do not include a PR title or body.

Final diff paths and statuses:
%s%s%s`, branch, baseSHA, sctx.Run.HeadSHA, baseBranch, inventory, prDraftIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))
	prompt += prIntentBriefPromptSection(sctx)
	prompt += agent.MemoryFilesRule
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prIntentBriefOnlySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return fmt.Errorf("draft configured public Intent brief: %w", err)
	}
	var response struct {
		IntentBrief *prIntentBrief `json:"intent_brief"`
	}
	if result == nil || json.Unmarshal(result.Output, &response) != nil {
		return fmt.Errorf("agent returned no valid configured public Intent brief")
	}
	return acceptPRIntentBrief(sctx, response.IntentBrief)
}

func needsPRIntentBrief(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Config != nil && strings.EqualFold(strings.TrimSpace(sctx.Config.PR.Intent.Format), "brief") && publicPRIntentSource(sctx) != ""
}

func prIntentBriefSections(sctx *pipeline.StepContext) []string {
	if sctx == nil || sctx.Config == nil || len(sctx.Config.PR.Intent.Sections) == 0 {
		return []string{"why", "scope", "out_of_scope"}
	}
	sections := make([]string, len(sctx.Config.PR.Intent.Sections))
	for i, section := range sctx.Config.PR.Intent.Sections {
		sections[i] = strings.ToLower(strings.TrimSpace(section))
	}
	return sections
}

func acceptPRIntentBrief(sctx *pipeline.StepContext, brief *prIntentBrief) error {
	if !needsPRIntentBrief(sctx) {
		return nil
	}
	if brief == nil {
		return fmt.Errorf("agent returned no configured public Intent brief")
	}
	rendered, err := renderPRIntentBrief(*brief, prIntentBriefSections(sctx))
	if err != nil {
		return err
	}
	sctx.PublicIntentBrief = rendered
	return nil
}

func renderPRIntentBrief(brief prIntentBrief, sections []string) (string, error) {
	goal, err := cleanPRIntentBriefLine(brief.Goal, maxPRIntentGoalRunes, "goal")
	if err != nil {
		return "", err
	}
	items := map[string][]string{
		"why":          brief.Why,
		"scope":        brief.Scope,
		"out_of_scope": brief.OutOfScope,
	}
	labels := map[string]string{
		"why":          "Why",
		"scope":        "Scope",
		"out_of_scope": "Out of scope",
	}
	var b strings.Builder
	b.WriteString(goal)
	for _, section := range sections {
		bullets, ok := items[section]
		if !ok {
			return "", fmt.Errorf("public Intent brief requested unknown section")
		}
		if len(bullets) > maxPRIntentBullets {
			return "", fmt.Errorf("public Intent brief %s has more than %d bullets", section, maxPRIntentBullets)
		}
		if len(bullets) == 0 {
			continue
		}
		b.WriteString("\n\n### ")
		b.WriteString(labels[section])
		for _, raw := range bullets {
			bullet, err := cleanPRIntentBriefLine(raw, maxPRIntentBulletRunes, section)
			if err != nil {
				return "", err
			}
			b.WriteString("\n- ")
			b.WriteString(bullet)
		}
	}
	result := b.String()
	if utf8.RuneCountInString(result) > maxPRIntentBriefRunes {
		return "", fmt.Errorf("public Intent brief exceeds %d characters", maxPRIntentBriefRunes)
	}
	return result, nil
}

func cleanPRIntentBriefLine(raw string, limit int, field string) (string, error) {
	line := intent.RedactSecrets(intent.StripAdversarial(sanitizePromptMultilineText(strings.TrimSpace(raw))))
	line = strings.TrimSpace(line)
	if line == "" || !utf8.ValidString(line) || strings.ContainsAny(line, "\r\n") || utf8.RuneCountInString(line) > limit {
		return "", fmt.Errorf("public Intent brief %s must be a non-empty single line of at most %d characters", field, limit)
	}
	for _, r := range line {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("public Intent brief %s contains a control character", field)
		}
	}
	if field == "goal" && strings.HasPrefix(line, "#") {
		return "", fmt.Errorf("public Intent brief goal must be plain text, not a Markdown heading")
	}
	return line, nil
}

func prIntentBriefPromptSection(sctx *pipeline.StepContext) string {
	if !needsPRIntentBrief(sctx) {
		return ""
	}
	var sections strings.Builder
	for _, section := range prIntentBriefSections(sctx) {
		sections.WriteString("\n- ")
		sections.WriteString(section)
	}
	return `

Public Intent brief (separate from the What Changed body):
- Include an intent_brief object with a one-line goal and only these selected bullet lists:` + sections.String() + `
- Use the original intent as review criteria; do not change or weaken it.
- Write for a teammate reviewing this PR. Keep every line concise and grounded in the intent and final diff.
- Do not invent a why, scope item, or exclusion. Leave a selected list empty when the source does not support it.
- Omit operational or orchestration details unless they are necessary to explain the reviewer-facing change.
- Do not put this brief into the body field; code renders the structured value under ## Intent.
`
}
