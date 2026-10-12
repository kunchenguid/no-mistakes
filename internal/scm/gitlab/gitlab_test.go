package gitlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
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
		"glab mr view 42 --output json": {
			stdout: fmt.Sprintf(`{"iid":42,"title":"existing","web_url":%q}`+"\n", url),
		},
		"glab mr update 42 --title updated --description body": {
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

// TestUpdatePRDoesNotPassUnsupportedYesFlag guards against reintroducing
// -y/--yes on `glab mr update`: unlike `glab mr create`, glab v1.5x's
// `mr update` has no such flag, so passing it fails the whole command with
// "unknown flag: --yes" and every UpdatePR call errors. The fake CmdFactory
// below matches by exact command string, so a regression here would hit the
// "unexpected command" fallback and surface as an UpdatePR error.
func TestUpdatePRDoesNotPassUnsupportedYesFlag(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 7 --output json": {
			stdout: `{"iid":7,"title":"updated"}` + "\n",
		},
		"glab mr update 7 --title updated --description body": {
			stdout: "updated\n",
		},
	}), nil, "", "")

	pr := &scm.PR{Number: "7"}
	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "updated", Body: "body"})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() returned unexpected PR: %+v", updated)
	}
}

func TestSetPRBaseBranchUsesTargetBranchFlag(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr update 7 --target-branch epic/feature": {
			stdout: "updated\n",
		},
	}), nil, "", "")

	if err := host.SetPRBaseBranch(context.Background(), &scm.PR{Number: "7"}, "epic/feature"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch feature/refactor --target-branch release/1.0 --output json": {
			stdout: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

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

func TestFindPRRejectsURLForDifferentProject(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch feature/refactor --target-branch main --output json": {
			stdout: `[{"iid":42,"web_url":"https://gitlab.example.com/group/other/-/merge_requests/42"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want project mismatch error")
	}
	if !strings.Contains(err.Error(), "parse glab mr list") {
		t.Fatalf("FindPR() error = %v, want parse context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestFindPRReturnsJSONError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
	}{
		{name: "no JSON", output: "not-json\n"},
		{name: "malformed JSON", output: "[{\n"},
		{name: "null", output: "null\n"},
		{name: "missing identity", output: "notice\n[{}]\n"},
		{
			name:   "later missing identity",
			output: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42"},{}]` + "\n",
		},
		{
			name:   "invalid URL",
			output: `[{"web_url":"not-a-merge-request-url"}]` + "\n",
		},
		{
			name:   "IID URL mismatch",
			output: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/43"}]` + "\n",
		},
		{
			name:   "negative identity",
			output: `[{"iid":-1,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/-1"}]` + "\n",
		},
		{
			name:   "bare identity",
			output: `[{"iid":42,"web_url":"42"}]` + "\n",
		},
		{
			name:   "query suffix",
			output: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42?view=files"}]` + "\n",
		},
		{
			name:   "fragment suffix",
			output: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42#discussion"}]` + "\n",
		},
		{
			name:   "encoded identity",
			output: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/%34%32"}]` + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				"glab mr list --source-branch feature/refactor --target-branch main --output json": {
					stdout: tc.output,
				},
			}), nil, "", "")

			pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
			if err == nil {
				t.Fatal("FindPR() error = nil, want JSON error")
			}
			if !strings.Contains(err.Error(), "parse glab mr list") {
				t.Fatalf("FindPR() error = %v, want parse context", err)
			}
			if pr != nil {
				t.Fatalf("FindPR() PR = %+v, want nil", pr)
			}
		})
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

func TestFetchFailedCheckTargetLogsAggregatesEverySelectedJob(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {stdout: `{"head_pipeline":{"id":77}}` + "\n"},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `{"jobs":[{"id":55,"name":"build","status":"failed"},{"id":56,"name":"lint","status":"failed"}]}` + "\n",
			stderr: "glab diagnostic notice\n",
		},
		"glab ci trace 55": {stdout: "build failed\n"},
		"glab ci trace 56": {stdout: "lint failed\n"},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "", "", []scm.CheckTarget{{Name: "build", ProviderID: "gitlab-job:55"}, {Name: "lint", ProviderID: "gitlab-job:56"}})
	if err != nil {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v", err)
	}
	if len(logs) != 2 || logs[0].Output != "build failed" || logs[1].Output != "lint failed" {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, want target-separated logs", logs)
	}
}

func TestFetchFailedCheckTargetLogsReturnsPartialLogsWithRetrievalError(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json":                                {stdout: `{"head_pipeline":{"id":77}}` + "\n"},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {stdout: `{"jobs":[{"id":55,"name":"build","status":"failed"},{"id":56,"name":"lint","status":"failed"}]}` + "\n"},

		// Keep this blank line: gofmt versions disagree on whether these short
		// keys join the alignment group above, so an explicit group break is the
		// only layout every toolchain formats identically.
		"glab ci trace 55": {stdout: "build failed\n"},
		"glab ci trace 56": {stderr: "expired", code: 1},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "", "", []scm.CheckTarget{{ProviderID: "gitlab-job:55"}, {ProviderID: "gitlab-job:56"}})
	if err != nil || len(logs) != 2 || logs[0].Output != "build failed" || logs[1].Err == nil || !strings.Contains(logs[1].Err.Error(), "job 56") {
		t.Fatalf("FetchFailedCheckTargetLogs() = (%+v, %v), want retained partial logs and job 56 error", logs, err)
	}
}

