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
}

// headerPatterns redact the whole value of an HTTP header that carries a
// session or auth token, whatever the scheme ("Bearer", "Basic", "SSWS",
// "Bot", "ApiKey", ...). The value runs to the end of the line, since cookie
// values contain spaces, ';' and '='. The \b also covers Proxy-Authorization.
//
// They fail closed, so they are applied only to captured evidence, never to
// intent or prompt text, where a line such as "Authorization: only admins may
// delete projects" is a requirement, not a credential.
var headerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*\S[^\r\n]*`),
	regexp.MustCompile(`(?i)\b(?:set-)?cookie\s*:\s*\S[^\r\n]*`),
	regexp.MustCompile(`(?i)\bx-auth-token\s*[:=]\s*\S[^\r\n]*`),
}

// Redact returns text with likely credentials replaced by [REDACTED].
func Redact(text string) string {
	return replace(text, patterns)
}

// RedactEvidence is Redact plus whole-value redaction of credential-bearing
// HTTP headers (Authorization, Cookie, Set-Cookie, X-Auth-Token). Use it for
// agent-captured evidence (logs, traces, saved pages) before it leaves the
// machine. The header patterns run first: the generic authorization pattern
// replaces only the name and first token, which would hide the header name
// and let the rest of a multi-part value through.
func RedactEvidence(text string) string {
	return Redact(replace(text, headerPatterns))
}

func replace(text string, pats []*regexp.Regexp) string {
	for _, pat := range pats {
		text = pat.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
