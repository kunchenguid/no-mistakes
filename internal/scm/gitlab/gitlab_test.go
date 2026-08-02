package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestProjectPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://gitlab.example.com/group/project.git", "group/project"},
		{"https without .git", "https://gitlab.example.com/group/project", "group/project"},
		{"https nested subgroups", "https://gitlab.example.com/group/sub/project.git", "group/sub/project"},
		{"https trailing slash", "https://gitlab.example.com/group/project/", "group/project"},
		{"scp ssh", "git@gitlab.example.com:group/project.git", "group/project"},
		{"scp ssh nested", "git@gitlab.example.com:group/sub/project.git", "group/sub/project"},
		// scp-style without a "user@" prefix must still yield the project path;
		// an empty path here would drop the REST job read back to branch-dependent
		// `glab ci get`, which fails in the daemon's detached-HEAD worktree.
		{"scp ssh no user", "gitlab.example.com:group/project.git", "group/project"},
		{"scp ssh no user nested", "gitlab.example.com:group/sub/project.git", "group/sub/project"},
		{"ssh url", "ssh://git@gitlab.example.com:22/group/project.git", "group/project"},
		{"empty", "", ""},
		{"host only", "https://gitlab.example.com", ""},
		// A Windows local filesystem path carries a drive-letter colon, but it is
		// not scp-style host:path syntax: it must not be parsed into a project
		// path or the job read would target a non-existent REST project.
		{"windows drive path backslash", `C:\Users\me\repo`, ""},
		{"windows drive path forward slash", "C:/Users/me/repo", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectPath(tc.in); got != tc.want {
				t.Fatalf("ProjectPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGetMergeableStateTreatsBlockedStatusesAsResolved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   scm.MergeableState
	}{
		{name: "draft", status: "draft_status", want: scm.MergeableOK},
		{name: "discussions unresolved", status: "discussions_not_resolved", want: scm.MergeableOK},
		{name: "blocked", status: "blocked_status", want: scm.MergeableOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				"glab mr view 123 --output json": {
					stdout: fmt.Sprintf(`{"iid":123,"state":"opened","detailed_merge_status":"%s"}`+"\n", tt.status),
				},
			}), nil, "", "")

			got, err := host.GetMergeableState(context.Background(), &scm.PR{Number: "123"})
			if err != nil {
				t.Fatalf("GetMergeableState() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("GetMergeableState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetChecksFallbackParsesMRJSONAfterPreamble(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {
			stdout: "notice\n{\"head_pipeline\":{\"id\":77}}\n",
		},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `[{"name":"test","status":"success"}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.getChecksFallback(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("getChecksFallback() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "test" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing test job", checks[0])
	}
}

func TestGetChecksReturnsFallbackErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		responses  map[string]gitlabTestResponse
		wantErrSub string
	}{
		{
			name: "invalid mr json",
			responses: map[string]gitlabTestResponse{
				"glab ci status --mr 123 --output json": {
					stderr: "unknown flag: --mr\n",
					code:   1,
				},
				"glab mr view 123 --output json": {
					stdout: "notice\nnot json\n",
				},
			},
			wantErrSub: "invalid JSON output",
		},
		{
			name: "pipeline jobs fetch fails",
			responses: map[string]gitlabTestResponse{
				"glab ci status --mr 123 --output json": {
					stderr: "unknown flag: --mr\n",
					code:   1,
				},
				"glab mr view 123 --output json": {
					stdout: `{"head_pipeline":{"id":77}}` + "\n",
				},
				"glab ci get --pipeline-id 77 --output json --with-job-details": {
					stderr: "gitlab unavailable\n",
					code:   1,
				},
			},
			wantErrSub: "glab pipeline jobs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host := New(gitlabTestCmdFactory(tt.responses), nil, "", "")

			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
			if err == nil {
				t.Fatalf("GetChecks() error = nil, want error containing %q", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("GetChecks() error = %v, want substring %q", err, tt.wantErrSub)
			}
			if checks != nil {
				t.Fatalf("GetChecks() checks = %+v, want nil", checks)
			}
		})
	}
}

func TestGetChecksReturnsPrimaryStatusErrorWhenMRFlagIsSupported(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stderr: "gitlab unavailable\n",
			code:   1,
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err == nil {
		t.Fatal("GetChecks() error = nil, want primary ci status error")
	}
	if !strings.Contains(err.Error(), "glab ci status") {
		t.Fatalf("GetChecks() error = %v, want glab ci status context", err)
	}
	if checks != nil {
		t.Fatalf("GetChecks() checks = %+v, want nil", checks)
	}
}

func TestGetChecksFallsBackForVariantUnsupportedMRFlagErrors(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stderr: "error: unrecognized arguments: --mr\n",
			code:   1,
		},
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `[{"name":"test","status":"success"}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "test" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing test job", checks[0])
	}
}

func TestFindPRWithoutIIDKeepsNumberEmptyAndUpdatesByNumberFromURL(t *testing.T) {
	t.Parallel()

	branch := "feature/refactor"
	url := "https://gitlab.example.com/group/project/-/merge_requests/42"
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch " + branch + " --target-branch main --output json": {
			stdout: fmt.Sprintf(`[{"web_url":%q}]`+"\n", url),
		},
		"glab mr update 42 --title updated --description body --yes": {
			stdout: "updated\n",
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), branch, "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "" {
		t.Fatalf("FindPR() number = %q, want empty", pr.Number)
	}
	if pr.URL != url {
		t.Fatalf("FindPR() URL = %q, want %q", pr.URL, url)
	}

	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "updated", Body: "body"})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() returned unexpected PR: %+v", updated)
	}
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch feature/refactor --target-branch release/1.0 --output json": {
			stdout: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42"}]` + "\n",
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "release/1.0")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want %q", pr.Number, "42")
	}
	if pr.URL != "https://gitlab.example.com/group/project/-/merge_requests/42" {
		t.Fatalf("FindPR() URL = %q, want matching base MR", pr.URL)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch feature/refactor --target-branch main --output json": {
			stderr: "gitlab unavailable\n",
			code:   1,
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "glab mr list") {
		t.Fatalf("FindPR() error = %v, want glab mr list context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestVerifyUnpublishedHistoryRejectsPreservedHead(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	p := strings.Repeat("b", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"renamed-feature","sha":"%s"}]`+"\n", a),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/versions?per_page=100": {
			stdout: fmt.Sprintf(`[{"head_commit_sha":"%s"}]`+"\n", p),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {
			stdout: "[]\n",
		},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, p, 0, 0, "https://gitlab.example.com/group/project/-/merge_requests/7"); err == nil {
		t.Fatal("VerifyUnpublishedHistory() error = nil, want preserved-head rejection")
	}
}

func TestVerifyUnpublishedHistoryIgnoresUnrelatedMergeRequests(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	q := strings.Repeat("c", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"renamed-feature","sha":"%s"},{"iid":8,"source_branch":"other","sha":"%s"}]`+"\n", a, q),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/versions?per_page=100":              {stdout: "[]\n"},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {stdout: "[]\n"},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, strings.Repeat("b", 40), 0, 0, "https://gitlab.example.com/group/project/-/merge_requests/7"); err != nil {
		t.Fatalf("VerifyUnpublishedHistory() error = %v, want unrelated MR ignored", err)
	}
}

func TestVerifyUnpublishedHistoryAllowsNoMergeRequestIdentity(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"feature","sha":"%s"}]`+"\n", a),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/versions?per_page=100":              {stdout: "[]\n"},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {stdout: "[]\n"},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, strings.Repeat("b", 40), 0, 0, ""); err != nil {
		t.Fatalf("VerifyUnpublishedHistory() error = %v, want no-MR target accepted", err)
	}
}