func TestFetchFailedCheckTargetLogsReportsMissingSelectedJob(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json":                                {stdout: `{"head_pipeline":{"id":77}}` + "\n"},
		"glab ci get --pipeline-id 77 --output json --with-job-details": {stdout: `{"jobs":[{"id":55,"name":"build","status":"failed"}]}` + "\n"},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "", "", []scm.CheckTarget{{ProviderID: "gitlab-job:999"}})
	if err != nil || len(logs) != 1 || logs[0].Err == nil || !strings.Contains(logs[0].Err.Error(), "gitlab-job:999") {
		t.Fatalf("FetchFailedCheckTargetLogs() = (%+v, %v), want explicit missing-target error", logs, err)
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

func TestCreatePRAddsDraftFlagWhenConfigured(t *testing.T) {
	t.Parallel()

	host := NewWithDraft(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr create --source-branch feature/draft --target-branch main --title fix: draft --description body --yes --draft": {
			stdout: "https://gitlab.example.com/group/project/-/merge_requests/9\n",
		},
	}), nil, "", "", true)

	pr, err := host.CreatePR(context.Background(), "feature/draft", "main", scm.PRContent{
		Title: "fix: draft",
		Body:  "body",
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "9" {
		t.Fatalf("CreatePR() PR = %+v, want !9", pr)
	}
}

func TestCreatePROmitsDraftFlagByDefault(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr create --source-branch feature/x --target-branch main --title fix: x --description body --yes": {
			stdout: "https://gitlab.example.com/group/project/-/merge_requests/3\n",
		},
	}), nil, "", "")

	pr, err := host.CreatePR(context.Background(), "feature/x", "main", scm.PRContent{
		Title: "fix: x",
		Body:  "body",
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "3" {
		t.Fatalf("CreatePR() PR = %+v, want !3", pr)
	}
}

func TestCreatePRTitleLimitCountsCharacters(t *testing.T) {
	t.Parallel()
	title := strings.Repeat("😀", 255)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr create --source-branch feature/x --target-branch main --title " + title + " --description body --yes": {},
	}), nil, "", "")
	if _, err := host.CreatePR(context.Background(), "feature/x", "main", scm.PRContent{Title: title, Body: "body"}); err != nil {
		t.Fatalf("255-character title rejected: %v", err)
	}

	host = New(gitlabTestCmdFactory(nil), nil, "", "")
	_, err := host.CreatePR(context.Background(), "feature/x", "main", scm.PRContent{Title: strings.Repeat("😀", 256), Body: "body"})
	if err == nil || !strings.Contains(err.Error(), "255 characters") {
		t.Fatalf("256-character title error = %v", err)
	}
}

func TestUpdatePRTitleLimitIncludesDraftMarker(t *testing.T) {
	t.Parallel()
	allowed := strings.Repeat("x", 248)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 9 --output json": {
			stdout: `{"iid":9,"title":"Draft: old","web_url":"https://gitlab.example.com/group/project/-/merge_requests/9"}` + "\n",
		},
		"glab mr update 9 --title Draft: " + allowed + " --description body": {},
	}), nil, "", "")
	pr := &scm.PR{Number: "9", URL: "https://gitlab.example.com/group/project/-/merge_requests/9"}
	if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: allowed, Body: "body"}); err != nil {
		t.Fatalf("255-character draft title rejected: %v", err)
	}

	host = New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 9 --output json": {
			stdout: `{"iid":9,"title":"Draft: old","web_url":"https://gitlab.example.com/group/project/-/merge_requests/9"}` + "\n",
		},
	}), nil, "", "")
	_, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: strings.Repeat("x", 249), Body: "body"})
	if err == nil || !strings.Contains(err.Error(), "255 characters") {
		t.Fatalf("256-character draft title error = %v", err)
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

func TestFindFailedJobTargetIDsScansConcatenatedPages(t *testing.T) {
	t.Parallel()

	out := []byte(`[{"id":1,"name":"build","status":"success"}]` + "\n" +
		`[{"id":2,"name":"deploy","status":"failed"}]` + "\n")
	got := findFailedJobTargetIDs(out, []scm.CheckTarget{{Name: "deploy", ProviderID: "gitlab-job:2"}})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("findFailedJobTargetIDs() = %v, want [2]", got)
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

func TestGetChecksRejectsUnreadableJobs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
	}{
		{"no JSON", "not JSON"},
		{"empty output", ""},
		{"missing jobs", `{}`},
		{"null response", `null`},
		{"scalar response", `true`},
		{"null before empty page", "null\n[]"},
		{"null before passing page", "null\n" + `[{"name":"build","status":"success"}]`},
		{"boolean before empty page", "true\n[]"},
		{"false before empty page", "false\n[]"},
		{"number before empty page", "42\n[]"},
		{"negative number before empty page", "-1\n[]"},
		{"string before empty page", "\"unavailable\"\n[]"},
		{"array inside string", `"[]"`},
		{"non-JSON before empty page", "unavailable\n[]"},
		{"object jobs", `{"jobs":{}}`},
		{"string jobs", `{"jobs":"unavailable"}`},
		{"null job", `[null]`},
		{"missing job name", `[{"status":"success"}]`},
		{"missing job status", `[{"name":"build"}]`},
		{"wrong job field type", `[{"name":"build","status":true}]`},
		{"wrong second page", `[{"name":"build","status":"success"}]` + "\n{}"},
		{"null second page", `[{"name":"build","status":"success"}]` + "\nnull"},
		{"wrong page after empty page", "[]\n{}"},
	}
	for _, route := range []string{"primary", "REST fallback", "CLI fallback"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					responses := map[string]gitlabTestResponse{
						"glab ci status --mr 123 --output json": {stdout: tt.output},
					}
					projectPath := ""
					if route != "primary" {
						responses["glab ci status --mr 123 --output json"] = gitlabTestResponse{stderr: "unknown flag: --mr\n", code: 1}
						responses["glab mr view 123 --output json"] = gitlabTestResponse{stdout: `{"head_pipeline":{"id":77}}`}
						jobsCommand := "glab ci get --pipeline-id 77 --output json --with-job-details"
						if route == "REST fallback" {
							projectPath = "group/project"
							jobsCommand = "glab api --paginate projects/group%2Fproject/pipelines/77/jobs"
						}
						responses[jobsCommand] = gitlabTestResponse{stdout: tt.output}
					}
					host := New(gitlabTestCmdFactory(responses), nil, "", projectPath)
					checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
					if err == nil {
						t.Fatalf("GetChecks() = (%+v, nil), want a read error for %q", checks, tt.output)
					}
					if !strings.Contains(err.Error(), "decode gitlab jobs") {
						t.Fatalf("GetChecks() error = %v, want a jobs read error for %q", err, tt.output)
					}
				})
			}
		})
	}
}

func TestGetChecksRequiresExplicitPipelineMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
	}{
		{"missing pipeline", `{}`},
		{"missing pipeline ID", `{"head_pipeline":{}}`},
		{"null pipeline ID", `{"head_pipeline":{"id":null}}`},
		{"zero pipeline ID", `{"head_pipeline":{"id":0}}`},
		{"negative pipeline ID", `{"head_pipeline":{"id":-1}}`},
		{"wrong pipeline ID type", `{"head_pipeline":{"id":"77"}}`},
		{"wrong pipeline shape", `{"head_pipeline":[]}`},
		{"null MR", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				"glab ci status --mr 123 --output json": {stderr: "unknown flag: --mr\n", code: 1},
				"glab mr view 123 --output json":        {stdout: tt.output},
				// The old code passed a negative ID on to the jobs reader. Give
				// that call a successful answer, so the test fails unless the ID
				// itself is refused.
				"glab ci get --pipeline-id -1 --output json --with-job-details": {stdout: `[]`},
			}), nil, "", "")
			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
			if err == nil {
				t.Fatalf("GetChecks() = (%+v, nil), want a read error for %q", checks, tt.output)
			}
			// A `null` MR document fails before the field parse, so the
			// invalid-JSON marker counts too. Both exclude the fake's
			// unmapped-command error, which is what this guard is for.
			if !strings.Contains(err.Error(), "head_pipeline") && !strings.Contains(err.Error(), "invalid JSON output") {
				t.Fatalf("GetChecks() error = %v, want a head_pipeline read error for %q", err, tt.output)
			}
			if checks != nil {
				t.Fatalf("GetChecks() checks = %+v, want nil for invalid pipeline metadata", checks)
			}
		})
	}
}

// The subprocess stream contract keeps diagnostics out of successful job JSON
// and retains them on failure. Mixing the streams would reject the build job.
func TestGetChecksSeparatesJobJSONFromStderr(t *testing.T) {
	t.Parallel()

	for _, route := range []string{"primary", "REST fallback", "CLI fallback"} {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fails=%t", route, fails), func(t *testing.T) {
				t.Parallel()
				jobsCommand := "glab ci status --mr 123 --output json"
				projectPath := ""
				responses := map[string]gitlabTestResponse{}
				if route != "primary" {
					responses[jobsCommand] = gitlabTestResponse{stderr: "unknown flag: --mr\n", code: 1}
					responses["glab mr view 123 --output json"] = gitlabTestResponse{stdout: `{"head_pipeline":{"id":77}}`}
					jobsCommand = "glab ci get --pipeline-id 77 --output json --with-job-details"
					if route == "REST fallback" {
						projectPath = "group/project"
						jobsCommand = "glab api --paginate projects/group%2Fproject/pipelines/77/jobs"
					}
				}
				response := gitlabTestResponse{
					stdout: `[{"id":55,"name":"build","status":"success"}]`,
					stderr: "glab diagnostic notice\n",
				}
				if fails {
					response.code = 1
				}
				responses[jobsCommand] = response
				host := New(gitlabTestCmdFactory(responses), nil, "", projectPath)
				checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
				if fails {
					if err == nil || !strings.Contains(err.Error(), "glab diagnostic notice") || len(checks) != 0 {
						t.Fatalf("GetChecks() = (%+v, %v), want command error with stderr and no checks", checks, err)
					}
					return
				}
				if err != nil || len(checks) != 1 || checks[0].Name != "build" || checks[0].ProviderID != "gitlab-job:55" || checks[0].Bucket != scm.CheckBucketPass {
					t.Fatalf("GetChecks() = (%+v, %v), want passing build job despite stderr notice", checks, err)
				}
			})
		}
	}
}

