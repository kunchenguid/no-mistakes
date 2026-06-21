package safeurl

import "testing"

func TestRedactHidesHTTPSCredentials(t *testing.T) {
	got := Redact("https://user:token@example.com/owner/repo.git")
	if got != "https://redacted@example.com/owner/repo.git" {
		t.Fatalf("Redact() = %q, want credentials hidden", got)
	}
}

func TestRedactLeavesCredentialFreeValues(t *testing.T) {
	for _, input := range []string{
		"https://github.com/owner/repo.git",
		"git@github.com:owner/repo.git",
		"/tmp/repo.git",
	} {
		if got := Redact(input); got != input {
			t.Fatalf("Redact(%q) = %q, want unchanged", input, got)
		}
	}
}