func TestVerifyUnpublishedHistoryIgnoresUnrelatedNoMergeRequestIdentity(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	p := strings.Repeat("b", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"other","sha":"%s"}]`+"\n", a),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {stdout: `[{"source_branch":"other"}]` + "\n"},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, p, 0, 0, ""); err != nil {
		t.Fatalf("unrelated merge request history = %v, want ignored", err)
	}
}

func TestVerifyUnpublishedHistoryUsesRenamedTargetLineage(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	p := strings.Repeat("b", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"renamed-feature","sha":"%s"}]`+"\n", a),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/versions?per_page=100": {stdout: `[]` + "\n"},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {
			stdout: fmt.Sprintf(`[{"source_branch":"feature","head_commit_sha":"%s"}]`+"\n", p),
		},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, p, 0, 0, ""); err == nil {
		t.Fatal("renamed target lineage containing preserved head was accepted")
	}
}

func TestVerifyUnpublishedRefHistoryRejectsPreservedBranchHead(t *testing.T) {
	t.Parallel()
	p := strings.Repeat("b", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/events?per_page=100": {
			stdout: fmt.Sprintf(`[{"created_at":"2026-08-01T12:00:00Z","push_data":{"ref":"feature","commit_to":"%s"}}]`+"\n", p),
		},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedRefHistory(context.Background(), "refs/heads/feature", strings.Repeat("a", 40), p, 0, 0); err == nil {
		t.Fatal("preserved branch head passed historical ref publication proof")
	}
}

