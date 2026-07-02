package github

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRepoSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/test/repo", "test/repo"},
		{"https with .git suffix", "https://github.com/test/repo.git", "test/repo"},
		{"pr url", "https://github.com/test/repo/pull/42", "test/repo"},
		{"ssh scp form", "git@github.com:test/repo.git", "test/repo"},
		{"ssh scp form no suffix", "git@github.com:test/repo", "test/repo"},
		{"ssh url form", "ssh://git@github.com/test/repo.git", "test/repo"},
		{"https with port", "https://github.com:8443/test/repo", "test/repo"},
		{"already a slug", "test/repo", "test/repo"},
		{"trailing slash", "https://github.com/test/repo/", "test/repo"},
		{"empty", "", ""},
		{"host only", "https://github.com/", ""},
		{"owner only", "https://github.com/onlyowner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoSlug(tc.in); got != tc.want {
				t.Fatalf("RepoSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHostPrefixedSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// github.com inputs keep the plain owner/name format.
		{"github.com https", "https://github.com/test/repo", "test/repo"},
		{"github.com https with .git suffix", "https://github.com/test/repo.git", "test/repo"},
		{"github.com pr url", "https://github.com/test/repo/pull/42", "test/repo"},
		{"github.com ssh scp form", "git@github.com:test/repo.git", "test/repo"},
		{"github.com ssh url form", "ssh://git@github.com/test/repo.git", "test/repo"},
		{"github.com https with port", "https://github.com:8443/test/repo", "test/repo"},
		{"github.com mixed case host", "https://GitHub.com/test/repo.git", "test/repo"},
		{"github.com trailing slash", "https://github.com/test/repo/", "test/repo"},

		// GitHub Enterprise Server inputs get the host prefix gh requires.
		{"ghe https", "https://bbgithub.dev.bloomberg.com/org/repo", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe https with .git suffix", "https://bbgithub.dev.bloomberg.com/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe ssh scp form", "git@bbgithub.dev.bloomberg.com:org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe ssh url form", "ssh://git@bbgithub.dev.bloomberg.com/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe pr url", "https://bbgithub.dev.bloomberg.com/org/repo/pull/42", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe https with port", "https://bbgithub.dev.bloomberg.com:8443/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe trailing slash", "https://bbgithub.dev.bloomberg.com/org/repo/", "bbgithub.dev.bloomberg.com/org/repo"},

		// Empty/malformed inputs return "" so the --repo flag is omitted.
		{"empty", "", ""},
		{"host only ghe", "https://bbgithub.dev.bloomberg.com/", ""},
		{"owner only ghe", "https://bbgithub.dev.bloomberg.com/onlyowner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostPrefixedSlug(tc.in); got != tc.want {
				t.Fatalf("HostPrefixedSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGetChecksPassesRepoFlag(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want single build check", checks)
	}
}

func TestGetPRStatePassesRepoFlag(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json state --jq .state": {
			stdout: "MERGED\n",
		},
	}), nil, "", "test/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateMerged)
	}
}

func TestCreatePRStreamsBodyThroughStdin(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- keep generated pull request bodies postable"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr create --head feature/body-cap --base main --repo test/repo --title fix: cap body --body-file -": {
			stdout:    "https://github.com/test/repo/pull/42\n",
			wantStdin: body,
		},
	}), nil, "", "test/repo")

	pr, err := host.CreatePR(context.Background(), "feature/body-cap", "main", scm.PRContent{
		Title: "fix: cap body",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" {
		t.Fatalf("CreatePR() PR = %+v, want #42", pr)
	}
}

func TestUpdatePRStreamsBodyThroughStdin(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- update existing pull request bodies without long argv"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr edit 42 --repo test/repo --title fix: cap body --body-file -": {
			wantStdin: body,
		},
	}), nil, "", "test/repo")

	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{
		Title: "fix: cap body",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() = %+v, want original PR", updated)
	}
}

func TestGetChecksFallsBackToStateWhenBucketMissing(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":""},{"name":"tests","state":"PENDING","bucket":""}]` + "\n",
		},
	}), nil, "", "")

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

func TestGetChecksParsesCompletedAt(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-04-24T04:15:00Z"},{"name":"tests","state":"SUCCESS","bucket":"pass","completedAt":"not-a-time"}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}

	wantCompletedAt := time.Date(2026, 4, 24, 4, 15, 0, 0, time.UTC)
	if !checks[0].CompletedAt.Equal(wantCompletedAt) {
		t.Fatalf("checks[0].CompletedAt = %v, want %v", checks[0].CompletedAt, wantCompletedAt)
	}
	if !checks[1].CompletedAt.IsZero() {
		t.Fatalf("checks[1].CompletedAt = %v, want zero time for invalid timestamp", checks[1].CompletedAt)
	}
}

