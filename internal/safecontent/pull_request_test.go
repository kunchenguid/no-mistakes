package safecontent

import (
	"errors"
	"strings"
	"testing"
)

func TestScrubPullRequestText_RedactsLocalPublicationIdentity(t *testing.T) {
	t.Setenv("PI_SESSION_ID", "019ff2f3-5f31-744b-90b8-679074ff7686")
	t.Setenv("HERDR_PANE_ID", "pi:47")

	in := strings.Join([]string{
		"home: /Users/testuser/Downloads/result.log",
		"treehouse: /Users/testuser/.treehouse/private-service-a1b2c3/4/private-service/internal/api.go:42",
		"mac temp: /var/folders/zz/synthetic-cache/T/pi-bash-a1b2c3d4e5f60718.log",
		"linux temp: /tmp/tmp.A1b2C3d4E5/output.json",
		"pi: 019ff2f3-5f31-744b-90b8-679074ff7686",
		"HERDR_PANE_ID=pi:47",
		"Run `01ARZ3NDEKTSV4RRFFQ69G5FAV` failed",
	}, "\n")

	got, err := ScrubPullRequestText(in)
	if err != nil {
		t.Fatalf("ScrubPullRequestText() error = %v", err)
	}
	for _, raw := range []string{
		"testuser",
		"private-service-a1b2c3",
		"synthetic-cache",
		"tmp.A1b2C3d4E5",
		"019ff2f3-5f31-744b-90b8-679074ff7686",
		"pi:47",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",
	} {
		if strings.Contains(got, raw) {
			t.Errorf("scrubbed text retained local value %q:\n%s", raw, got)
		}
	}
	for _, safe := range []string{
		"home: ~/Downloads/result.log",
		"treehouse: [worktree]/internal/api.go:42",
		"mac temp: [temp]",
		"linux temp: [temp]",
		"pi: [id]",
		"HERDR_PANE_ID=[id]",
		"Run `[id]` failed",
	} {
		if !strings.Contains(got, safe) {
			t.Errorf("scrubbed text missing %q:\n%s", safe, got)
		}
	}
}

func TestScrubPullRequestText_PreservesSafeMarkdownByteForByte(t *testing.T) {
	t.Parallel()
	const safe = "## Testing\n\n- kept `/tmp/example`\n- run `go:test`\n- set `PI_SESSION_ID=<session-id>`\n- see https://example.com/users/octocat/treehouse\n\n" +
		"<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"0123456789abcdef0123456789abcdef01234567\",\"steps\":[{\"step\":\"test\",\"status\":\"completed\"}]} -->\n"
	got, err := ScrubPullRequestText(safe)
	if err != nil {
		t.Fatalf("ScrubPullRequestText() error = %v", err)
	}
	if got != safe {
		t.Fatalf("safe Markdown changed:\ngot:  %q\nwant: %q", got, safe)
	}
}

func TestScrubPullRequestText_PreservesHTTPURLContainingTreehouseSegment(t *testing.T) {
	t.Parallel()
	const safe = "See https://docs.example.com/config/.treehouse/example/1/example for a generic layout."
	got, err := ScrubPullRequestText(safe)
	if err != nil {
		t.Fatalf("ScrubPullRequestText() error = %v", err)
	}
	if got != safe {
		t.Fatalf("safe URL changed: got %q, want %q", got, safe)
	}
}

func TestScrubPullRequestText_RefusesAmbiguousTreehousePathWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	const raw = "~/.treehouse/private-managed-copy/not-a-slot"
	got, err := ScrubPullRequestText("evidence at " + raw)
	if !errors.Is(err, ErrUnsafePullRequestContent) {
		t.Fatalf("ScrubPullRequestText() error = %v, want privacy refusal", err)
	}
	if got != "" {
		t.Fatalf("refused output = %q, want empty", got)
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-managed-copy") {
		t.Fatalf("privacy error echoed rejected content: %v", err)
	}
}

func TestScrubPullRequestText_NeverGrowsContent(t *testing.T) {
	t.Setenv("PI_SESSION_ID", "019ff2f3-5f31-744b-90b8-679074ff7686")
	t.Setenv("HERDR_PANE_ID", "pi:47")
	for _, in := range []string{
		"/Users/testuser/project/file.go",
		"/Users/testuser/.treehouse/project-a1b2c3/1/project/file.go",
		"/var/folders/zz/synthetic-cache/T/TestThing123456789/001/result.log",
		"/tmp/tmp.A1b2C3d4E5/result.log",
		"PI_SESSION_ID=019ff2f3-5f31-744b-90b8-679074ff7686",
		"HERDR_PANE_ID=pi:47",
		"HERDR_WORKSPACE_ID=ws",
	} {
		got, err := ScrubPullRequestText(in)
		if err != nil {
			t.Fatalf("ScrubPullRequestText(%q) error = %v", in, err)
		}
		if len(got) > len(in) {
			t.Errorf("ScrubPullRequestText(%q) grew from %d to %d bytes: %q", in, len(in), len(got), got)
		}
	}
}
