package steps

import (
	"errors"
	"fmt"
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
func publishedPRIntent(body string) (string, error) {
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

// intentSection renders a published Intent section ending in its marker. A
// bare marker line in the text is escaped and a fence the text leaves open is
// closed, so the marker is always the first one a re-raise finds outside a
// fence. Text read back by liveIntentSection already satisfies both, so
// republishing it is byte for byte.
func intentSection(text string) string {
	lines := strings.Split(text, "\n")
	var fence markdownFence
	for i, line := range lines {
		if fence.marker == 0 && strings.TrimRight(line, "\r") == prIntentEndMarker {
			lines[i] = `\` + line
		}
		fence.consume(line)
	}
	text = strings.Join(lines, "\n")
	if fence.marker != 0 {
		text += "\n" + strings.Repeat(string(fence.marker), fence.width)
	}
	return "## Intent\n\n" + text + "\n\n" + prIntentEndMarker
}

// The reasons a re-raise cannot keep a published Intent. The step states the
// reason when this run's intent replaces the section or, with no intent of
// its own, when the section is removed.
var (
	errUnmarkedPRIntent   = errors.New("no intent marker in the published body; republish once to opt in")
	errNearMissPRIntent   = errors.New("no exact ## Intent heading in the published body")
	errUnreadablePRIntent = errors.New("this forge cannot read the published body")
)

// clampKeepingIntentMarker wraps clamp for text that may open with a published
// Intent section: what follows the section is cut first, and when the section
// itself must shrink only its text is cut, so a published Intent always ends
// with prIntentEndMarker. It returns "" when not even a shortened section fits.
func clampKeepingIntentMarker(units func(string) int, clamp func(string, int) string) func(string, int) string {
	return func(text string, limit int) string {
		if units(text) <= limit {
			return text
		}
		intent, err := liveIntentSection(text)
		section := intentSection(intent)
		if err != nil || intent == "" || !strings.HasPrefix(text, section) {
			return clamp(text, limit)
		}
		if tail := strings.TrimLeft(text[len(section):], "\n"); tail != "" {
			if budget := limit - units(section+"\n\n"); budget > 0 {
				if clamped := clamp(tail, budget); strings.TrimSpace(clamped) != "" && units(clamped) <= budget {
					return section + "\n\n" + clamped
				}
			}
		}
		if units(section) <= limit {
			return section
		}
		for budget := limit - units(intentSection("")); budget > 0; {
			shortened := intentSection(clamp(intent, budget))
			over := units(shortened) - limit
			if over <= 0 {
				return shortened
			}
			budget -= over
		}
		return ""
	}
}

// liveIntentSection returns the text of the first "## Intent" section in an
// existing PR body, exactly as published up to its prIntentEndMarker, without
// the blank lines that separate it from its neighbours. Only the exact heading
// line the generator writes opens the section, and a fenced line never opens
// or ends it. It returns "" when the body has no such section or it is empty,
// errUnmarkedPRIntent when the section has no marker, and errNearMissPRIntent
// when no exact heading exists but a hand-edited one does: the marker is the
// only boundary trusted, so a heading inside the Intent, even one spelled like
// a generated section, stays author text.
func liveIntentSection(body string) (string, error) {
	lines := strings.SplitAfter(body, "\n")
	var fence markdownFence
	start := -1
	nearMiss := false
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if fence.marker == 0 {
			if start < 0 {
				if line == "## Intent" {
					start = i + 1
					continue
				}
				trimmed := strings.TrimSpace(line)
				nearMiss = nearMiss || (strings.HasPrefix(trimmed, "#") && strings.EqualFold(strings.Trim(trimmed, "# \t:.!?"), "intent"))
			} else if line == prIntentEndMarker {
				intent := strings.Trim(strings.Join(lines[start:i], ""), "\r\n")
				if strings.TrimSpace(intent) == "" {
					return "", nil
				}
				return intent, nil
			}
		}
		fence.consume(line)
	}
	if start >= 0 {
		return "", errUnmarkedPRIntent
	}
	if nearMiss {
		return "", errNearMissPRIntent
	}
	return "", nil
}
