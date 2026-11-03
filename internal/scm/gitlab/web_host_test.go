package gitlab

import (
	"context"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetReviewCommentsSeparateWebHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, project, apiHost, webURL string
		configExit                     int
		wantError                      bool
	}{
		{name: "configured web host", project: "group/sub/project", apiHost: "example.com", webURL: "https://example.com/group/sub/project/-/merge_requests/42"},
		{name: "derived project", apiHost: "example.com", webURL: "https://example.com/group/sub/project/-/merge_requests/42"},
		{name: "remote host", project: "group/sub/project", webURL: "https://git.example.com/group/sub/project/-/merge_requests/42"},
		{name: "unrelated host", project: "group/sub/project", apiHost: "example.com", webURL: "https://other.example/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "different project", project: "group/sub/project", apiHost: "example.com", webURL: "https://example.com/group/other/-/merge_requests/42", wantError: true},
		{name: "missing config", project: "group/sub/project", configExit: 1, webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "empty config", project: "group/sub/project", webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "malformed config", project: "group/sub/project", apiHost: "https://example.com", webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				"glab config get api_host --host git.example.com --global": {stdout: tc.apiHost, code: tc.configExit},
				"glab api --paginate projects/group%2Fsub%2Fproject/merge_requests/42/discussions?per_page=100 --hostname git.example.com": {
					stdout: `[{"id":"d1","notes":[{"id":7,"body":"Bot finding","resolvable":true,"author":{"username":"greptileai"}}]}]`,
				},
			}), nil, "git.example.com", tc.project)
			comments, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "42", URL: tc.webURL})
			if tc.wantError {
				if err == nil || len(comments) != 0 {
					t.Fatalf("GetReviewComments() = %+v, %v; want refusal", comments, err)
				}
				return
			}
			if err != nil || len(comments) != 1 {
				t.Fatalf("GetReviewComments() = %+v, %v; want one bot finding", comments, err)
			}
			if comments[0].ID != "7" || comments[0].URL != tc.webURL+"#note_7" {
				t.Fatalf("unexpected comment: %+v", comments[0])
			}
		})
	}
}

func TestFindPRSeparateWebHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, apiHost, webURL string
		configExit            int
		wantError             bool
	}{
		{name: "configured web host", apiHost: "example.com\n", webURL: "https://example.com/group/sub/project/-/merge_requests/42"},
		{name: "case insensitive", apiHost: "EXAMPLE.COM\n", webURL: "https://example.com/group/sub/project/-/merge_requests/42"},
		{name: "API port", apiHost: "example.com:8443\n", webURL: "https://example.com/group/sub/project/-/merge_requests/42"},
		{name: "unrelated host", apiHost: "example.com", webURL: "https://other.example/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "different project", apiHost: "example.com", webURL: "https://example.com/group/other/-/merge_requests/42", wantError: true},
		{name: "missing config", configExit: 1, webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "empty config", webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "URL not host", apiHost: "https://example.com", webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "malformed config", apiHost: "example.com\nother.example", webURL: "https://example.com/group/sub/project/-/merge_requests/42", wantError: true},
		{name: "query", apiHost: "example.com", webURL: "https://example.com/group/sub/project/-/merge_requests/42?x=1", wantError: true},
		{name: "IID mismatch", apiHost: "example.com", webURL: "https://example.com/group/sub/project/-/merge_requests/43", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				"glab mr list --source-branch feature --output json": {stdout: fmt.Sprintf(`[{"iid":42,"web_url":%q}]`, tc.webURL)},
				// Pin both the remote host and global scope: neither an unrelated account
				// nor repository-local config may authorize the returned URL.
				"glab config get api_host --host git.example.com --global": {stdout: tc.apiHost, stderr: "config notice\n", code: tc.configExit},
			}), nil, "git.example.com", "group/sub/project")
			pr, err := host.FindPR(context.Background(), "feature", "")
			if tc.wantError {
				if err == nil || pr != nil {
					t.Fatalf("FindPR() = %+v, %v; want refusal", pr, err)
				}
				return
			}
			if err != nil || pr == nil {
				t.Fatalf("FindPR() = %+v, %v", pr, err)
			}
			if pr.Number != "42" || pr.URL != tc.webURL {
				t.Fatalf("FindPR() = %+v", pr)
			}
		})
	}
}