// The commit identities the head-binding tests reason about: the commit a run
// is delivering, one it is not, one the source branch moved to mid-read, and
// the temporary merged commit GitLab builds a merged-results pipeline on.
const (
	deliveredSHA = "aaaa000000000000000000000000000000000001"
	otherSHA     = "bbbb000000000000000000000000000000000002"
	movedSHA     = "cccc000000000000000000000000000000000003"
	mergeRefSHA  = "dddd000000000000000000000000000000000004"
	targetSHA    = "eeee000000000000000000000000000000000005"
)

// gitlabMRAtHead renders a merge request whose source commit is sourceSHA and
// whose head pipeline (77) ran at pipelineSHA for pipelineRef. An empty
// pipelineSHA omits the pipeline entirely.
func gitlabMRAtHead(sourceSHA, pipelineSHA, pipelineRef string) gitlabTestResponse {
	pipeline := "null"
	if pipelineSHA != "" {
		pipeline = fmt.Sprintf(`{"id":77,"sha":%q,"ref":%q}`, pipelineSHA, pipelineRef)
	}
	return gitlabTestResponse{stdout: fmt.Sprintf(`{"iid":123,"sha":%q,"head_pipeline":%s}`+"\n", sourceSHA, pipeline)}
}

// TestGetChecksRejectsAHeadPipelineThatRanAtADifferentCommit is the reported
// case in its simplest shape: the merge request's source commit is the head
// being delivered, but its head pipeline ran at another commit and holds green
// jobs. GitLab only replaces the head-pipeline pointer once a pipeline for the
// newer revision exists, so an old pipeline's green jobs must never be
// reported as the delivered commit's checks.
func TestGetChecksRejectsAHeadPipelineThatRanAtADifferentCommit(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		// The unbound legacy path would return exactly these green jobs; a
		// build that still takes it for a named head fails this test.
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, otherSHA, "refs/heads/feature"),
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want an error for a pipeline that ran at %s, not %s", checks, otherSHA, deliveredSHA)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() returned %+v alongside the error; want no checks from a pipeline that is not the delivered commit's", checks)
	}
	for _, want := range []string{otherSHA, deliveredSHA} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("GetChecks() error = %v, want it to name %s", err, want)
		}
	}
}

func TestGetChecksReadsJobsOfAHeadPipelineThatRanAtTheDeliveredCommit(t *testing.T) {
	t.Parallel()

	// A head pipeline that ran at the delivered commit is the pipeline to read,
	// and the unbound `glab ci status --mr` path is not consulted for it: the
	// failing job it would return must not appear among the checks.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":9,"name":"legacy-status","status":"failed"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, deliveredSHA, "refs/heads/feature"),
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("GetChecks() = %+v, want the head pipeline's single passing build job", checks)
	}
}

func TestGetChecksRejectsAMergeRequestThatIsNotAtTheDeliveredCommit(t *testing.T) {
	t.Parallel()

	// The merge request's own source revision is authoritative: when it is not
	// the commit being delivered, its pipeline cannot speak for that commit,
	// even if the pipeline matches the merge request's revision.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(otherSHA, otherSHA, "refs/heads/feature"),
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want a head-change error", checks)
	}
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("GetChecks() error = %v, want it to wrap %v", err, scm.ErrHeadChanged)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() returned %+v alongside the error", checks)
	}
}

