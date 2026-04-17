package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetChecksFallsBackToStateWhenBucketMissing(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":""},{"name":"tests","state":"PENDING","bucket":""}]` + "\n",
		},
	}), nil)

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}
	if checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[0] = %+v, want failing build check", checks[0])
	}
	if checks[1].Name != "tests" || checks[1].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[1] = %+v, want pending tests check", checks[1])
	}
}

type githubTestResponse struct {
	stdout string
	stderr string
	code   int
}

func githubTestCmdFactory(responses map[string]githubTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = githubTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"GITHUB_TEST_HELPER=1",
			"GITHUB_TEST_STDOUT="+response.stdout,
			"GITHUB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestGitHubHelperProcess(t *testing.T) {
	if os.Getenv("GITHUB_TEST_HELPER") != "1" {
		return
	}

	if _, err := fmt.Fprint(os.Stdout, os.Getenv("GITHUB_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("GITHUB_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("GITHUB_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
