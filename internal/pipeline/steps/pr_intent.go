package steps

import (
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
	// A re-raise keeps the Intent the PR already publishes, even when this
	// run's own intent is empty or different. The suppression checks still
	// win, so a since-tightened policy removes it.
	if sctx != nil && sctx.PublishedPRIntent != "" && publishesPRIntent(sctx) {
		return sctx.PublishedPRIntent
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

// liveIntentSection returns the text of the first top-level "## Intent"
// section in an existing PR body, exactly as published: everything after the
// heading up to the next "#" or "##" heading outside a fenced block, without
// the blank lines that separate it from its neighbours. ok is false when the
// body has no such section or it is empty.
func liveIntentSection(body string) (string, bool) {
	lines := strings.SplitAfter(body, "\n")
	var fence markdownFence
	start := -1
	var text strings.Builder
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if trimmed := strings.TrimSpace(line); fence.marker == 0 && (strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ")) {
			if start >= 0 {
				break
			}
			if strings.HasPrefix(trimmed, "## ") && strings.EqualFold(strings.TrimRight(strings.TrimSpace(trimmed[3:]), ":.!? "), "intent") {
				start = 0
				continue
			}
		}
		fence.consume(line)
		if start >= 0 {
			text.WriteString(raw)
		}
	}
	intent := strings.Trim(text.String(), "\r\n")
	if start < 0 || strings.TrimSpace(intent) == "" {
		return "", false
	}
	return intent, true
}