func TestGetChecksRejectsAMoveOfTheSourceCommitDuringCheckDiscovery(t *testing.T) {
	t.Parallel()

	// The merge request was at the delivered commit when the read started and
	// moved while the jobs were being listed - the same window the GitHub
	// adapter rejects, because the observation no longer describes the commit
	// that will be certified.
	host := New(gitlabSequenceCmdFactory(t, map[string][]gitlabTestResponse{
		"glab mr view 123 --output json": {
			gitlabMRAtHead(deliveredSHA, deliveredSHA, "refs/heads/feature"),
			gitlabMRAtHead(movedSHA, deliveredSHA, "refs/heads/feature"),
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			{stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n"},
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want a head-change error", checks)
	}
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("GetChecks() error = %v, want it to wrap %v", err, scm.ErrHeadChanged)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() returned %+v alongside the error", checks)
	}
	if !strings.Contains(err.Error(), movedSHA) {
		t.Fatalf("GetChecks() error = %v, want it to name the revision the merge request moved to", err)
	}
}

func TestGetChecksAcceptsAMergedResultsPipelineThatIncludesTheDeliveredCommit(t *testing.T) {
	t.Parallel()

	// A merged-results pipeline runs on a temporary commit that merges the
	// source revision into the target side, so its SHA can never equal the
	// source commit. GitLab records the source commit as a parent of that
	// commit; that provenance - not the SHA difference - is what binds the
	// pipeline to the delivered commit.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":9,"name":"legacy-status","status":"failed"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, mergeRefSHA, "refs/merge-requests/123/merge"),
		"glab api projects/group%2Fproject/repository/commits/" + mergeRefSHA: {
			stdout: fmt.Sprintf(`{"id":%q,"parent_ids":[%q,%q]}`, mergeRefSHA, targetSHA, deliveredSHA) + "\n",
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("GetChecks() = %+v, want the merged-results pipeline's passing build job", checks)
	}
}

func TestGetChecksRejectsAMergedResultsPipelineThatDoesNotIncludeTheDeliveredCommit(t *testing.T) {
	t.Parallel()

	// The merge-results pipeline predates the latest source commit: its
	// temporary commit carries an older source revision, so a green run there
	// says nothing about the delivered commit.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, mergeRefSHA, "refs/merge-requests/123/merge"),
		"glab api projects/group%2Fproject/repository/commits/" + mergeRefSHA: {
			stdout: fmt.Sprintf(`{"id":%q,"parent_ids":[%q,%q]}`, mergeRefSHA, targetSHA, otherSHA) + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want an error for a merged-results pipeline that does not include %s", checks, deliveredSHA)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() returned %+v alongside the error", checks)
	}
}

func TestGetChecksFailsClosedWhenMergedResultsProvenanceCannotBeRead(t *testing.T) {
	t.Parallel()

	// Without a project path the merge-result commit cannot be read, so its
	// provenance cannot be proven. Failing closed keeps an unprovable pipeline
	// from being graded; the jobs read below must never be reached.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, mergeRefSHA, "refs/merge-requests/123/merge"),
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want a provenance error", checks)
	}
	if !strings.Contains(err.Error(), "project path") {
		t.Fatalf("GetChecks() error = %v, want it to name the missing project path", err)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() returned %+v alongside the error", checks)
	}
}

func TestGetChecksFailsClosedWhenTheHeadPipelineReportsNoCommit(t *testing.T) {
	t.Parallel()

	// An omitted commit is not an absent pipeline: the binding cannot be shown,
	// so the read fails closed instead of grading jobs whose commit is unknown.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": {
			stdout: fmt.Sprintf(`{"iid":123,"sha":%q,"head_pipeline":{"id":77}}`+"\n", deliveredSHA),
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	if _, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA}); err == nil {
		t.Fatal("GetChecks() error = nil, want a fail-closed error for a pipeline with no commit")
	}
}

func TestGetChecksFailsClosedWhenTheMergeRequestReportsNoSourceCommit(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": {
			stdout: `{"iid":123,"head_pipeline":{"id":77,"sha":"` + deliveredSHA + `"}}` + "\n",
		},
	}), nil, "", "")

	if _, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA}); err == nil {
		t.Fatal("GetChecks() error = nil, want a fail-closed error for an unnamed source commit")
	}
}

func TestGetChecksUsesTheDiffHeadCommitWhenTheMergeRequestReportsNoSHA(t *testing.T) {
	t.Parallel()

	// diff_refs.head_sha populates asynchronously after a push and is the
	// documented head of the merge request's latest diff version; a merge
	// request that has not reported its own sha yet is still bound through it.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {
			stdout: fmt.Sprintf(`{"iid":123,"sha":"","diff_refs":{"head_sha":%q},"head_pipeline":{"id":77,"sha":%q,"ref":"refs/heads/feature"}}`+"\n", deliveredSHA, deliveredSHA),
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("GetChecks() = %+v, want the bound pipeline's build job", checks)
	}
}

func TestGetChecksReturnsNoChecksWhenNoPipelineIsRegisteredForTheHead(t *testing.T) {
	t.Parallel()

	// Nothing has registered for this commit yet: an empty observation keeps
	// the monitor waiting, where the legacy read would have graded a pipeline
	// that belongs to a different revision.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, "", ""),
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() = %+v, want no checks before a pipeline registers for the head", checks)
	}
}

func TestFetchFailedCheckTargetLogsRejectsAPipelineThatRanAtADifferentCommit(t *testing.T) {
	t.Parallel()

	// The selected check failed in a different commit's pipeline. Tracing a
	// same-named job from the pipeline that is currently the merge request's
	// head would attach another commit's log to this selection, so the fetch
	// must fail closed instead.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, otherSHA, "refs/heads/feature"),
		"glab ci get --pipeline-id 77 --output json --with-job-details": {
			stdout: `{"jobs":[{"id":55,"name":"lint","status":"failed"}]}` + "\n",
		},
		"glab ci trace 55": {stdout: "lint failed\n"},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "", deliveredSHA, []scm.CheckTarget{{Name: "lint"}})
	if err == nil {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, err = nil; want a binding error", logs)
	}
	for _, log := range logs {
		if log.Output != "" {
			t.Fatalf("FetchFailedCheckTargetLogs() returned the log %q from another commit's pipeline", log.Output)
		}
	}
}

func TestFetchFailedCheckTargetLogsBindsToTheDeliveredCommit(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, deliveredSHA, "refs/heads/feature"),
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			stdout: `[{"id":55,"name":"lint","status":"failed"}]` + "\n",
		},
		"glab ci trace 55": {stdout: "lint failed\n"},
	}), nil, "gitlab.example.com", "group/project")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "", deliveredSHA, []scm.CheckTarget{{ProviderID: "gitlab-job:55"}})
	if err != nil {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Output != "lint failed" {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, want the head pipeline's lint trace", logs)
	}
}

