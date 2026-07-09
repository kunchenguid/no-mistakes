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
	if !caps.RequiresChecks {
		t.Fatal("Capabilities().RequiresChecks = false, want true (empty ADO checks must never be read as ready)")
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

// TestGetChecksExcludesHumanGatesByIdentity is the classification guard. A PR
// can carry human review / sign-off / attestation gates that are NOT the simple
// own-type approval gates above: Code Review Compliance Policy is posted through
// the generic "Status" policy type (so a naive Build/Status allow-list lets it
// through and its blocking "rejected" pollutes the content signal), while
// Ownership Enforcer and Proof Of Presence are their OWN policy types (confirmed
// live on ADO PR 16330529). All three must be classified as human gates by
// stable identity and excluded from the content-CI checks, even when rejected.
func TestGetChecksExcludesHumanGatesByIdentity(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {
			stdout: `[` +
				// Human sign-off posted through the Status policy type - the leak a
				// Build/Status allow-list alone does not catch. Matched on stable
				// (genre, name), not the localizable displayName.
				`{"status":"rejected","configuration":{"type":{"displayName":"Status"},"settings":{"displayName":"Code Review Compliance","statusGenre":"microsoft-policy-service","statusName":"CodeReviewCompliancePolicy"}}},` +
				// Own-type human gates.
				`{"status":"rejected","configuration":{"type":{"displayName":"Ownership Enforcer"}}},` +
				`{"status":"rejected","configuration":{"type":{"displayName":"Proof Of Presence"}}}` +
				`]` + "\n",
		},
	})

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("GetChecks() = %+v, want empty (human review/sign-off/attestation gates are not content CI checks)", checks)
	}
}

// TestGetChecksGatesOnContentStatusChecks is the positive counterpart: content-
// influenced automation posted through the Status policy type (Component
// Governance security/license, code coverage) must still be gated on. Only the
// specifically-identified human status genres are excluded; every other Status
// gate stays a real content check so B2's fail-safe posture is preserved and the
// human-gate exclusion never manufactures a vacuous green.
func TestGetChecksGatesOnContentStatusChecks(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {
			stdout: `[` +
				`{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"displayName":"Component Governance","statusGenre":"security","statusName":"ComponentGovernance"}}},` +
				`{"status":"running","configuration":{"type":{"displayName":"Status"},"settings":{"displayName":"Code coverage","statusGenre":"codecoverage","statusName":"coverage"}}},` +
				// And a genuine human sign-off in the same list must still be excluded.
				`{"status":"rejected","configuration":{"type":{"displayName":"Status"},"settings":{"displayName":"Code Review Compliance","statusGenre":"microsoft-policy-service","statusName":"CodeReviewCompliancePolicy"}}}` +
				`]` + "\n",
		},
	})

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2 (Component Governance + coverage kept, human sign-off excluded): %+v", len(checks), checks)
	}
	if checks[0].Name != "Component Governance" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing 'Component Governance'", checks[0])
	}
	if checks[1].Name != "Code coverage" || checks[1].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[1] = %+v, want pending 'Code coverage'", checks[1])
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

// TestGetChecksSurfacesUnknownStatusAsPending guards the B2 fix: a build
// validation eval reporting a status this connector does not recognize must be
// surfaced as pending, never dropped. Dropping it would let an unexpected
// status silently vanish into an empty check list and manufacture a vacuous
// green - the exact false-green this fix closes.
func TestGetChecksSurfacesUnknownStatusAsPending(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {
			stdout: `[` +
				`{"status":"someNewStatus","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"Build validation"}}}` +
				`]` + "\n",
		},
	})
	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1 (unknown status must not be dropped): %+v", len(checks), checks)
	}
	if checks[0].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[0].Bucket = %q, want pending (unknown status surfaced, not passed)", checks[0].Bucket)
	}
}

// TestGetChecksNotApplicableStaysIgnoredWithOtherPassingGate is the B2
// regression guard: a path-scoped notApplicable policy is still correctly
// dropped (it is genuinely not content-influenced), and a PR whose other real
// build gate passed still yields a non-empty, all-passing check list so it can
// go green. B2 must not over-block on notApplicable.
func TestGetChecksNotApplicableStaysIgnoredWithOtherPassingGate(t *testing.T) {
	t.Parallel()

	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {
			stdout: `[` +
				`{"status":"notApplicable","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"Path-scoped build"}}},` +
				`{"status":"approved","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"Build validation"}}}` +
				`]` + "\n",
		},
	})
	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1 (notApplicable dropped, passing gate kept): %+v", len(checks), checks)
	}
	if checks[0].Name != "Build validation" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing 'Build validation'", checks[0])
	}
}

func TestAzStatusBucket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status string
		want   scm.CheckBucket
	}{
		{name: "approved passes", status: "approved", want: scm.CheckBucketPass},
		{name: "rejected fails", status: "rejected", want: scm.CheckBucketFail},
		{name: "broken fails", status: "broken", want: scm.CheckBucketFail},
		{name: "queued pending", status: "queued", want: scm.CheckBucketPending},
		{name: "running pending", status: "running", want: scm.CheckBucketPending},
		{name: "notApplicable dropped", status: "notApplicable", want: ""},
		{name: "empty dropped", status: "", want: ""},
		{name: "unknown non-empty surfaced as pending (B2)", status: "someFutureStatus", want: scm.CheckBucketPending},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := azStatusBucket(tc.status); got != tc.want {
				t.Fatalf("azStatusBucket(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestIsCICheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		typeName   string
		genre      string
		statusName string
		want       bool
	}{
		{name: "build validation gates", typeName: "Build", want: true},
		{name: "external status check gates", typeName: "Status", want: true},
		{name: "component governance status gates", typeName: "Status", genre: "security", statusName: "ComponentGovernance", want: true},
		{name: "code coverage status gates", typeName: "Status", genre: "codecoverage", statusName: "coverage", want: true},
		{name: "min reviewers excluded (own type)", typeName: "Minimum number of reviewers", want: false},
		{name: "required reviewers excluded (own type)", typeName: "Required reviewers", want: false},
		{name: "comment requirements excluded (own type)", typeName: "Comment requirements", want: false},
		{name: "merge strategy excluded (own type)", typeName: "Require a merge strategy", want: false},
		{name: "work item linking excluded (own type)", typeName: "Work item linking", want: false},
		{name: "ownership enforcer excluded (own type)", typeName: "Ownership Enforcer", want: false},
		{name: "proof of presence excluded (own type)", typeName: "Proof Of Presence", want: false},
		{name: "code review compliance excluded (Status-type human sign-off)", typeName: "Status", genre: "microsoft-policy-service", statusName: "CodeReviewCompliancePolicy", want: false},
		{name: "code review compliance genre match is case-insensitive", typeName: "Status", genre: "Microsoft-Policy-Service", statusName: "codereviewcompliancepolicy", want: false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var e policyEval
			e.Configuration.Type.DisplayName = tc.typeName
			e.Configuration.Settings.StatusGenre = tc.genre
			e.Configuration.Settings.StatusName = tc.statusName
			if got := e.isCICheck(); got != tc.want {
				t.Fatalf("isCICheck(type=%q genre=%q name=%q) = %v, want %v", tc.typeName, tc.genre, tc.statusName, got, tc.want)
			}
		})
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
