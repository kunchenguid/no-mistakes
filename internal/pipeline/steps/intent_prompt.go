package steps

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// userIntentPromptSection returns a prompt fragment describing the inferred
// user intent for the change being processed. The fragment is empty when
// no intent is available, so steps can append it unconditionally.
//
// The wording deliberately frames the intent as a hint, not ground truth -
// it was derived from a local transcript and may be partial, stale, or
// belong to a different change than the one being reviewed.
func userIntentPromptSection(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	intent := strings.TrimSpace(sctx.UserIntent)
	if intent == "" {
		return ""
	}
	return "\n\nUser intent (inferred from the author's recent agent session, may be partial or wrong; treat as a hint, not ground truth):\n" +
		sanitizePromptMultilineText(intent) + "\n"
}