func TestMergeResultRefMatchesOnlyThisMergeRequestsTemporaryRefs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		ref    string
		number string
		want   bool
	}{
		{"refs/merge-requests/12/merge", "12", true},
		{"refs/merge-requests/12/train", "12", true},
		{"refs/merge-requests/12/merge", " 12 ", true},
		{"refs/merge-requests/13/merge", "12", false},
		// A detached merge request pipeline runs at the source revision itself,
		// so it is never the merged-results exception to the SHA match.
		{"refs/merge-requests/12/head", "12", false},
		{"refs/heads/feature", "12", false},
		{" refs/merge-requests/12/merge ", "12", true},
		{"refs/merge-requests/12/merge", "", false},
	} {
		if got := mergeResultRef(tc.ref, tc.number); got != tc.want {
			t.Errorf("mergeResultRef(%q, %q) = %v, want %v", tc.ref, tc.number, got, tc.want)
		}
	}
}

func TestGetChecksAcceptsExplicitEmptyResults(t *testing.T) {
	t.Parallel()

	outputs := []string{
		`[]`,
		`{"jobs":[]}`,
		"[]\n{\"jobs\":[]}",
		// What glab 1.114 prints for a pipeline with no jobs: `ci get` and
		// `ci status --output json` marshal a nil job slice as null.
		`{"id":77,"status":"failed","jobs":null}`,
		`{"pipeline":{"id":77,"status":"failed"},"jobs":null}`,
	}
	for _, route := range []string{"primary", "REST fallback", "CLI fallback"} {
		for _, output := range outputs {
			t.Run(route+"/"+output, func(t *testing.T) {
				t.Parallel()
				responses := map[string]gitlabTestResponse{
					"glab ci status --mr 123 --output json": {stdout: output},
				}
				projectPath := ""
				if route != "primary" {
					responses["glab ci status --mr 123 --output json"] = gitlabTestResponse{stderr: "unknown flag: --mr\n", code: 1}
					responses["glab mr view 123 --output json"] = gitlabTestResponse{stdout: `{"head_pipeline":{"id":77}}`}
					jobsCommand := "glab ci get --pipeline-id 77 --output json --with-job-details"
					if route == "REST fallback" {
						projectPath = "group/project"
						jobsCommand = "glab api --paginate projects/group%2Fproject/pipelines/77/jobs"
					}
					responses[jobsCommand] = gitlabTestResponse{stdout: output}
				}
				host := New(gitlabTestCmdFactory(responses), nil, "", projectPath)
				checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
				if err != nil || len(checks) != 0 {
					t.Fatalf("GetChecks() = (%+v, %v), want an explicitly empty check list", checks, err)
				}
			})
		}
	}
	t.Run("absent pipeline", func(t *testing.T) {
		t.Parallel()
		host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
			"glab ci status --mr 123 --output json": {stderr: "unknown flag: --mr\n", code: 1},
			"glab mr view 123 --output json":        {stdout: `{"head_pipeline":null}`},
		}), nil, "", "")
		checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
		if err != nil || len(checks) != 0 {
			t.Fatalf("GetChecks() = (%+v, %v), want an explicitly absent pipeline", checks, err)
		}
	})
}

func TestGetChecksRejectsAMergedResultsPipelineWhoseCommitIsNotACommitID(t *testing.T) {
	t.Parallel()

	// The merge-result commit is interpolated into a repository API path, so a
	// value that is not a commit id is refused before any request is built.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 123 --output json": {
			stdout: `[{"id":1,"name":"build","status":"success"}]` + "\n",
		},
		"glab mr view 123 --output json": gitlabMRAtHead(deliveredSHA, "not-a-commit", "refs/merge-requests/123/merge"),
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want a refusal for a non-commit pipeline SHA", checks)
	}
	if !strings.Contains(err.Error(), "not a commit id") {
		t.Fatalf("GetChecks() error = %v, want it to name the unusable commit id", err)
	}
}