func TestVerifyUnpublishedRefHistoryRejectsEmptyCoverage(t *testing.T) {
	t.Parallel()
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/events?per_page=100": {stdout: "[]\n"},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedRefHistory(context.Background(), "refs/heads/feature", strings.Repeat("a", 40), strings.Repeat("b", 40), 0, 0); err == nil {
		t.Fatal("empty ref history passed as complete evidence")
	}
}

func TestVerifyUnpublishedRefHistoryRejectsTruncatedOlderCoverage(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix()
	until := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC).Unix()
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/events?per_page=100": {
			stdout: `[{"created_at":"2026-08-01T12:00:00Z","push_data":{"ref":"feature","commit_to":"` + strings.Repeat("a", 40) + `"}}]` + "\n",
		},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedRefHistory(context.Background(), "refs/heads/feature", strings.Repeat("a", 40), strings.Repeat("b", 40), since, until); err == nil {
		t.Fatal("truncated ref history passed as complete evidence")
	}
}

func TestGitLabAuditCoverageRequiresExhaustedPagination(t *testing.T) {
	if _, _, complete, err := gitlabIncludedAuditEvents([]byte("HTTP/2 200 OK\r\nX-Next-Page: 2\r\n\r\n[]\n")); err != nil || complete {
		t.Fatalf("incomplete GitLab audit pages = complete %v, err %v", complete, err)
	}
	events, pages, complete, err := gitlabIncludedAuditEvents([]byte("HTTP/2 200 OK\r\nX-Next-Page:\r\n\r\n[]\n"))
	if err != nil || pages != 1 || !complete || len(events) != 0 {
		t.Fatalf("complete GitLab audit pages = events %d pages %d complete %v err %v", len(events), pages, complete, err)
	}
}

func TestParseGitLabAuditPageRequiresProviderDateAndCursor(t *testing.T) {
	page, err := parseGitLabAuditPage([]byte("HTTP/2 200 OK\r\nDate: Sun, 02 Aug 2026 12:00:00 GMT\r\nX-Next-Page: 2\r\n\r\n[{\"id\":1}]\n"))
	if err != nil || page.serverDate != time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC).Unix() || page.nextPage != 2 || len(page.events) != 1 {
		t.Fatalf("GitLab audit page = %#v, err %v", page, err)
	}
	if _, err := parseGitLabAuditPage([]byte("HTTP/2 200 OK\r\n\r\n[]\n")); err == nil {
		t.Fatal("GitLab audit page without provider date was accepted")
	}
}

