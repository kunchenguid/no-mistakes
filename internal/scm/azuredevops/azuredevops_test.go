package azuredevops

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

const (
	testOrg     = "https://dev.azure.com/myorg"
	testProject = "myproject"
	testRepo    = "myrepo"
)

func newTestHost(responses map[string]azdoTestResponse) *Host {
	return New(azdoTestCmdFactory(responses), func() bool { return true }, testOrg, testProject, testRepo)
}

func TestProviderAndCapabilities(t *testing.T) {
	t.Parallel()

	h := newTestHost(nil)
	if h.Provider() != scm.ProviderAzureDevOps {
		t.Fatalf("Provider() = %q, want %q", h.Provider(), scm.ProviderAzureDevOps)
	}
	caps := h.Capabilities()
	if !caps.MergeableState {
		t.Fatal("Capabilities().MergeableState = false, want true")
	}
	if caps.FailedCheckLogs {
		t.Fatal("Capabilities().FailedCheckLogs = true, want false (not implemented)")
	}
}

func TestAvailableChecksExtensionAndAuth(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az extension show --name azure-devops":                                             {stdout: "{}\n"},
		"az devops project list --query value[0].id --output tsv --organization " + testOrg: {stdout: "abc\n"},
	})
	if err := h.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestAvailableReportsMissingExtension(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az extension show --name azure-devops": {stderr: "not installed\n", code: 1},
	})
	err := h.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "azure-devops extension") {
		t.Fatalf("Available() error = %v, want azure-devops extension error", err)
	}
}

func TestFindPRReturnsBrowsableURL(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --target-branch main --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
			stdout: `[{"pullRequestId":42,"status":"active","repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]` + "\n",
		},
	})

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want 42", pr.Number)
	}
	if pr.URL != "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/42" {
		t.Fatalf("FindPR() URL = %q, want browsable pullrequest URL", pr.URL)
	}
}

func TestFindPRNoMatch(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
			stdout: "[]\n",
		},
	})

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil", pr)
	}
}

func TestFindPRIgnoresStderrChatter(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --target-branch main --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
			stdout: `[{"pullRequestId":42,"status":"active","repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]` + "\n",
			stderr: "Command group 'repos pr' is in preview and under development.\n",
		},
	})

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" {
		t.Fatalf("FindPR() = %+v, want PR 42 (stderr chatter must not corrupt JSON)", pr)
	}
}

func TestFindPRReportsParseError(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --target-branch main --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
			stdout: "not json at all\n",
		},
	})

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err == nil {
		t.Fatalf("FindPR() error = nil, want parse error (must not be silently treated as no-PR)")
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil on parse failure", pr)
	}
}

func TestCreatePRConstructsURL(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr create --source-branch feature --target-branch main --title T --description B --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
			// az returns an _apis/... url in the top-level field; it must NOT be used.
			stdout: `{"pullRequestId":7,"url":"https://dev.azure.com/myorg/_apis/git/repositories/abc/pullRequests/7"}` + "\n",
		},
	})

	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: "B"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "7" {
		t.Fatalf("CreatePR() number = %q, want 7", pr.Number)
	}
	if pr.URL != "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/7" {
		t.Fatalf("CreatePR() URL = %q, want constructed browsable URL", pr.URL)
	}
}

func TestCreatePRTruncatesOverlongDescription(t *testing.T) {
	t.Parallel()

	// A body well over Azure DevOps' 4000-character description cap. Before the
	// clamp, CreatePR passed this verbatim and az rejected it with
	// "Invalid argument value. ... must not be longer than 4000 characters".
	body := strings.Repeat("x", 5000)
	clamped := scm.ClampPRBody(body, scm.MaxPRBodyChars(scm.ProviderAzureDevOps))
	if scm.PRBodyLen(clamped) > 4000 {
		t.Fatalf("clamped description left %d units, want <= 4000", scm.PRBodyLen(clamped))
	}

	key := "az repos pr create --source-branch feature --target-branch main --title T --description " + clamped +
		" --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json"
	h := newTestHost(map[string]azdoTestResponse{
		key: {stdout: `{"pullRequestId":7}` + "\n"},
	})

	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: body})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "7" {
		t.Fatalf("CreatePR() number = %q, want 7", pr.Number)
	}
}

