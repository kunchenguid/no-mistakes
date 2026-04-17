package gitlab

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

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
			}), nil)

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
	}), nil)

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
				"glab ci get --pipeline-id 77 --output json": {
					stderr: "gitlab unavailable\n",
					code:   1,
				},
			},
			wantErrSub: "glab ci get",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host := New(gitlabTestCmdFactory(tt.responses), nil)

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
	}), nil)

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
	}), nil)

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
		"glab mr list --source-branch " + branch + " --target-branch main --state opened --output json": {
			stdout: fmt.Sprintf(`[{"web_url":%q}]`+"\n", url),
		},
		"glab mr update 42 --title updated --description body --yes": {
			stdout: "updated\n",
		},
	}), nil)

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
		"glab mr list --source-branch feature/refactor --target-branch release/1.0 --state opened --output json": {
			stdout: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42"}]` + "\n",
		},
	}), nil)

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
		"glab mr list --source-branch feature/refactor --target-branch main --state opened --output json": {
			stderr: "gitlab unavailable\n",
			code:   1,
		},
	}), nil)

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
	}), nil)

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
	}), nil)

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
	}), nil)

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
