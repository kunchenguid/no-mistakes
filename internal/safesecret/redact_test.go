package safesecret

import (
	"strings"
	"testing"
)

// fakeGitHubPAT and fakeAWSKey are built at runtime so the source tree holds
// no contiguous credential-shaped literal for a public-push scanner to flag.
var (
	fakeGitHubPAT = "ghp_" + "abcdefghijklmnopqrstuvwx12"
	fakeAWSKey    = "AKIA" + "IOSFODNN7EXAMPLE"
)

// TestRedactKeepsMainsCoverage runs the inputs the redactor caught before
// this package existed (internal/intent/redact_test.go on main) through both
// entry points: neither may be weaker than main.
func TestRedactKeepsMainsCoverage(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{"github pat", "use " + fakeGitHubPAT + " to push", fakeGitHubPAT},
		{"openai key", "key sk-abcdefghijklmnop12345678", "sk-abcdefghijklmnop12345678"},
		{"aws key", fakeAWSKey + " inline", fakeAWSKey},
		{"jwt", "token eyJhbGciOi.eyJzdWIiOi.SflKxwRJSM works", "eyJhbGciOi.eyJzdWIiOi.SflKxwRJSM"},
		{"api_key assignment", `api_key = "abcdef1234567890abc"`, "abcdef1234567890abc"},
		{"authorization assignment", "authorization=abcdef1234567890abc", "abcdef1234567890abc"},
		{"password on the next line", "password:\n  abcdef1234567890abc", "abcdef1234567890abc"},
	}
	for _, tt := range tests {
		for name, redact := range map[string]func(string) string{"Redact": Redact, "RedactEvidence": RedactEvidence} {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				if got := redact(tt.in); strings.Contains(got, tt.secret) || !strings.Contains(got, "[REDACTED]") {
					t.Errorf("%s(%q) = %q, want %q replaced by [REDACTED]", name, tt.in, got, tt.secret)
				}
			})
		}
	}
}

func TestRedactEvidence(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		secret string
	}{
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
		{"cookie on the next line", "Cookie:\n  sid=SECRET-next", "SECRET-next"},
		{"authorization on the next line", "Authorization:\n  Bearer SECRET-next", "SECRET-next"},
		{"json authorization", `{"Authorization": "Bearer opaque-token-abc"}`, "opaque-token-abc"},
		{"python dict", `{'authorization': 'Bearer opaque-py', 'cookie': 'sid=xyz'}`, "opaque-py"},
		{"python dict cookie", `{'authorization': 'Bearer opaque-py', 'cookie': 'sid=xyz'}`, "sid=xyz"},
		{"json x-auth-token", `"x-auth-token": "tok-abc"`, "tok-abc"},
		{"escaped quotes in a json value", `{"Authorization": "Bearer \"abc\" xyz-secret", "ok": 1}`, "xyz-secret"},
		{"quoted prefix", `Cookie: "a" sessionid=SECRET-q`, "SECRET-q"},
		{"quoted prefix and comma", `Set-Cookie: "a", sid=SECRET-c`, "SECRET-c"},
		{"har cookie", `{"name":"Cookie","value":"sid=secretval"}`, "secretval"},
		{"har authorization", `{"name": "authorization", "value": "SSWS opaque-har"}`, "opaque-har"},
		{"har value with escapes", `{"name":"Cookie","value":"sid=\"SECRET-e\""}`, "SECRET-e"},
		{"har value before name", `{"value":"sid=opaque-session-42","name":"Cookie"}`, "opaque-session-42"},
		{"har field between name and value", `{"name":"Cookie","comment":"x","value":"sid=opaque-mid-1"}`, "opaque-mid-1"},
		{"har field between value and name", `{"value":"Bearer opaque-mid-2","comment":"{x}","name":"Authorization"}`, "opaque-mid-2"},
		{"pretty-printed har", "{\n  \"name\": \"Authorization\",\n  \"value\": \"Bearer opaque-okta\"\n}", "opaque-okta"},
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

// TestRedactEvidenceHARMatchStaysInsideOneEntry pins that the either-order
// HAR patterns never pair a credential name in one header entry with the
// value of a neighbouring, harmless entry.
func TestRedactEvidenceHARMatchStaysInsideOneEntry(t *testing.T) {
	in := `[{"name":"Accept","value":"*/*"},{"value":"sid=opaque-7","name":"Cookie"},{"name":"Host","value":"example.com"}]`
	want := `[{"name":"Accept","value":"*/*"},{[REDACTED]},{"name":"Host","value":"example.com"}]`
	if got := RedactEvidence(in); got != want {
		t.Errorf("RedactEvidence(%q) = %q, want %q", in, got, want)
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
}
