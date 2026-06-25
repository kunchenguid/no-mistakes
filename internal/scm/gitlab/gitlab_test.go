package gitlab

import (
	"context"
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