func TestGitLabAuditPagesRejectsNonemptyPageWithoutProviderContinuation(t *testing.T) {
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --include projects/group%2Fproject/audit_events?created_before=1&page=1": {
			stdout: "HTTP/2 200 OK\r\nDate: Sun, 02 Aug 2026 12:00:00 GMT\r\n\r\n[{\"id\":1}]\n",
		},
	}), nil, "", "group/project")
	if _, _, _, _, err := host.gitlabAuditPages(context.Background(), "projects/group%2Fproject/audit_events?created_before=1", 1, 0); err == nil {
		t.Fatal("GitLab audit pagination accepted a nonempty page without a provider continuation")
	}
}

func TestGitLabAuditPagesFollowsProviderContinuationToEmptyPage(t *testing.T) {
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --include projects/group%2Fproject/audit_events?created_before=1970-01-01T00%3A00%3A01Z&page=1": {
			stdout: "HTTP/2 200 OK\r\nDate: Sun, 02 Aug 2026 12:00:00 GMT\r\nX-Next-Page: 2\r\n\r\n[{\"id\":1}]\n",
		},
		"glab api --include projects/group%2Fproject/audit_events?created_before=1970-01-01T00%3A00%3A01Z&page=2": {
			stdout: "HTTP/2 200 OK\r\nDate: Sun, 02 Aug 2026 12:00:00 GMT\r\n\r\n[]\n",
		},
	}), nil, "", "group/project")
	events, pages, _, chain, err := host.gitlabAuditPages(context.Background(), "projects/group%2Fproject/audit_events", 1, 1)
	if err != nil || len(events) != 1 || pages != 2 || chain != "1,2" {
		t.Fatalf("GitLab audit pagination = events %d pages %d chain %q err %v", len(events), pages, chain, err)
	}
}

