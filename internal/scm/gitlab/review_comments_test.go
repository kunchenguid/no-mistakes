package gitlab

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// TestCapabilitiesDeclareReviewComments pins the opt-in capability the shared
// finding reader gates on: GitLab can read a merge request's unresolved review
// comments through the discussions API.
func TestCapabilitiesDeclareReviewComments(t *testing.T) {
	t.Parallel()

	if !New(nil, nil, "gitlab.example.com", "group/project").Capabilities().ReviewComments {
		t.Fatal("GitLab must declare the review-comments capability")
	}
}

// TestGetReviewCommentsReadsUnresolvedBotDiscussions covers the reader path:
// only a resolvable, unresolved, non-system diff note authored by a registered
// review-bot login becomes a comment. Resolution lives on the discussion's
// notes, and a general MR comment is never resolvable, so neither can become a
// finding.
func TestGetReviewCommentsReadsUnresolvedBotDiscussions(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fproject/merge_requests/123/discussions?per_page=100": {
			stdout: `[
				{"id":"d1","notes":[
					{"id":1126,"body":"Unresolved bot finding","resolvable":true,"resolved":false,"created_at":"2026-09-07T12:00:00.263Z","author":{"username":"greptileai"},"position":{"new_path":"internal/app.go","new_line":42}},
					{"id":1127,"body":"A reply by the same bot","resolvable":true,"resolved":false,"created_at":"2026-09-07T12:05:00Z","author":{"username":"greptileai"},"position":{"new_path":"internal/app.go","new_line":43}}
				]},
				{"id":"d2","notes":[
					{"id":1128,"body":"Already resolved","resolvable":true,"resolved":true,"author":{"username":"greptileai"},"position":{"new_path":"internal/app.go","new_line":44}}
				]},
				{"id":"d3","notes":[
					{"id":1133,"body":"GitHub bot spelling","resolvable":true,"resolved":false,"author":{"username":"greptile-apps[bot]"}},
					{"id":1134,"body":"GitHub app spelling","resolvable":true,"resolved":false,"author":{"username":"greptile-apps"}},
					{"id":1129,"body":"A human review comment","resolvable":true,"resolved":false,"author":{"username":"octocat"},"position":{"new_path":"internal/app.go","new_line":45}}
				]},
				{"id":"d4","notes":[
					{"id":1130,"body":"Bot summary comment","resolvable":false,"resolved":false,"author":{"username":"greptileai"}}
				]},
				{"id":"d5","notes":[
					{"id":1131,"body":"merged branch foo into main","system":true,"resolvable":false,"author":{"username":"greptileai"}}
				]},
				{"id":"d6","notes":[
					{"id":1132,"body":"Finding on a removed line","resolvable":true,"resolved":false,"author":{"username":"GreptileAI"},"position":{"old_path":"internal/old.go","old_line":7}}
				]}
			]` + "\n",
		},
	}), nil, "", "group/project")

	comments, err := host.GetReviewComments(context.Background(), &scm.PR{
		Number: "123",
		URL:    "https://gitlab.example.com/group/project/-/merge_requests/123",
	})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("GetReviewComments() = %+v, want three unresolved bot comments", comments)
	}
	first := comments[0]
	if first.ID != "1126" || first.Author != "greptileai" || first.Path != "internal/app.go" || first.Line != 42 || first.Body != "Unresolved bot finding" {
		t.Fatalf("comments[0] = %+v, want the first unresolved diff note", first)
	}
	if !first.CreatedAt.Equal(time.Date(2026, 9, 7, 12, 0, 0, 263000000, time.UTC)) {
		t.Fatalf("comments[0].CreatedAt = %v, want the note timestamp", first.CreatedAt)
	}
	if first.URL != "https://gitlab.example.com/group/project/-/merge_requests/123#note_1126" {
		t.Fatalf("comments[0].URL = %q, want the MR link anchored on the note", first.URL)
	}
	if comments[1].ID != "1127" {
		t.Fatalf("comments[1] = %+v, want the bot's second note in the same discussion", comments[1])
	}
	// A removed-line note has no new_line; the old side is the only anchor.
	if comments[2].Path != "internal/old.go" || comments[2].Line != 7 {
		t.Fatalf("comments[2] = %+v, want the old-side path and line", comments[2])
	}
	// The GitLab login spelling is not case-sensitive, matching every other
	// login comparison in the registry.
	if comments[2].Author != "GreptileAI" {
		t.Fatalf("comments[2].Author = %q, want the provider's own spelling preserved", comments[2].Author)
	}
}

// TestGetReviewCommentsPaginatesConcatenatedPages pins that a merge request
// with more discussions than one page never silently loses the later ones: an
// unresolved bot comment on a dropped page would never become a finding.
func TestGetReviewCommentsPaginatesConcatenatedPages(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fproject/merge_requests/123/discussions?per_page=100": {
			stdout: `[{"id":"d1","notes":[{"id":1,"body":"page one","resolvable":true,"resolved":false,"author":{"username":"greptileai"},"position":{"new_path":"a.go","new_line":1}}]}]` + "\n" +
				`[{"id":"d2","notes":[{"id":2,"body":"page two","resolvable":true,"resolved":false,"author":{"username":"greptileai"},"position":{"new_path":"b.go","new_line":2}}]}]` + "\n",
		},
	}), nil, "", "group/project")

	comments, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	if len(comments) != 2 || comments[0].Body != "page one" || comments[1].Body != "page two" {
		t.Fatalf("GetReviewComments() = %+v, want both pages", comments)
	}
}