// TestGetChecksRejectsAHeadPipelineStrandedByAHeadMoveWithoutAPipeline mirrors a
// live reproduction: the branch head moved to a commit for which the project's
// CI rules created no pipeline at all, so GitLab left the merge request's
// head_pipeline on the pipeline the previous commit ran at. A run delivering
// the new head must not read that pipeline's verdict or its failed job's trace.
func TestGetChecksRejectsAHeadPipelineStrandedByAHeadMoveWithoutAPipeline(t *testing.T) {
	t.Parallel()

	// The delivered commit has no pipeline; the head pipeline belongs to the
	// commit before it, where one job passed and one failed.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab ci status --mr 1 --output json": {
			stdout: `[{"id":646,"name":"green-on-head","status":"success"},{"id":647,"name":"fails-on-purpose","status":"failed"}]` + "\n",
		},
		"glab mr view 1 --output json": {
			stdout: fmt.Sprintf(`{"iid":1,"sha":%q,"head_pipeline":{"id":174,"sha":%q,"ref":"refs/merge-requests/1/head"}}`+"\n", deliveredSHA, otherSHA),
		},
		"glab api --paginate projects/group%2Fproject/pipelines/174/jobs": {
			stdout: `[{"id":646,"name":"green-on-head","status":"success"},{"id":647,"name":"fails-on-purpose","status":"failed"}]` + "\n",
		},
		"glab ci trace 647": {stdout: "this job fails on purpose for CI_COMMIT_SHA=" + otherSHA + "\n"},
	}), nil, "gitlab.example.com", "group/project")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "1", HeadSHA: deliveredSHA})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, err = nil; want a refusal for a pipeline stranded on %s", checks, otherSHA)
	}
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("GetChecks() error = %v, want it to wrap %v so a caller can wait rather than treat it as a provider failure", err, scm.ErrHeadChanged)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() returned %+v alongside the error", checks)
	}

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "1"}, "", deliveredSHA, []scm.CheckTarget{{Name: "fails-on-purpose"}})
	if err == nil {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, err = nil; want a refusal for another commit's pipeline", logs)
	}
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v, want it to wrap %v", err, scm.ErrHeadChanged)
	}
	for _, log := range logs {
		if strings.Contains(log.Output, otherSHA) {
			t.Fatalf("FetchFailedCheckTargetLogs() returned the trace of %s for the delivered commit %s", otherSHA, deliveredSHA)
		}
	}
}

// TestFetchFailedCheckTargetLogsRejectsAHeadMoveDuringLogRetrieval covers the
// second read: the traces are fetched for a pipeline proven to belong to the
// delivered commit, and a source revision that moved while they were being
// fetched must invalidate them rather than leave the caller with another
// commit's logs.
func TestFetchFailedCheckTargetLogsRejectsAHeadMoveDuringLogRetrieval(t *testing.T) {
	t.Parallel()

	host := New(gitlabSequenceCmdFactory(t, map[string][]gitlabTestResponse{
		"glab mr view 1 --output json": {
			gitlabMRAtHead(deliveredSHA, deliveredSHA, "refs/heads/feature"),
			gitlabMRAtHead(movedSHA, deliveredSHA, "refs/heads/feature"),
		},
		"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
			{stdout: `[{"id":647,"name":"fails-on-purpose","status":"failed"}]` + "\n"},
		},
		"glab ci trace 647": {{stdout: "this job fails on purpose\n"}},
	}), nil, "gitlab.example.com", "group/project")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "1"}, "", deliveredSHA, []scm.CheckTarget{{Name: "fails-on-purpose"}})
	if err == nil {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, err = nil; want a head-move error", logs)
	}
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v, want it to wrap %v", err, scm.ErrHeadChanged)
	}
	if len(logs) != 0 {
		t.Fatalf("FetchFailedCheckTargetLogs() returned %+v alongside the error", logs)
	}
	if !strings.Contains(err.Error(), movedSHA) {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v, want it to name the revision the merge request moved to", err)
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
		return gitlabTestHelperCmd(ctx, key, response)
	}
}

// gitlabSequenceCmdFactory serves a queue of responses per command line: a
// command with several queued responses gets them in order, and the last one
// repeats after the queue is exhausted. Tests that must observe two different
// reads of the same command within one call use it.
func gitlabSequenceCmdFactory(t *testing.T, responses map[string][]gitlabTestResponse) CmdFactory {
	t.Helper()
	var mu sync.Mutex
	served := map[string]int{}
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		mu.Lock()
		queue := responses[key]
		var response gitlabTestResponse
		if len(queue) == 0 {
			response = gitlabTestResponse{stderr: "unexpected command: " + key, code: 1}
		} else {
			index := served[key]
			if index >= len(queue) {
				index = len(queue) - 1
			}
			response = queue[index]
			served[key] = index + 1
		}
		mu.Unlock()
		return gitlabTestHelperCmd(ctx, key, response)
	}
}

// gitlabTestHelperCmd re-executes the test binary as its own fake glab, so the
// adapter runs a real process with the faked stdout/stderr/exit code.
func gitlabTestHelperCmd(ctx context.Context, key string, response gitlabTestResponse) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitlabHelperProcess", "--", key)
	cmd.Env = append(os.Environ(),
		"GITLAB_TEST_HELPER=1",
		"GITLAB_TEST_STDOUT="+response.stdout,
		"GITLAB_TEST_STDERR="+response.stderr,
		fmt.Sprintf("GITLAB_TEST_EXIT_CODE=%d", response.code),
	)
	return cmd
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

func TestUpdatePRPreservesDraftTitle(t *testing.T) {
	t.Parallel()

	// GitLab encodes draft state in the title, so an update carrying the plain
	// title would silently mark the MR ready for review.
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 9 --output json": {
			stdout: `{"iid":9,"title":"Draft: fix: x","web_url":"https://gitlab.example.com/group/project/-/merge_requests/9"}` + "\n",
		},
		"glab mr update 9 --title Draft: fix: x --description body": {},
	}), nil, "", "")

	pr := &scm.PR{Number: "9", URL: "https://gitlab.example.com/group/project/-/merge_requests/9"}
	if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "fix: x", Body: "body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
}