func TestFetchFailedCheckLogsSelectsMatchingRunForHeadSHA(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run list --branch feature --commit abc123 --status failure --limit 20 --json databaseId,headSha,name,displayTitle,workflowName": {
			stdout: `[{"databaseId":101,"headSha":"abc123","name":"CI","displayTitle":"feature","workflowName":"CI"},{"databaseId":102,"headSha":"abc123","name":"Lint","displayTitle":"lint","workflowName":"Lint"}]` + "\n",
		},
		"gh run view 101 --json jobs": {
			stdout: `{"jobs":[{"name":"unit","conclusion":"failure"}]}` + "\n",
		},
		"gh run view 102 --json jobs": {
			stdout: `{"jobs":[{"name":"lint","conclusion":"failure"}]}` + "\n",
		},
		"gh run view 102 --log-failed": {
			stdout: "lint failed\n",
		},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "123"}, "feature", "abc123", []string{"lint"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "lint failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "lint failed")
	}
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base release/1.0 --state open --json number,url": {
			stdout: `[{"number":42,"url":"https://github.example.com/org/repo/pull/42"}]` + "\n",
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
	if pr.URL != "https://github.example.com/org/repo/pull/42" {
		t.Fatalf("FindPR() URL = %q, want matching base PR", pr.URL)
	}
}

func TestFindPRForkUsesBareHeadAndFiltersOwner(t *testing.T) {
	t.Parallel()

	branch := "feature/refactor"
	host := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head fork-owner:" + branch + " --base main --repo parent/repo --state open --json number,url,headRefName,headRepositoryOwner": {
			stderr: `invalid argument: "--head" does not support "<owner>:<branch>"` + "\n",
			code:   1,
		},
		"gh pr list --head " + branch + " --base main --repo parent/repo --state open --json number,url,headRefName,headRepositoryOwner": {
			stdout: `[` +
				`{"number":40,"url":"https://github.com/parent/repo/pull/40","headRefName":"feature/refactor","headRepositoryOwner":{"login":"other-owner"}},` +
				`{"number":42,"url":"https://github.com/parent/repo/pull/42","headRefName":"feature/refactor","headRepositoryOwner":{"login":"fork-owner"}}` +
				`]` + "\n",
		},
	}), nil, "", "parent/repo", "fork-owner/repo")

	pr, err := host.FindPR(context.Background(), branch, "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want fork PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want 42", pr.Number)
	}
	if pr.URL != "https://github.com/parent/repo/pull/42" {
		t.Fatalf("FindPR() URL = %q, want fork-owned parent PR", pr.URL)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base main --state open --json number,url": {
			stderr: "api unavailable\n",
			code:   1,
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "gh pr list") {
		t.Fatalf("FindPR() error = %v, want gh pr list context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestAvailableScopesAuthToConfiguredHost(t *testing.T) {
	t.Parallel()

	// With a known host, the auth check must be scoped via --hostname so a
	// stale credential on some other configured gh host (e.g. github.com vs
	// a GHE instance) cannot make this repo look unauthenticated. The
	// unscoped form is treated as a failure here to prove the scoped form
	// is the one actually invoked.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status --hostname ghe.example.com": {},
		"gh auth status": {stderr: "github.com: token invalid\n", code: 1},
	}), func() bool { return true }, "ghe.example.com", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil (scoped auth should pass)", err)
	}
}

func TestAvailableFallsBackToUnscopedAuthWhenHostUnknown(t *testing.T) {
	t.Parallel()

	// No host -> behave as before: a bare `gh auth status`.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {},
	}), func() bool { return true }, "", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

type githubTestResponse struct {
	stdout    string
	stderr    string
	wantStdin string
	code      int
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
			"GITHUB_TEST_WANT_STDIN="+response.wantStdin,
			fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestGitHubHelperProcess(t *testing.T) {
	if os.Getenv("GITHUB_TEST_HELPER") != "1" {
		return
	}

	if want := os.Getenv("GITHUB_TEST_WANT_STDIN"); want != "" {
		got, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v", err)
			os.Exit(1)
		}
		if string(got) != want {
			fmt.Fprintf(os.Stderr, "stdin = %q, want %q", string(got), want)
			os.Exit(1)
		}
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