// TestGetReviewCommentsSurfacesCorruptPage keeps a malformed payload from
// reading as "no unresolved comments": a page that cannot be decoded must fail
// the read instead of reporting a clean merge request.
func TestGetReviewCommentsSurfacesCorruptPage(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fproject/merge_requests/123/discussions?per_page=100": {
			stdout: `[{"id":"d1","notes":[{"id":1`,
		},
	}), nil, "", "group/project")

	if _, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "123"}); err == nil {
		t.Fatal("GetReviewComments() error = nil, want a decode error for a corrupt page")
	}
}

// TestGetReviewCommentsScopesTheReadToTheHost pins that the discussions read
// carries the repo's GitLab hostname, so a stale credential or an ambient
// working directory cannot make another instance's merge request answer.
func TestGetReviewCommentsScopesTheReadToTheHost(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fproject/merge_requests/123/discussions?per_page=100 --hostname gitlab.example.com": {
			stdout: `[]`,
		},
	}), nil, "gitlab.example.com", "group/project")

	comments, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("GetReviewComments() = %+v, want none", comments)
	}
}

// TestGetReviewCommentsDerivesProjectFromTheMRURL covers a host built without a
// project path: the recorded merge request URL is validated and supplies the
// project, so a reattached run still reads the right discussions.
func TestGetReviewCommentsDerivesProjectFromTheMRURL(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fsubgroup%2Fproject/merge_requests/77/discussions?per_page=100": {
			stdout: `[{"id":"d1","notes":[{"id":5,"body":"nested project finding","resolvable":true,"resolved":false,"author":{"username":"greptileai"},"position":{"new_path":"a.go","new_line":3}}]}]`,
		},
	}), nil, "", "")

	comments, err := host.GetReviewComments(context.Background(), &scm.PR{
		URL: "https://gitlab.example.com/group/subgroup/project/-/merge_requests/77",
	})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	if len(comments) != 1 || comments[0].Body != "nested project finding" {
		t.Fatalf("GetReviewComments() = %+v, want the nested project's finding", comments)
	}
}

// TestGetReviewCommentsWithoutCoordinatesIsUnsupported keeps a host that cannot
// address a merge request from parking a run under ci.review_bot_comments:
// always. It answers "cannot supply comments" - the same answer a provider
// without the capability gives - rather than a failed read.
func TestGetReviewCommentsWithoutCoordinatesIsUnsupported(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(nil), nil, "gitlab.example.com", "")
	_, err := host.GetReviewComments(context.Background(), &scm.PR{})
	if !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("GetReviewComments() error = %v, want scm.ErrUnsupported", err)
	}
}

// TestGetReviewCommentsSurfacesTheCLIError pins that a rejected glab invocation
// fails the read rather than reading as an empty comment list.
func TestGetReviewCommentsSurfacesTheCLIError(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fproject/merge_requests/123/discussions?per_page=100": {
			stderr: "403 Forbidden\n",
			code:   1,
		},
	}), nil, "", "group/project")

	_, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "123"})
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("GetReviewComments() error = %v, want the CLI failure surfaced", err)
	}
}

func TestProjectPathFromMRURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"merge request", "https://gitlab.example.com/group/project/-/merge_requests/123", "group/project"},
		{"nested subgroup", "https://gitlab.example.com/group/sub/project/-/merge_requests/7", "group/sub/project"},
		{"issue url", "https://gitlab.example.com/group/project/-/issues/123", ""},
		{"repository remote", "https://gitlab.example.com/group/project.git", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectPathFromMRURL(tc.in); got != tc.want {
				t.Fatalf("ProjectPathFromMRURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The discussions API contract requires array pages, including [] for no
// discussions. A successful CLI exit alone must not certify an unread payload.
func TestGetReviewCommentsRequiresDiscussionArrays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		payload   string
		wantError bool
	}{
		{"empty array", "[]", false},
		{"empty pages", "[]\n[]", false},
		{"empty output", "", true},
		{"whitespace", " \r\n", true},
		{"null", "null", true},
		{"non JSON", "request failed", true},
		{"object", "{}", true},
		{"scalar", "42", true},
		{"null page", "[]\nnull", true},
		{"trailing text", "[]\nfailed", true},
		{"prefix text", "failed\n[]", true},
		{"null discussion", "[null]", true},
		{"missing discussion fields", "[{}]", true},
		{"wrong notes shape", `[{"id":"d1","notes":{}}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				"glab api --paginate projects/group%2Fproject/merge_requests/123/discussions?per_page=100": {stdout: tc.payload},
			}), nil, "", "group/project")
			comments, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "123"})
			if (err != nil) != tc.wantError {
				t.Fatalf("GetReviewComments() = %v, %v; want error %v", comments, err, tc.wantError)
			}
			if len(comments) != 0 {
				t.Fatalf("comments = %v, want no comments", comments)
			}
		})
	}
}