func TestGitLabAuditHeadValidationRequiresCanonicalTargetEvidence(t *testing.T) {
	a := strings.Repeat("a", 40)
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "abbreviated", raw: `{"before":"` + a + `","after":"abc"}`, want: true},
		{name: "uppercase", raw: `{"before":"` + a + `","after":"` + strings.Repeat("A", 40) + `"}`, want: true},
		{name: "missing old head for push", raw: `{"action":"push","after":"` + a + `"}`, want: true},
		{name: "delete new head is not zero", raw: `{"action":"delete","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "canonical push", raw: `{"action":"push","before":"` + a + `","after":"` + a + `"}`, want: false},
		{name: "create before only", raw: `{"action":"create","before":"` + zero + `"}`, want: true},
		{name: "create after only", raw: `{"action":"create","after":"` + a + `"}`, want: true},
		{name: "canonical create", raw: `{"action":"create","before":"` + zero + `","after":"` + a + `"}`, want: false},
		{name: "delete after only", raw: `{"action":"delete","after":"` + zero + `"}`, want: true},
		{name: "canonical delete", raw: `{"action":"delete","before":"` + a + `","after":"` + zero + `"}`, want: false},
		{name: "force push missing side", raw: `{"action":"force_push","before":"` + a + `"}`, want: true},
		{name: "conflicting pairs", raw: `{"action":"push","before":"` + a + `","after":"` + a + `","oldrev":"` + zero + `","newrev":"` + a + `"}`, want: true},
		{name: "rename missing refs", raw: `{"action":"rename","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "canonical rename", raw: `{"action":"rename","old_ref":"feature","new_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: false},
		{name: "rename noncanonical ref", raw: `{"action":"rename","old_ref":"feature..old","new_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename leading dash", raw: `{"action":"rename","old_ref":"-feature","new_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename full ref leading dash", raw: `{"action":"rename","old_ref":"refs/heads/-feature","new_ref":"refs/heads/renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: false},
		{name: "rename lock ref", raw: `{"action":"rename","old_ref":"feature.lock","new_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename leading slash", raw: `{"action":"rename","old_ref":"/feature","new_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename trailing slash", raw: `{"action":"rename","old_ref":"feature/","new_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename conflicting aliases", raw: `{"action":"rename","old_ref":"feature","new_ref":"renamed-feature","from_ref":"other","to_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename partial alias", raw: `{"action":"rename","old_ref":"feature","new_ref":"renamed-feature","from_ref":"other","before":"` + a + `","after":"` + a + `"}`, want: true},
		{name: "rename consistent aliases", raw: `{"action":"rename","old_ref":"feature","new_ref":"renamed-feature","from_ref":"refs/heads/feature","to_ref":"renamed-feature","before":"` + a + `","after":"` + a + `"}`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gitlabAuditValidateHeadValues(json.RawMessage(tc.raw), a)
			if (err != nil) != tc.want {
				t.Fatalf("gitlabAuditValidateHeadValues() error = %v, want error %v", err, tc.want)
			}
		})
	}
}

func TestGitLabAuditTargetClassificationFailsClosedForAmbiguousEvents(t *testing.T) {
	requestSet := map[string]struct{}{"refs/merge-requests/7/head": {}}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"ref":"other","action":"push","after":"`+strings.Repeat("b", 40)+`"}`), requestSet, "feature"); targeted || !ambiguous {
		t.Fatalf("non-current GitLab audit event classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"action":"push","after":"`+strings.Repeat("b", 40)+`"}`), requestSet, "feature"); targeted || !ambiguous {
		t.Fatalf("ambiguous GitLab audit event classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
	if ref, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"ref":"feature","after":"`+strings.Repeat("a", 40)+`"}`), requestSet, "feature"); ref != "feature" || !targeted || ambiguous {
		t.Fatalf("target GitLab audit event = %q targeted=%v ambiguous=%v", ref, targeted, ambiguous)
	}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"merge_request_iid":99,"after":"`+strings.Repeat("a", 40)+`"}`), requestSet, "feature"); targeted || !ambiguous {
		t.Fatalf("disappeared GitLab merge request event classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"merge_request_iid":99,"after":"`+strings.Repeat("a", 40)+`"}`), nil, "feature"); targeted || !ambiguous {
		t.Fatalf("unbound GitLab merge request event without lineage classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"source_branch":"renamed-feature","after":"`+strings.Repeat("a", 40)+`"}`), requestSet, "feature"); targeted || !ambiguous {
		t.Fatalf("renamed GitLab source event classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"ref":"renamed-feature","action":"push","after":"`+strings.Repeat("b", 40)+`"}`), requestSet, "feature"); targeted || !ambiguous {
		t.Fatalf("renamed GitLab ref event classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
	if _, targeted, ambiguous := gitlabAuditTargetRef(json.RawMessage(`{"old_ref":"feature","new_ref":"renamed-feature","action":"rename","before":"`+strings.Repeat("a", 40)+`","after":"`+strings.Repeat("a", 40)+`"}`), requestSet, "feature"); targeted || !ambiguous {
		t.Fatalf("GitLab rename event without current ref classified as targeted=%v ambiguous=%v", targeted, ambiguous)
	}
}

func TestVerifyUnpublishedHistoryInspectsRenamedNoMergeRequestHistory(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"renamed-feature","sha":"%s"}]`+"\n", a),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/versions?per_page=100":              {stdout: "[]\n"},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {stdout: `[{"source_branch":"feature"}]` + "\n"},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, strings.Repeat("b", 40), 0, 0, ""); err != nil {
		t.Fatalf("renamed merge request history = %v, want complete history inspection", err)
	}
}

func TestVerifyUnpublishedHistoryRejectsPreservedHeadInRenamedNoMergeRequestIdentity(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	p := strings.Repeat("b", 40)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
		"glab api --paginate projects/group%2Fproject/merge_requests?state=all&per_page=100": {
			stdout: fmt.Sprintf(`[{"iid":7,"source_branch":"renamed-feature","sha":"%s"}]`+"\n", a),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/versions?per_page=100": {
			stdout: fmt.Sprintf(`[{"head_commit_sha":"%s"}]`+"\n", p),
		},
		"glab api --paginate projects/group%2Fproject/merge_requests/7/resource_state_events?per_page=100": {stdout: `[{"source_branch":"feature"}]` + "\n"},
	}), nil, "", "group/project")
	if err := host.VerifyUnpublishedHistory(context.Background(), "feature", a, p, 0, 0, ""); err == nil {
		t.Fatal("renamed merge request containing preserved head was accepted")
	}
}

func TestGetChecksFallbackRequestsJobDetails(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `{"jobs":[{"name":"lint","status":"failed"}]}` + "\n",
		},
	}), nil, "", "")

	checks, err := host.getChecksFallback(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("getChecksFallback() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "lint" || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[0] = %+v, want failing lint job", checks[0])
	}
}

func TestFetchFailedCheckLogsRequestsJobDetails(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `{"jobs":[{"id":55,"name":"lint","status":"failed"}]}` + "\n",
		},
		"glab ci trace 55": {
			stdout: "lint failed\n",
		},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "123"}, "", "", []string{"lint"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "lint failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "lint failed")
	}
}

func TestFetchFailedCheckLogsParsesMRJSONAfterPreamble(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {
			stdout: "notice\n{\"head_pipeline\":{\"id\":77}}\n",
		},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `[{"id":55,"name":"lint","status":"failed"}]` + "\n",
		},
		"glab ci trace 55": {
			stdout: "lint failed\n",
		},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "123"}, "", "", []string{"lint"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "lint failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "lint failed")
	}
}

func TestGitlabStatusBucketTreatsManualJobsAsSkipped(t *testing.T) {
	t.Parallel()

	if got := gitlabStatusBucket("manual"); got != scm.CheckBucketSkip {
		t.Fatalf("gitlabStatusBucket(manual) = %q, want %q", got, scm.CheckBucketSkip)
	}
}

func TestAvailableScopesAuthToConfiguredHost(t *testing.T) {
	t.Parallel()

	// With a known host, the auth check must be scoped via --hostname so a
	// stale credential on some other configured glab instance cannot make this
	// repo look unauthenticated. The unscoped form is treated as a failure
	// here to prove the scoped form is the one actually invoked.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status --hostname gitlab.example.com": {},
		"glab auth status": {stderr: "gitlab.com: token invalid\n", code: 1},
	}), func() bool { return true }, "gitlab.example.com", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil (scoped auth should pass)", err)
	}
}

func TestAvailableFallsBackToUnscopedAuthWhenHostUnknown(t *testing.T) {
	t.Parallel()

	// No host -> behave as before: a bare `glab auth status`.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab auth status": {},
	}), func() bool { return true }, "", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestFindPRDoesNotPassRemovedStateFlag(t *testing.T) {
	t.Parallel()

	// glab v1.5x removed --state; the open-by-default list must be used. The
	// fixture key omits --state, so a regression that re-adds it would fall
	// through to the "unexpected command" error and fail this test.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch feature/x --target-branch main --output json": {
			stdout: `[{"iid":7,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/7"}]` + "\n",
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "7" {
		t.Fatalf("FindPR() = %+v, want MR !7", pr)
	}
}

func TestGetChecksReadsJobsViaAPIWhenProjectPathKnown(t *testing.T) {
	t.Parallel()

	// With a project path, pipeline jobs are read via `glab api` (REST), which
	// is branch-independent and works in the daemon's detached-HEAD worktree.
	// finished_at must be captured into CompletedAt.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stderr: "unknown flag: --mr\n",
			code:   1,
		},
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":9,"name":"test","status":"success","finished_at":"2026-04-24T04:15:00.000Z"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "test" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing test job", checks[0])
	}
	wantCompletedAt := time.Date(2026, 4, 24, 4, 15, 0, 0, time.UTC)
	if !checks[0].CompletedAt.Equal(wantCompletedAt) {
		t.Fatalf("checks[0].CompletedAt = %v, want %v", checks[0].CompletedAt, wantCompletedAt)
	}
}

func TestGetChecksLeavesCompletedAtZeroWhenFinishedAtMissingOrInvalid(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stderr: "unknown flag: --mr\n",
			code:   1,
		},
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"name":"running","status":"running"},{"name":"bad","status":"success","finished_at":"not-a-time"}]` + "\n",
		},
	}), nil, "", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}
	for _, c := range checks {
		if !c.CompletedAt.IsZero() {
			t.Fatalf("check %q CompletedAt = %v, want zero time", c.Name, c.CompletedAt)
		}
	}
}

func TestGetChecksPaginatesJobsAcrossConcatenatedPages(t *testing.T) {
	t.Parallel()

	// `glab api --paginate` walks every page and writes one JSON array per page,
	// so the output is several arrays concatenated back to back. The parser must
	// read all of them; otherwise a failed job on a later page is silently
	// dropped and the CI verdict misses it. The map key also asserts that the
	// `--paginate` flag is actually present on the jobs call.
	page1 := `[{"id":1,"name":"build","status":"success"}]`
	page2 := `[{"id":2,"name":"deploy","status":"failed"}]`
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stderr: "unknown flag: --mr\n",
			code:   1,
		},
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: page1 + "\n" + page2 + "\n",
		},
	}), nil, "", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2 (jobs from both pages)", len(checks))
	}
	var sawFailedDeploy bool
	for _, c := range checks {
		if c.Name == "deploy" && c.Bucket == scm.CheckBucketFail {
			sawFailedDeploy = true
		}
	}
	if !sawFailedDeploy {
		t.Fatalf("failed job on the second page was dropped: %+v", checks)
	}
}

func TestFindFailedJobIDScansConcatenatedPages(t *testing.T) {
	t.Parallel()

	// The failed job lives on the second concatenated page; findFailedJobID must
	// still locate it across paginated output.
	out := []byte(`[{"id":1,"name":"build","status":"success"}]` + "\n" +
		`[{"id":2,"name":"deploy","status":"failed"}]` + "\n")
	if got := findFailedJobID(out, []string{"deploy"}); got != 2 {
		t.Fatalf("findFailedJobID() = %d, want 2", got)
	}
}

func TestParseGitlabJobsSurfacesCorruptPayload(t *testing.T) {
	t.Parallel()

	// A wholly-malformed payload must surface a decode error rather than be
	// mistaken for an empty (no-jobs) result.
	if _, err := parseGitlabJobs([]byte(`[{"id":1`)); err == nil {
		t.Fatal("parseGitlabJobs() error = nil, want decode error for corrupt payload")
	}

	// When a good page parses before a corrupt one, the parsed jobs are still
	// returned, but the decode error must surface too: a failed job on the
	// dropped page would otherwise be silently hidden and read as green.
	out := []byte(`[{"id":1,"name":"build","status":"success"}]` + "\n" + `[{"id":2`)
	checks, err := parseGitlabJobs(out)
	if err == nil {
		t.Fatal("parseGitlabJobs() error = nil, want decode error from the corrupt later page")
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("parseGitlabJobs() = %+v, want the single parsed build job alongside the error", checks)
	}
}

func TestGetChecksSurfacesErrorWhenPaginatedPageIsCorrupt(t *testing.T) {
	t.Parallel()

	// End-to-end through GetChecks: a corrupt later page of paginated `glab api`
	// output must fail the call rather than return a partial (potentially
	// all-green) slice that hides a failed job on the dropped page.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stderr: "unknown flag: --mr\n",
			code:   1,
		},
		"glab mr view 123 --output json": {
			stdout: `{"head_pipeline":{"id":77}}` + "\n",
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n" + `[{"id":2`,
		},
	}), nil, "", "group/project")

	if _, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"}); err == nil {
		t.Fatal("GetChecks() error = nil, want decode error surfaced from the corrupt page")
	}
}

type gitlabTestResponse struct {
	stdout string
	stderr string
	code   int
}

func gitlabTestCmdFactory(responses map[string]gitlabTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = gitlabTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitlabHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"GITLAB_TEST_HELPER=1",
			"GITLAB_TEST_STDOUT="+response.stdout,
			"GITLAB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("GITLAB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestGitlabHelperProcess(t *testing.T) {
	if os.Getenv("GITLAB_TEST_HELPER") != "1" {
		return
	}

	if _, err := fmt.Fprint(os.Stdout, os.Getenv("GITLAB_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("GITLAB_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("GITLAB_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
