package safesecret

import (
	"strings"
	"testing"
)

// fakeGitHubPAT is built at runtime so the source tree holds no contiguous
// ghp_<20+ alnum> literal for a public-push scanner to flag.
var fakeGitHubPAT = "ghp_" + "abcdefghijklmnopqrstuvwx12"

func TestRedactEvidence(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{"github pat", "push with " + fakeGitHubPAT, fakeGitHubPAT},
		{"bearer", "Authorization: Bearer abcdef1234567890abcdef", "abcdef1234567890abcdef"},
		{"short bearer", "> Authorization: Bearer tok-42", "tok-42"},
		{"okta ssws", "Authorization: SSWS 00aBcDeF", "00aBcDeF"},
		{"discord bot", "Authorization: Bot MTk4NjIy", "MTk4NjIy"},
		{"apikey scheme", "authorization: ApiKey k-7f3a", "k-7f3a"},
		{"authorization assignment", "authorization=raw-token-9", "raw-token-9"},
		{"proxy-authorization", "Proxy-Authorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"curl -H argument", `curl -H "Authorization: Bearer opaque-1234" https://x`, "opaque-1234"},
		{"cookie header", "> Cookie: session=s3cr3t-value; theme=dark", "s3cr3t-value"},
		{"set-cookie header", "< Set-Cookie: sid=abc123def456; Path=/; HttpOnly", "abc123def456"},
		{"x-auth-token header", "X-Auth-Token: tok-9f8e7d6c", "tok-9f8e7d6c"},
		{"x-auth-token assignment", "x-auth-token=tok-1a2b3c", "tok-1a2b3c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactEvidence(tt.in)
			if strings.Contains(got, tt.secret) || !strings.Contains(got, "[REDACTED]") {
				t.Errorf("RedactEvidence(%q) = %q, want %q replaced by [REDACTED]", tt.in, got, tt.secret)
			}
		})
	}
}

func TestRedactEvidenceKeepsOrdinaryText(t *testing.T) {
	in := "GET /checkout 200\ncookie jar emptied\nX-Request-Id: 42\n"
	if got := RedactEvidence(in); got != in {
		t.Errorf("RedactEvidence(%q) = %q, want it unchanged", in, got)
	}
}

func TestRedactEvidenceStopsAtTheEndOfTheHeaderLine(t *testing.T) {
	got := RedactEvidence("Cookie: sid=abc\r\nHost: example.com\n")
	if want := "[REDACTED]\r\nHost: example.com\n"; got != want {
		t.Errorf("RedactEvidence = %q, want %q", got, want)
	}
}

// TestRedactLeavesHeaderShapedProse pins that the prompt/intent redactor does
// not take the fail-closed header patterns: an intent line that names a
// header is a requirement, and wiping it would hide acceptance criteria.
func TestRedactLeavesHeaderShapedProse(t *testing.T) {
	in := "Add RBAC. Authorization: only admins may delete projects.\nCookie: must be HttpOnly."
	if got := Redact(in); got != in {
		t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
	}
	if got := Redact("use " + fakeGitHubPAT); got != "use [REDACTED]" {
		t.Errorf("Redact still has to catch token shapes, got %q", got)
	}
}
