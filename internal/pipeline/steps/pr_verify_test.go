package steps

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestPRAppendixVerifyPublicationParity(t *testing.T) {
	_, appendix := ownedFixture(t)
	valid := wrapPRAppendix(appendix)
	legacy := appendix + "\nraw \x1b and literal ^ERROR"
	cases := []struct{ name, body, want string }{
		{"match", valid, "match"},
		{"changed byte", strings.Replace(valid, "Low recorded risk.", "Low recorded Risk.", 1), "mismatch"},
		{"missing end", strings.ReplaceAll(valid, prAppendixEnd, ""), "malformed"},
		{"missing start", strings.ReplaceAll(valid, prAppendixStart, ""), "malformed"},
		{"duplicate", valid + "\n" + valid, "malformed"},
		{"truncated", prAppendixStart, "malformed"},
		{"unowned", "author text", "absent"},
		{"unowned attestation", appendix, "absent"},
		{"legacy subset", strings.ReplaceAll(wrapPRAppendix(legacy), "\x1b", "^["), "match"},
		{"legacy all above cap", strings.ReplaceAll(wrapPRAppendix(appendix+strings.Repeat("\x1b", 13)), "\x1b", "^["), "match"},
		{"open fence", wrapPRAppendix(appendix + "\n```"), "malformed"},
		{"bad attestation", wrapPRAppendix("<!-- no-mistakes-pipeline-attestation:v1 {} -->"), "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := sha256.Sum256([]byte(tc.body))
			got := VerifyPRAppendix(tc.body)
			if got.Verdict != tc.want || got.Reason == "" {
				t.Fatalf("verify = %+v, want %s and a reason", got, tc.want)
			}
			parts, err := parsePROwnedBody(tc.body)
			if (got.Verdict == "match") != (err == nil && parts.managed) {
				t.Fatalf("verification disagrees with publication: %+v, %v", parts, err)
			}
			if err != nil && got.Reason != err.Error() {
				t.Fatalf("reason %q differs from publication %q", got.Reason, err)
			}
			if sha256.Sum256([]byte(tc.body)) != before {
				t.Fatal("verification changed the body")
			}
		})
	}
}