func TestUpdatePRDoesNotAddDraftToReadyMR(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 9 --output json": {
			stdout: `{"iid":9,"title":"fix: x","web_url":"https://gitlab.example.com/group/project/-/merge_requests/9"}` + "\n",
		},
		"glab mr update 9 --title fix: x --description body": {},
	}), nil, "", "")

	pr := &scm.PR{Number: "9", URL: "https://gitlab.example.com/group/project/-/merge_requests/9"}
	if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "fix: x", Body: "body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
}

func TestUpdatePRRejectsMissingLiveTitle(t *testing.T) {
	t.Parallel()

	for _, response := range []string{
		`{"iid":9}` + "\n",
		`{"iid":9,"title":null}` + "\n",
		`{"iid":9,"title":"  "}` + "\n",
	} {
		host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
			"glab mr view 9 --output json": {
				stdout: response,
			},
			"glab mr update 9 --title fix: x --description body --yes": {},
		}), nil, "", "")

		pr := &scm.PR{Number: "9"}
		_, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "fix: x", Body: "body"})
		if err == nil || !strings.Contains(err.Error(), "missing merge request title") {
			t.Fatalf("UpdatePR() error = %v, want missing title error", err)
		}
	}
}

func TestIsDraftTitle(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		title string
		want  bool
	}{
		{"Draft: fix", true},
		{"draft: fix", true},
		{"[Draft] fix", true},
		{"(draft) fix", true},
		{"fix: draft handling", false},
		{"", false},
	} {
		if got := isDraftTitle(tt.title); got != tt.want {
			t.Errorf("isDraftTitle(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}

func TestFinalSourceReadWithoutRevisionIsAProviderError(t *testing.T) {
	t.Parallel()
	// An incomplete provider response is not evidence of a moved head. The
	// error must reach the monitor's read-error budget, with no usable results.
	for _, operation := range []string{"checks", "selected logs", "combined logs"} {
		t.Run(operation, func(t *testing.T) {
			host := New(gitlabSequenceCmdFactory(t, map[string][]gitlabTestResponse{
				"glab mr view 123 --output json": {
					gitlabMRAtHead(deliveredSHA, deliveredSHA, "refs/heads/feature"),
					{stdout: `{"iid":123}`},
				},
				"glab api --paginate projects/group%2Fproject/pipelines/77/jobs": {
					{stdout: `[{"id":55,"name":"lint","status":"failed"}]`},
				},
				"glab ci trace 55": {{stdout: "lint failed\n"}},
			}), nil, "gitlab.example.com", "group/project")
			pr := &scm.PR{Number: "123", HeadSHA: deliveredSHA}
			var err error
			switch operation {
			case "checks":
				var checks []scm.Check
				checks, err = host.GetChecks(context.Background(), pr)
				if len(checks) != 0 {
					t.Fatalf("unverified checks returned: %+v", checks)
				}
			case "selected logs":
				var logs []scm.FailedCheckLog
				logs, err = host.FetchFailedCheckTargetLogs(context.Background(), pr, "", deliveredSHA, []scm.CheckTarget{{ProviderID: "gitlab-job:55"}})
				if len(logs) != 0 {
					t.Fatalf("unverified logs returned: %+v", logs)
				}
			case "combined logs":
				var logs string
				logs, err = host.FetchFailedCheckLogs(context.Background(), pr, "", deliveredSHA, []string{"lint"})
				if logs != "" {
					t.Fatalf("unverified logs returned: %s", logs)
				}
			}
			if err == nil || errors.Is(err, scm.ErrHeadChanged) || !strings.Contains(err.Error(), "no source commit") {
				t.Fatalf("error = %v, want missing-source provider error", err)
			}
		})
	}
}

func TestParseGitlabJobsTreatsAllowedFailureAsNonBlocking(t *testing.T) {
	t.Parallel()

	checks, err := parseGitlabJobs([]byte(`[
		{"id":1,"name":"flaky-lint","status":"failed","allow_failure":true},
		{"id":2,"name":"unit","status":"failed","allow_failure":false},
		{"id":3,"name":"e2e","status":"failed"}
	]`))
	if err != nil {
		t.Fatalf("parseGitlabJobs() error = %v", err)
	}
	want := map[string]scm.CheckBucket{
		"flaky-lint": scm.CheckBucketSkip,
		"unit":       scm.CheckBucketFail,
		"e2e":        scm.CheckBucketFail,
	}
	if len(checks) != len(want) {
		t.Fatalf("parseGitlabJobs() = %+v, want %d checks", checks, len(want))
	}
	for _, c := range checks {
		if c.Bucket != want[c.Name] {
			t.Errorf("check %q bucket = %q, want %q", c.Name, c.Bucket, want[c.Name])
		}
	}
}

func TestFindFailedJobTargetIDsIgnoresAllowedFailure(t *testing.T) {
	t.Parallel()

	out := []byte(`[
		{"id":1,"name":"flaky-lint","status":"failed","allow_failure":true},
		{"id":2,"name":"unit","status":"failed"}
	]`)
	if got := findFailedJobTargetIDs(out, nil); len(got) != 1 || got[0] != 2 {
		t.Fatalf("findFailedJobTargetIDs() = %v, want [2]", got)
	}
}
