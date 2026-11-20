package steps

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// publicPRIntent returns the intent text published on the PR body, or "" when
// publication is suppressed. It is the single publication gate: every PR-body
// render path (ordinary drafting, fallback content, and template appendix)
// goes through it, so caller-side omission and the repository's trusted
// policy compose here and nowhere else.
//
// The control is tighten-only in both directions:
//
//   - The repository's trusted pr.publish_intent (default true) is the
//     ceiling. A caller can never publish intent on a repository whose
//     trusted config disabled it.
//   - The caller-side decision recorded on the run (runs.omit_intent: the
//     axi run --no-publish-intent flag, or the operator's global
//     intent.publish_intent: false default, folded together at run start)
//     can only remove the section, never restore it.
//
// The repository policy never touches the intent that reaches step prompts.
// The caller-side omission is different: it also WITHHOLDS the intent from the
// PR-drafting turns (prDraftIntentPromptSection), because a PR title or body
// drafted with the intent in context can paraphrase it into the public text,
// and there is deliberately no output filter or prose scanner to catch that.
// Review, test, document, lint, and CI-fix prompts keep the full intent
// under either signal.
func publicPRIntent(sctx *pipeline.StepContext) string {
	// A re-raise keeps the Intent the PR already publishes instead, as author
	// text outside the generated prose (keptPRIntent).
	if sctx != nil && sctx.PublishedPRIntent != "" {
		return ""
	}
	source := publicPRIntentSource(sctx)
	if source == "" {
		return ""
	}
	if sctx.Config != nil && strings.EqualFold(strings.TrimSpace(sctx.Config.PR.Intent.Format), "brief") {
		return strings.TrimSpace(sctx.PublicIntentBrief)
	}
	return source
}

func publicPRIntentSource(sctx *pipeline.StepContext) string {
	if !publishesPRIntent(sctx) {
		return ""
	}
	return cleanedUserIntent(sctx)
}

// keptPRIntent returns the live Intent section a re-raise republishes, even
// when this run's own intent is empty or different. It is author text: the
// caller places it byte for byte, never through neutralizeAttestationMarkers
// or a size helper, so a hand-written closing line such as "Closes #12" keeps
// working. The suppression checks still win, so a since-tightened policy
// removes it. A kept Intent carrying a pipeline attestation is refused, since
// it would sit ahead of this run's attestation and cannot be rewritten.
func keptPRIntent(sctx *pipeline.StepContext) (string, error) {
	if sctx == nil || sctx.PublishedPRIntent == "" || !publishesPRIntent(sctx) {
		return "", nil
	}
	if strings.Contains(sctx.PublishedPRIntent, pipelineAttestationCommentPrefix) {
		return "", fmt.Errorf("existing PR Intent section contains a pipeline attestation marker; refusing to republish it ahead of this run's attestation or to rewrite author text")
	}
	return sctx.PublishedPRIntent, nil
}

// publishedPRIntent returns the Intent section an existing PR body already
// publishes: from the owned appendix when the body has one, since the text
// above it is the author's own narrative, otherwise from the whole body.
func publishedPRIntent(body string) (string, bool) {
	if parts, err := parsePROwnedBody(body); err == nil && parts.managed {
		body = parts.appendix
	}
	return liveIntentSection(body)
}

// publishesPRIntent reports whether neither the repository policy nor the
// caller-side omission suppresses the Intent section.
func publishesPRIntent(sctx *pipeline.StepContext) bool {
	if sctx != nil && sctx.Config != nil && !sctx.Config.PR.PublishesIntent() {
		return false
	}
	return !runOmitsIntent(sctx)
}

// runOmitsIntent reports whether this run was started with the caller-side,
// tighten-only request to keep the generated Intent section out of the PR
// body. The decision is stamped on the run row at start, so it survives
// daemon restarts and reruns.
func runOmitsIntent(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Run != nil && sctx.Run.OmitIntent
}

// prDraftIntentPromptSection is the intent section for the PR-drafting turns
// (ordinary narrative, title-only fallback, and repository-template
// narrative). Under the caller-side omission it is empty, so those turns
// draft from the diff and commit messages only and no intent text can reach
// the public PR through a paraphrase. Every other step prompt keeps
// userIntentPromptSection unchanged.
func prDraftIntentPromptSection(sctx *pipeline.StepContext) string {
	if runOmitsIntent(sctx) {
		return ""
	}
	return userIntentPromptSection(sctx)
}

