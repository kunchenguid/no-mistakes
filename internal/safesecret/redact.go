// Package safesecret replaces likely credentials in text with [REDACTED].
//
// It is a leaf package so every boundary can share one set of patterns:
// intent.RedactSecrets (prompt and intent text), the evidence branch publish
// (evidence.Publish), and the PR testing section's artifact content and
// embedded evidence text. internal/evidence cannot import internal/intent
// (intent -> db -> integration -> evidence), which is why the patterns live
// here and not in intent.
package safesecret

import "regexp"

// patterns are intentionally loose - we'd rather redact a few innocent strings
// than leak a real key.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|secret[_-]?(?:key|token)?|password|passwd|bearer|authorization)\s*[:=]\s*['"]?([A-Za-z0-9_\-./+=]{12,})`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`gho_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),
	// HTTP headers that carry a session or auth token. Each pattern needs the
	// value to look like a credential (an auth scheme and a token, a cookie
	// name=value pair), so prose such as "Authorization: only admins may
	// delete" in intent text is left alone. The value runs to the end of the
	// line, since cookie values contain spaces, ';' and '='. The \b also
	// covers Proxy-Authorization.
	regexp.MustCompile(`(?i)\bauthorization\s*:\s*(?:bearer|basic|digest|token|negotiate)\s+\S[^\r\n]*`),
	regexp.MustCompile(`(?i)\b(?:set-)?cookie\s*:\s*[^\s=;:]+=[^\r\n]*`),
	regexp.MustCompile(`(?i)\bx-auth-token\s*:\s*\S[^\r\n]*`),
}

// Redact returns text with likely credentials replaced by [REDACTED].
func Redact(text string) string {
	for _, pat := range patterns {
		text = pat.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
