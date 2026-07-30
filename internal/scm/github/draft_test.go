package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCapabilitiesDeclaresDraftSupport(t *testing.T) {
	t.Parallel()

	if !New(nil, nil, "", "test/repo").Capabilities().Draft {
		t.Fatal("GitHub must declare draft PR support")
	}
}

func TestCreatePRPassesDraftFlagOnlyWhenRequested(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		draft     bool
		wantDraft bool
	}{
		{"draft requested", true, true},
		{"draft not requested", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var recorded [][]string
			host := New(recordingCmdFactory("https://github.com/test/repo/pull/7\n", &recorded), nil, "", "test/repo")

			if _, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{
				Title: "feat: add thing",
				Body:  "body",
				Draft: tc.draft,
			}); err != nil {
				t.Fatalf("CreatePR() error = %v", err)
			}
			if len(recorded) != 1 {
				t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
			}
			argv := strings.Join(recorded[0], " ")
			if got := strings.Contains(argv, " --draft"); got != tc.wantDraft {
				t.Fatalf("--draft present = %v, want %v (argv: %s)", got, tc.wantDraft, argv)
			}
		})
	}
}

// MarkPRReady shares the explicit-PR-selector boundary with every other
// PR-targeting gh call: the daemon runs gh from the detached bare gate repo
// whose HEAD is the default branch, so an inferred selector would flip the
// wrong PR (or none) out of draft.
func TestMarkPRReadyTargetsKnownPRByURLWhenNumberMissing(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

	prURL := "https://github.com/test/repo/pull/123"
	if err := host.MarkPRReady(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("MarkPRReady() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := recorded[0]
	// argv is: gh pr ready <selector> --repo ...
	if len(got) < 4 || got[1] != "pr" || got[2] != "ready" {
		t.Fatalf("unexpected argv: %v", got)
	}
	if selector := got[3]; selector != prURL {
		t.Fatalf("ready selector = %q, want the known PR URL %q", selector, prURL)
	}
	if !strings.Contains(strings.Join(got, " "), "--repo test/repo") {
		t.Fatalf("expected --repo scoping, got: %v", got)
	}
}

func TestMarkPRReadyPrefersPRNumber(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

	if err := host.MarkPRReady(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}); err != nil {
		t.Fatalf("MarkPRReady() error = %v", err)
	}
	if len(recorded) != 1 || recorded[0][3] != "42" {
		t.Fatalf("expected `gh pr ready 42`, got %v", recorded)
	}
}

func TestMarkPRReadyFailsClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")

	if err := host.MarkPRReady(context.Background(), &scm.PR{}); err == nil {
		t.Fatal("MarkPRReady() with no PR identity: expected error, got nil")
	}
}

// An adopted PR's draft state must be known without a second gh round trip, so
// FindPR asks for isDraft in the field list it already requests.
func TestFindPRReportsDraftState(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory(`[{"number":42,"url":"https://github.com/test/repo/pull/42","isDraft":true}]`+"\n", &recorded), nil, "", "test/repo")

	pr, err := host.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want the open PR")
	}
	if !pr.IsDraft {
		t.Fatal("FindPR() did not report the PR as a draft")
	}
	argv := strings.Join(recorded[0], " ")
	if !strings.Contains(argv, "isDraft") {
		t.Fatalf("expected isDraft in the --json field list, got: %s", argv)
	}
}

func TestFindPRReportsNonDraftState(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory(`[{"number":42,"url":"https://github.com/test/repo/pull/42","isDraft":false}]`+"\n", &recorded), nil, "", "test/repo")

	pr, err := host.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.IsDraft {
		t.Fatalf("FindPR() = %+v, want a non-draft PR", pr)
	}
}

func TestMarkPRReadySurfacesCLIFailure(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr ready 42 --repo test/repo": {stdout: "not a draft", code: 1},
	}), nil, "", "test/repo")

	err := host.MarkPRReady(context.Background(), &scm.PR{Number: "42"})
	if err == nil {
		t.Fatal("MarkPRReady() expected an error when gh fails")
	}
	if errors.Is(err, scm.ErrUnsupported) {
		t.Fatal("a gh failure must not be reported as unsupported")
	}
}