// prIntentEndMarker is the line the pipeline writes after every Intent
// section it publishes. It is a link reference definition, so no provider
// renders it, and it is the one boundary a re-raise trusts: a heading inside
// the Intent, even one spelled like a generated section, stays author text.
const prIntentEndMarker = "[//]: # (no-mistakes-intent-end)"

// intentSection renders a published Intent section ending in its marker.
func intentSection(text string) string {
	return "## Intent\n\n" + text + "\n\n" + prIntentEndMarker
}

// liveIntentSection returns the text of the first top-level "## Intent"
// section in an existing PR body, exactly as published, without the blank
// lines that separate it from its neighbours. ok is false when the body has no
// such section or it is empty. The section ends at prIntentEndMarker. A body
// published before that marker existed ends instead where the generator's own
// exact text begins (legacyIntentBoundary). Only a CommonMark ATX heading
// counts (markdownATXHeading), and a line in a fenced block is never a
// boundary.
func liveIntentSection(body string) (string, bool) {
	lines := strings.SplitAfter(body, "\n")
	var fence markdownFence
	start, end, legacyEnd := -1, -1, -1
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if fence.marker == 0 {
			if start < 0 {
				if level, title, ok := markdownATXHeading(line); ok && level == 2 && strings.EqualFold(strings.TrimRight(title, ":.!? "), "intent") {
					start = i + 1
					continue
				}
			} else if line == prIntentEndMarker {
				end = i
				break
			} else if legacyEnd < 0 && legacyIntentBoundary(lines, i) {
				legacyEnd = i
			}
		}
		fence.consume(line)
	}
	if start < 0 {
		return "", false
	}
	if end < 0 {
		end = legacyEnd
	}
	if end < 0 {
		end = len(lines)
	}
	intent := strings.Trim(strings.Join(lines[start:end], ""), "\r\n")
	if strings.TrimSpace(intent) == "" {
		return "", false
	}
	return intent, true
}

// legacyGeneratedHeadings are the exact heading lines the generator writes
// after an Intent section.
var legacyGeneratedHeadings = []string{"## Intent", "## What Changed", "## Risk Assessment", "## Testing", "## Pipeline", "## Evidence", issuesSectionHeading}

// legacyIntentBoundary reports whether lines[i] starts text the generator
// wrote after an Intent section published without prIntentEndMarker: one of
// its exact heading lines, the collapsed Validation block, the pipeline
// introduction, or the minimal appendix's one-line risk assessment directly
// ahead of that introduction.
func legacyIntentBoundary(lines []string, i int) bool {
	line := strings.TrimRight(lines[i], "\r\n")
	if slices.Contains(legacyGeneratedHeadings, line) || strings.HasPrefix(line, noMistakesPRSignature) {
		return true
	}
	details := strings.Split(validationDetailsOpen, "\n")
	if line == details[0] && i+1 < len(lines) && strings.TrimRight(lines[i+1], "\r\n") == details[1] {
		return true
	}
	next := ""
	for _, raw := range lines[i+1:] {
		if next = strings.TrimRight(raw, "\r\n"); strings.TrimSpace(next) != "" {
			break
		}
	}
	if !strings.HasPrefix(next, noMistakesPRSignature) {
		return false
	}
	for _, level := range []string{"low", "medium", "high", ""} {
		if strings.HasPrefix(line, riskEmoji(level)+" ") {
			return true
		}
	}
	return false
}

// markdownATXHeading parses line as a CommonMark ATX heading: at most three
// spaces of indentation, one to six "#", then a space, a tab, or the line end.
// title is the heading text without its optional closing "#" sequence.
func markdownATXHeading(line string) (level int, title string, ok bool) {
	rest := strings.TrimLeft(line, " ")
	if len(line)-len(rest) > 3 {
		return 0, "", false
	}
	for level < len(rest) && rest[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(rest) && rest[level] != ' ' && rest[level] != '\t') {
		return 0, "", false
	}
	title = strings.TrimSpace(rest[level:])
	if closing := strings.TrimRight(title, "#"); closing == "" || strings.HasSuffix(closing, " ") || strings.HasSuffix(closing, "\t") {
		title = strings.TrimSpace(closing)
	}
	return level, title, true
}
