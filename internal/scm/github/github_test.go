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

// UpdatePR shares the same explicit-PR selector boundary as the read methods:
// when the number is absent it must target the canonical PR URL, never an empty
// positional that makes `gh pr edit` resolve the cwd branch (main) from the
// detached bare gate repo and edit the wrong PR.
func TestUpdatePRTargetsKnownPRByURLWhenNumberMissing(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

	prURL := "https://github.com/test/repo/pull/123"
	if _, err := host.UpdatePR(context.Background(), &scm.PR{URL: prURL}, scm.PRContent{
		Title: "fix: cap body",
		Body:  "body",
	}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := recorded[0]
	// argv is: gh pr edit <selector> --repo ...
	if len(got) < 4 || got[1] != "pr" || got[2] != "edit" {
		t.Fatalf("unexpected argv: %v", got)
	}
	if selector := got[3]; selector != prURL {
		t.Fatalf("edit selector = %q, want the known PR URL %q (empty selector makes gh resolve the cwd branch)", selector, prURL)
	}
}

// UpdatePR must fail closed exactly like the read methods: with neither number
// nor URL it refuses to shell out rather than running an argument-less
// `gh pr edit` that would edit the inferred cwd branch's PR.
func TestUpdatePRFailsClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")

	if _, err := host.UpdatePR(context.Background(), &scm.PR{}, scm.PRContent{Title: "t", Body: "b"}); err == nil {
		t.Fatal("UpdatePR() with no PR identity: expected error, got nil")
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

// recordingCmdFactory captures the argv of every gh invocation into recorded
// and replies with a fixed successful stdout, so tests can assert exactly which
// PR selector reached gh instead of matching a whole command string.
func recordingCmdFactory(stdout string, recorded *[][]string) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*recorded = append(*recorded, append([]string{name}, args...))
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", "recorded")
		cmd.Env = append(os.Environ(),
			"GITHUB_TEST_HELPER=1",
			"GITHUB_TEST_STDOUT="+stdout,
			"GITHUB_TEST_EXIT_CODE=0",
		)
		return cmd
	}
}

// failIfInvokedCmdFactory fails the test if gh is invoked at all. It proves that
// a PR-targeting call fails closed (never shelling out) when the PR identity is
// unknown, instead of running an argument-less gh that infers the cwd branch.
func failIfInvokedCmdFactory(t *testing.T) CmdFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("gh should not be invoked without a known PR identity; got: %s %s", name, strings.Join(args, " "))
		return nil
	}
}

// The final CI check lookup must target the exact PR the pipeline already knows.
//
// Trigger: the CI monitor calls GetChecks with a PR the pipeline identifies by
// URL (Number can be empty when the identity was carried as a URL only).
// Masking condition: the daemon runs gh from the detached bare gate repo whose
// HEAD is the default branch (main).
// Symptom: appending an empty pr.Number produced an argument-less
// `gh pr checks --repo <slug>`, so gh fell back to resolving the cwd branch
// (main) and reported "no pull requests found for branch main" even though the
// feature PR's exact-head checks are green — certification could never finish.
//
// The fix passes the canonical PR URL as the explicit selector when the number
// is absent, so the target is always the known PR, never an inferred branch.
func TestGetChecksTargetsKnownPRByURLWhenNumberMissing(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("[]\n", &recorded), nil, "", "test/repo")

	prURL := "https://github.com/test/repo/pull/123"
	if _, err := host.GetChecks(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := recorded[0]
	// argv is: gh pr checks <selector> --repo ...
	if len(got) < 4 || got[1] != "pr" || got[2] != "checks" {
		t.Fatalf("unexpected argv: %v", got)
	}
	selector := got[3]
	if selector != prURL {
		t.Fatalf("check selector = %q, want the known PR URL %q (empty selector makes gh resolve the cwd branch)", selector, prURL)
	}
}

// Compare with the proven explicit-PR invocation: when the number is known it is
// passed verbatim as the selector, exactly as before.
func TestGetChecksTargetsKnownPRByNumber(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("[]\n", &recorded), nil, "", "test/repo")

	if _, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", URL: "https://github.com/test/repo/pull/123"}); err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(recorded) != 1 || len(recorded[0]) < 4 {
		t.Fatalf("unexpected invocations: %v", recorded)
	}
	if selector := recorded[0][3]; selector != "123" {
		t.Fatalf("check selector = %q, want %q", selector, "123")
	}
}

// Missing/invalid PR identity must stop safely rather than checking main or some
// other PR: with neither number nor URL, the PR-targeting reads refuse to shell
// out at all.
func TestPRTargetingReadsFailClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")
	pr := &scm.PR{}

	if _, err := host.GetChecks(context.Background(), pr); err == nil {
		t.Fatal("GetChecks() with no PR identity: expected error, got nil")
	}
	if _, err := host.GetPRState(context.Background(), pr); err == nil {
		t.Fatal("GetPRState() with no PR identity: expected error, got nil")
	}
	if _, err := host.GetMergeableState(context.Background(), pr); err == nil {
		t.Fatal("GetMergeableState() with no PR identity: expected error, got nil")
	}
}

// GetPRState and GetMergeableState share the same selector boundary as
// GetChecks, so a URL-only PR must target the URL there too.
func TestPRStateAndMergeableTargetKnownPRByURL(t *testing.T) {
	t.Parallel()

	prURL := "https://github.com/test/repo/pull/123"

	var stateArgs [][]string
	stateHost := New(recordingCmdFactory("OPEN\n", &stateArgs), nil, "", "test/repo")
	if _, err := stateHost.GetPRState(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if len(stateArgs) != 1 || len(stateArgs[0]) < 4 || stateArgs[0][3] != prURL {
		t.Fatalf("GetPRState selector = %v, want %q at argv[3]", stateArgs, prURL)
	}

	var mergeArgs [][]string
	mergeHost := New(recordingCmdFactory("MERGEABLE\n", &mergeArgs), nil, "", "test/repo")
	if _, err := mergeHost.GetMergeableState(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("GetMergeableState() error = %v", err)
	}
	if len(mergeArgs) != 1 || len(mergeArgs[0]) < 4 || mergeArgs[0][3] != prURL {
		t.Fatalf("GetMergeableState selector = %v, want %q at argv[3]", mergeArgs, prURL)
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
