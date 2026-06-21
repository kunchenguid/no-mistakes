package safeurl

import (
	"net/url"
	"strings"
)

// Redact hides URL userinfo while leaving non-URL and credential-free values
// unchanged.
func Redact(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User("redacted")
	return parsed.String()
}
