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
		"glab ci get --pipeline-id 77 --output json": {
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

func TestFetchFailedCheckLogsParsesMRJSONAfterPreamble(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {
			stdout: "notice\n{\"head_pipeline\":{\"id\":77}}\n",
		},
		"glab ci get --pipeline-id 77 --output json": {
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