func TestGetPRState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want scm.PRState
	}{
		{"active", scm.PRStateOpen},
		{"completed", scm.PRStateMerged},
		{"abandoned", scm.PRStateClosed},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			h := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json": {
					stdout: fmt.Sprintf(`{"pullRequestId":42,"status":%q}`, tc.raw) + "\n",
				},
			})
			state, err := h.GetPRState(context.Background(), &scm.PR{Number: "42"})
			if err != nil {
				t.Fatalf("GetPRState() error = %v", err)
			}
			if state != tc.want {
				t.Fatalf("GetPRState(%q) = %q, want %q", tc.raw, state, tc.want)
			}
		})
	}
}

func TestGetMergeableState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want scm.MergeableState
	}{
		{"succeeded", scm.MergeableOK},
		{"conflicts", scm.MergeableConflict},
		{"rejectedByPolicy", scm.MergeablePending},
		{"failure", scm.MergeablePending},
		{"queued", scm.MergeablePending},
		{"notSet", scm.MergeablePending},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			h := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json": {
					stdout: fmt.Sprintf(`{"pullRequestId":42,"mergeStatus":%q}`, tc.raw) + "\n",
				},
			})
			got, err := h.GetMergeableState(context.Background(), &scm.PR{Number: "42"})
			if err != nil {
				t.Fatalf("GetMergeableState() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("GetMergeableState(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestGetChecksMapsPolicyEvaluations(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {
			stdout: `[` +
				`{"status":"approved","completedDate":"2026-04-24T04:15:00Z","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"Build validation"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Build"},"settings":{}},"context":{"buildDefinitionName":"ci-build"}},` +
				`{"status":"running","configuration":{"type":{"displayName":"Status"}}},` +
				`{"status":"notApplicable","configuration":{"type":{"displayName":"Required reviewers"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Minimum number of reviewers"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Comment requirements"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Require a merge strategy"}}}` +
				`]` + "\n",
		},
	})

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("len(checks) = %d, want 3 (notApplicable + approval/merge gates omitted): %+v", len(checks), checks)
	}
	if checks[0].Name != "Build validation" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing 'Build validation'", checks[0])
	}
	wantTime := time.Date(2026, 4, 24, 4, 15, 0, 0, time.UTC)
	if !checks[0].CompletedAt.Equal(wantTime) {
		t.Fatalf("checks[0].CompletedAt = %v, want %v", checks[0].CompletedAt, wantTime)
	}
	if checks[1].Name != "ci-build" || checks[1].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[1] = %+v, want failing 'ci-build' from context", checks[1])
	}
	if checks[2].Name != "Status" || checks[2].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[2] = %+v, want pending 'Status'", checks[2])
	}
}

func TestGetChecksExcludesApprovalGatesOnHealthyPR(t *testing.T) {
	t.Parallel()

	// A normal open PR awaiting human review: every approval/merge gate reports a
	// blocking "rejected" status, but none is a CI failure. GetChecks must return
	// no checks so the CI monitor does not launch pointless auto-fix attempts.
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {
			stdout: `[` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Minimum number of reviewers"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Required reviewers"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Comment requirements"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Work item linking"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Require a merge strategy"}}}` +
				`]` + "\n",
		},
	})

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() = %+v, want empty (approval/merge gates are not CI checks)", checks)
	}
}

func TestGetChecksEmpty(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {stdout: "[]\n"},
	})
	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() = %+v, want empty", checks)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
			stderr: "TF401019: not found\n", code: 1,
		},
	})
	_, err := h.FindPR(context.Background(), "feature", "")
	if err == nil || !strings.Contains(err.Error(), "az repos pr list") {
		t.Fatalf("FindPR() error = %v, want az repos pr list context", err)
	}
}

func TestFetchFailedCheckLogsUnsupported(t *testing.T) {
	t.Parallel()

	h := newTestHost(nil)
	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123", []string{"ci-build"})
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() logs = %q, want empty", logs)
	}
	if err != scm.ErrUnsupported {
		t.Fatalf("FetchFailedCheckLogs() error = %v, want ErrUnsupported", err)
	}
}

type azdoTestResponse struct {
	stdout string
	stderr string
	code   int
}

func azdoTestCmdFactory(responses map[string]azdoTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = azdoTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzdoHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"AZDO_TEST_HELPER=1",
			"AZDO_TEST_STDOUT="+response.stdout,
			"AZDO_TEST_STDERR="+response.stderr,
			fmt.Sprintf("AZDO_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestAzdoHelperProcess(t *testing.T) {
	if os.Getenv("AZDO_TEST_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("AZDO_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("AZDO_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("AZDO_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
