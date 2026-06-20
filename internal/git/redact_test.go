package git

import (
	"context"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fine-grained PAT user and token",
			in:   "https://x-access-token:ghp_secret@github.com/o/r.git",
			want: "https://***@github.com/o/r.git",
		},
		{
			name: "bare token userinfo",
			in:   "https://ghp_secret@github.com/o/r.git",
			want: "https://***@github.com/o/r.git",
		},
		{
			name: "username and long token",
			in:   "https://ci-bot:x-access-token:ghp_xABCD1234567890@github.com/owner/repo.git",
			want: "https://***@github.com/owner/repo.git",
		},
		{
			name: "no userinfo is unchanged",
			in:   "https://github.com/o/r.git",
			want: "https://github.com/o/r.git",
		},
		{
			name: "scp-like ssh url unchanged (no scheme)",
			in:   "git@github.com:owner/repo.git",
			want: "git@github.com:owner/repo.git",
		},
		{
			name: "ssh scheme without userinfo unchanged",
			in:   "ssh://git@github.com/owner/repo.git",
			want: "ssh://***@github.com/owner/repo.git",
		},
		{
			name: "local path unchanged",
			in:   "/home/user/repos/no-mistakes.git",
			want: "/home/user/repos/no-mistakes.git",
		},
		{
			name: "relative ref unchanged",
			in:   "refs/heads/main",
			want: "refs/heads/main",
		},
		{
			name: "empty string unchanged",
			in:   "",
			want: "",
		},
		{
			name: "url embedded in text (stderr)",
			in:   "fatal: could not read Username for 'https://x-access-token:ghp_secret@github.com/o/r.git': terminal",
			want: "fatal: could not read Username for 'https://***@github.com/o/r.git': terminal",
		},
		{
			name: "multiple credentialled urls in one string",
			in:   "push https://token:secret@host/a.git then https://u:p@host/b.git",
			want: "push https://***@host/a.git then https://***@host/b.git",
		},
		{
			name: "gitlab deploy token shape",
			in:   "https://oauth2:glpat-xxxxxxxxxxxxxxxxxxxx@gitlab.com/o/r.git",
			want: "https://***@gitlab.com/o/r.git",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "ghp_secret") || strings.Contains(got, "secret") && tc.in != got && !strings.Contains(tc.want, "secret") {
				t.Errorf("RedactURL(%q) leaked credential: %q", tc.in, got)
			}
		})
	}
}

// TestRedactURLNeverLeaksToken feeds a representative credentialled URL and
// asserts the secret never survives redaction regardless of surrounding text.
func TestRedactURLNeverLeaksToken(t *testing.T) {
	t.Parallel()
	const token = "ghp_secret"
	urls := []string{
		"https://x-access-token:" + token + "@github.com/o/r.git",
		"https://" + token + "@github.com/o/r.git",
		"pushing to https://x-access-token:" + token + "@github.com/o/r.git (refs/heads/main)",
		"git push https://x-access-token:" + token + "@github.com/o/r.git HEAD:refs/heads/main: fatal",
	}
	for _, in := range urls {
		if strings.Contains(RedactURL(in), token) {
			t.Errorf("RedactURL leaked token %q in: %s -> %s", token, in, RedactURL(in))
		}
	}
}

// TestRunRedactsURLInError triggers a failing git command that carries a
// credentialled URL as an argument and asserts the token never reaches the
// returned error string. Uses `git remote set-url` against a nonexistent
// remote so it fails fast with no network.
func TestRunRedactsURLInError(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	const token = "ghp_secret_DO_NOT_LEAK"
	credURL := "https://x-access-token:" + token + "@github.com/o/r.git"

	_, err := Run(context.Background(), dir, "remote", "set-url", "no-such-remote", credURL)
	if err == nil {
		t.Fatal("expected error from git remote set-url on nonexistent remote")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("git.Run error leaked credential: %v", err)
	}
	if !strings.Contains(err.Error(), "***@github.com/o/r.git") {
		t.Fatalf("expected redacted URL marker in error, got: %v", err)
	}
}
