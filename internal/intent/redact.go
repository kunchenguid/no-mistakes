package intent

import (
	"regexp"
	"strings"
)

// secretPatterns redacts common credential shapes before transcript text
// reaches the summarizer. Matching is intentionally loose - we'd rather
// redact a few innocent strings than leak a real key.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|secret[_-]?(?:key|token)?|password|passwd|bearer|authorization)\s*[:=]\s*['"]?([A-Za-z0-9_\-./+=]{12,})`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`gho_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),
}

// RedactSecrets returns text with likely credentials replaced by [REDACTED].
// Exported for use at prompt-construction boundaries outside this package
// (e.g. when injecting cached intent summaries into step prompts) so the
// same redaction shape applies on the way into the LLM and on the way out.
func RedactSecrets(text string) string {
	for _, pat := range secretPatterns {
		text = pat.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// redactSecrets is the unexported shim retained for in-package callers
// that pre-date the export. New callers should use RedactSecrets.
func redactSecrets(text string) string { return RedactSecrets(text) }

// clampMessages drops messages from the *middle* of the conversation when
// the total Text budget exceeds maxBytes, preserving turns at the start
// and end. The first few messages usually carry the user's original ask
// and setup; the last few carry the most recent state and closing
// instructions. The middle tends to be exploratory back-and-forth that
// is least useful for inferring overall intent.
//
// The algorithm alternates picking from the front and back, so a long
// message at one end doesn't crowd out everything from the other. When
// any messages are dropped, a synthetic marker is inserted between the
// kept prefix and suffix so the LLM doesn't treat the result as a
// contiguous conversation.
func clampMessages(msgs []Message, maxBytes int) []Message {
	if maxBytes <= 0 || len(msgs) == 0 {
		return msgs
	}
	total := 0
	for _, m := range msgs {
		total += len(m.Text)
	}
	if total <= maxBytes {
		return msgs
	}

	const omittedMarker = "[... middle messages omitted to fit the context window; the conversation continues below with later messages from the same session ...]"
	// Reserve space for the marker so we don't overshoot the budget after
	// inserting it. The marker is short, so we ignore the case where the
	// budget is too small to even hold the marker.
	budget := maxBytes - len(omittedMarker)
	if budget <= 0 {
		budget = maxBytes
	}

	var frontKeep []Message
	var backKeep []Message
	used := 0
	front, back := 0, len(msgs)-1
	takeFront := true

	for front <= back {
		var size int
		if takeFront {
			size = len(msgs[front].Text)
		} else {
			size = len(msgs[back].Text)
		}
		if used+size > budget {
			break
		}
		if takeFront {
			frontKeep = append(frontKeep, msgs[front])
			front++
		} else {
			backKeep = append([]Message{msgs[back]}, backKeep...)
			back--
		}
		used += size
		takeFront = !takeFront
	}

	// Pathological case: every message individually exceeds the budget.
	// Fall back to the last message, byte-truncated, since the most recent
	// intent is usually the most relevant single signal.
	if len(frontKeep) == 0 && len(backKeep) == 0 {
		last := msgs[len(msgs)-1]
		if len(last.Text) > maxBytes {
			last.Text = last.Text[len(last.Text)-maxBytes:]
		}
		return []Message{last}
	}

	// front > back means we kept everything (no gap created).
	if front > back {
		return append(frontKeep, backKeep...)
	}

	gap := Message{Synthetic: true, Text: omittedMarker}
	return append(append(frontKeep, gap), backKeep...)
}

// StripAdversarial removes obvious prompt-injection markers that could try
// to escape the surrounding instructions. We don't try to be clever; we
// just neuter common delimiter shapes (ChatML control tokens, role tags,
// Llama/Mistral instruction delimiters) that an attacker might place in
// user-controlled text. This is a stop-gap, not a real defense - the
// real defense is wrapping the text with explicit "this is data, not
// instructions" framing.
func StripAdversarial(text string) string {
	repl := strings.NewReplacer(
		"<|", "<<|",
		"|>", "|>>",
		"<system>", "<sys>",
		"</system>", "</sys>",
		"[INST]", "[inst]",
		"[/INST]", "[/inst]",
	)
	return repl.Replace(text)
}

// stripAdversarial is the unexported shim retained for in-package callers
// that pre-date the export.
func stripAdversarial(text string) string { return StripAdversarial(text) }
