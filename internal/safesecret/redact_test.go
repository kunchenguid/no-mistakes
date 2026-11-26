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
		{"json authorization", `{"Authorization": "Bearer opaque-token-abc"}`, "opaque-token-abc"},
		{"python dict", `{'authorization': 'Bearer opaque-py', 'cookie': 'sid=xyz'}`, "opaque-py"},
		{"python dict cookie", `{'authorization': 'Bearer opaque-py', 'cookie': 'sid=xyz'}`, "sid=xyz"},
		{"json x-auth-token", `"x-auth-token": "tok-abc"`, "tok-abc"},
		{"har cookie", `{"name":"Cookie","value":"sid=secretval"}`, "secretval"},
		{"har authorization", `{"name": "authorization", "value": "SSWS opaque-har"}`, "opaque-har"},
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

// TestRedactEvidenceDropsEveryPartOfAHeaderValue pins the pattern order: if
// the generic authorization pattern ran first it would rewrite only the name
// and first token, and the later parts of the value would survive.
func TestRedactEvidenceDropsEveryPartOfAHeaderValue(t *testing.T) {
	in := "> Authorization: abcdef1234567890abcdef part-two part-three\n< 200 OK"
	if got, want := RedactEvidence(in), "> [REDACTED]\n< 200 OK"; got != want {
		t.Errorf("RedactEvidence(%q) = %q, want %q", in, got, want)
	}
}

// TestRedactEvidenceStopsAtAQuotedValue pins that a quoted value ends at its
// closing quote, so one header in a one-line JSON file does not wipe the rest.
func TestRedactEvidenceStopsAtAQuotedValue(t *testing.T) {
	in := `{"Authorization": "Bearer opaque-token-abc", "status": 200}`
	if got, want := RedactEvidence(in), `{"[REDACTED], "status": 200}`; got != want {
		t.Errorf("RedactEvidence(%q) = %q, want %q", in, got, want)
	}
}

// TestRedactEvidenceQuotedValueEdges pins that a quoted value is taken alone
// only when it is a whole, delimited string. An escaped quote inside it, an
// escaped quote around the whole header, or a quoted prefix followed by more
// text must not stop the redaction early.
func TestRedactEvidenceQuotedValueEdges(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"escaped quotes inside a json value", `{"Authorization": "Bearer \"abc\" xyz-secret", "ok": 1}`, `{"[REDACTED], "ok": 1}`},
		{"header inside an escaped json string", `"line": "Authorization: \"Bearer abc\" xyz-secret"`, `"line": "[REDACTED]`},
		{"quoted prefix", `Cookie: "a" sessionid=SECRET`, `[REDACTED]`},
		{"single-quoted prefix", `x-auth-token: 'a' rest-SECRET`, `[REDACTED]`},
		{"quoted value at end of line", "Authorization: \"Bearer SECRET\"\r\nHost: x", "[REDACTED]\r\nHost: x"},
		{"quoted value in an array", `["cookie: \"sid=SECRET\"", 'cookie': 'sid=SECRET2']`, `["[REDACTED]`},
		{"har value with escapes", `{"name":"Cookie","value":"sid=\"SECRET\""}`, `{[REDACTED]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactEvidence(tt.in)
			if got != tt.want || strings.Contains(got, "SECRET") || strings.Contains(got, "xyz-secret") {
				t.Errorf("RedactEvidence(%q) = %q, want %q", tt.in, got, tt.want)
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
