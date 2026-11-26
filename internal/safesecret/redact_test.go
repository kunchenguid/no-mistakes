package safesecret

import (
	"strings"
	"testing"
)

// fakeGitHubPAT is built at runtime so the source tree holds no contiguous
// ghp_<20+ alnum> literal for a public-push scanner to flag.
var fakeGitHubPAT = "ghp_" + "abcdefghijklmnopqrstuvwx12"

func TestRedact(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{"github pat", "push with " + fakeGitHubPAT, fakeGitHubPAT},
		{"authorization header", "Authorization: Bearer abcdef1234567890abcdef", "abcdef1234567890abcdef"},
		{"cookie header", "> Cookie: session=s3cr3t-value; theme=dark", "s3cr3t-value"},
		{"set-cookie header", "< Set-Cookie: sid=abc123def456; Path=/; HttpOnly", "abc123def456"},
		{"x-auth-token header", "X-Auth-Token: tok-9f8e7d6c", "tok-9f8e7d6c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if strings.Contains(got, tt.secret) || !strings.Contains(got, "[REDACTED]") {
				t.Errorf("Redact(%q) = %q, want %q replaced by [REDACTED]", tt.in, got, tt.secret)
			}
		})
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	in := "GET /checkout 200\ncookie jar emptied\nX-Request-Id: 42\n"
	if got := Redact(in); got != in {
		t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
	}
}

func TestRedactStopsAtTheEndOfTheHeaderLine(t *testing.T) {
	got := Redact("Cookie: sid=abc\r\nHost: example.com\n")
	if want := "[REDACTED]\r\nHost: example.com\n"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}
