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

// redactSecrets returns text with likely credentials replaced by [REDACTED].
func redactSecrets(text string) string {
	for _, pat := range secretPatterns {
		text = pat.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// clampMessages keeps the most recent messages such that the total Text
// budget stays under maxBytes. Older messages are dropped first because
// recent intent is more likely to reflect what the change actually does.
func clampMessages(msgs []Message, maxBytes int) []Message {
	if maxBytes <= 0 {
		return msgs
	}
	total := 0
	for _, m := range msgs {
		total += len(m.Text)
	}
	if total <= maxBytes {
		return msgs
	}
	// Walk from the end backwards, keeping messages until the budget runs out.
	keep := 0
	used := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		used += len(msgs[i].Text)
		if used > maxBytes {
			break
		}
		keep++
	}
	if keep == 0 {
		// At least keep the last message, truncated.
		last := msgs[len(msgs)-1]
		if len(last.Text) > maxBytes {
			last.Text = last.Text[len(last.Text)-maxBytes:]
		}
		return []Message{last}
	}
	return msgs[len(msgs)-keep:]
}

// stripAdversarial removes obvious prompt-injection markers that could try
// to escape the summarizer's instructions. We don't try to be clever; we
// just neuter common delimiter shapes that an attacker would place in a
// user-controlled transcript.
func stripAdversarial(text string) string {
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
