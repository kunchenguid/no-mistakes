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
// "Bot", "ApiKey", ...), from the first non-space character after the ':' or
// '=' to the end of that line. The \b also covers Proxy-Authorization, and
// the name may be quoted, as in a JSON log or a printed request object
// ({"Authorization": "Bearer ..."}). As in the generic patterns, the space
// after the separator may span a line break, so a value on the next line
// ("Cookie:\n  sid=...") is redacted too. The last two patterns cover the HAR
// / Playwright header entry {"name": "Cookie", "value": "..."} with its keys
// in either order and other fields between them (harOtherFields never leaves
// the entry's object); a HAR file's separate request/response "cookies"
// arrays are not covered.
//
// They fail closed: in a one-line JSON file the rest of the line goes with the
// value. So they are applied only to captured evidence, never to intent or
// prompt text, where a line such as "Authorization: only admins may delete
// projects" is a requirement, not a credential.
var headerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bauthorization['"]?\s*[:=]\s*\S[^\r\n]*`),
	regexp.MustCompile(`(?i)\b(?:set-)?cookie['"]?\s*:\s*\S[^\r\n]*`),
	regexp.MustCompile(`(?i)\bx-auth-token['"]?\s*[:=]\s*\S[^\r\n]*`),
	regexp.MustCompile(`(?i)` + harName + harOtherFields + harValue),
	regexp.MustCompile(`(?i)` + harValue + harOtherFields + harName),
}

const (
	harName  = `"name"\s*:\s*"(?:(?:proxy-)?authorization|(?:set-)?cookie|x-auth-token)"`
	harValue = `"value"\s*:\s*"(?:[^"\\]|\\.)*"`
	// harOtherFields is anything between the two keys of one entry: whole
	// JSON strings, or characters that are not a brace or a quote.
	harOtherFields = `(?:[^{}"]|"(?:[^"\\]|\\.)*")*?`
)

// Redact returns text with likely credentials replaced by [REDACTED].
func Redact(text string) string {
	for _, pat := range patterns {
		text = pat.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// RedactEvidence is Redact plus whole-value redaction of credential-bearing
// HTTP headers (Authorization, Cookie, Set-Cookie, X-Auth-Token). Use it for
// agent-captured evidence (logs, traces, saved pages) before it leaves the
// machine. The header patterns run first: the generic authorization pattern
// replaces only the name and first token, which would hide the header name
// and let the rest of a multi-part value through.
func RedactEvidence(text string) string {
	for _, pat := range headerPatterns {
		text = pat.ReplaceAllString(text, "[REDACTED]")
	}
	return Redact(text)
}
