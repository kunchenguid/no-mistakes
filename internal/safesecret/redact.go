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

// headerValue matches one header value. A quoted value (escapes allowed) is
// taken alone only when its closing quote is followed by a structural
// delimiter (',', '}', ']' or the end of the line), which group 1 captures so
// the replacement can keep it; anything else, such as an escaped quote or a
// quoted prefix (Cookie: "a" sessionid=...), runs to the end of the line.
const headerValue = `(?:(?:"(?:[^"\\\r\n]|\\.)*"|'(?:[^'\\\r\n]|\\.)*')([ \t\r]*(?:[,}\]]|$))|\S[^\r\n]*)`

// headerPatterns redact the whole value of an HTTP header that carries a
// session or auth token, whatever the scheme ("Bearer", "Basic", "SSWS",
// "Bot", "ApiKey", ...). The \b also covers Proxy-Authorization.
//
// The name may be quoted, as in a JSON log or a printed request object
// ({"Authorization": "Bearer ..."}), so the rest of a one-line JSON file
// survives the redaction of one value. An unquoted value runs to the end of
// the line, since cookie values contain spaces, ';' and '='. The last pattern
// covers the HAR / Playwright header entry {"name": "Cookie", "value": "..."};
// a HAR file's separate request/response "cookies" arrays are not covered.
//
// They fail closed, so they are applied only to captured evidence, never to
// intent or prompt text, where a line such as "Authorization: only admins may
// delete projects" is a requirement, not a credential.
var headerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)\bauthorization['"]?\s*[:=][ \t]*` + headerValue),
	regexp.MustCompile(`(?im)\b(?:set-)?cookie['"]?\s*:[ \t]*` + headerValue),
	regexp.MustCompile(`(?im)\bx-auth-token['"]?\s*[:=][ \t]*` + headerValue),
	regexp.MustCompile(`(?i)"name"\s*:\s*"(?:(?:proxy-)?authorization|(?:set-)?cookie|x-auth-token)"\s*,\s*"value"\s*:\s*"(?:[^"\\\r\n]|\\.)*"`),
}

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
		// ${1} keeps the delimiter after a quoted value; it is empty otherwise.
		text = pat.ReplaceAllString(text, "[REDACTED]${1}")
	}
	return Redact(text)
}
