package azuredevops

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

func TestOrgProjectRepo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		remoteURL   string
		wantOrg     string
		wantProject string
		wantRepo    string
	}{
		{
			name:        "ssh scp-style",
			remoteURL:   "git@ssh.dev.azure.com:v3/talroo/Product/ai-knowledge-base",
			wantOrg:     "https://dev.azure.com/talroo",
			wantProject: "Product",
			wantRepo:    "ai-knowledge-base",
		},
		{
			name:        "https",
			remoteURL:   "https://dev.azure.com/talroo/Product/_git/ai-knowledge-base",
			wantOrg:     "https://dev.azure.com/talroo",
			wantProject: "Product",
			wantRepo:    "ai-knowledge-base",
		},
		{
			name:        "https with userinfo",
			remoteURL:   "https://user@dev.azure.com/talroo/Product/_git/my-repo",
			wantOrg:     "https://dev.azure.com/talroo",
			wantProject: "Product",
			wantRepo:    "my-repo",
		},
		{
			name:        "visualstudio.com legacy",
			remoteURL:   "https://talroo.visualstudio.com/Product/_git/ai-knowledge-base",
			wantOrg:     "https://dev.azure.com/talroo",
			wantProject: "Product",
			wantRepo:    "ai-knowledge-base",
		},
		{
			name:        "empty",
			remoteURL:   "",
			wantOrg:     "",
			wantProject: "",
			wantRepo:    "",
		},
		{
			name:        "not azure",
			remoteURL:   "https://github.com/user/repo.git",
			wantOrg:     "",
			wantProject: "",
			wantRepo:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org, project, repo := OrgProjectRepo(tc.remoteURL)
			if org != tc.wantOrg {
				t.Errorf("OrgProjectRepo(%q) org = %q, want %q", tc.remoteURL, org, tc.wantOrg)
			}
			if project != tc.wantProject {
				t.Errorf("OrgProjectRepo(%q) project = %q, want %q", tc.remoteURL, project, tc.wantProject)
			}
			if repo != tc.wantRepo {
				t.Errorf("OrgProjectRepo(%q) repo = %q, want %q", tc.remoteURL, repo, tc.wantRepo)
			}
		})
	}
}

func TestFindPR(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr list --status active --repository my-repo --project MyProject --org https://dev.azure.com/myorg --source-branch feat/test --target-branch main -o json": {
			stdout: `[{"pullRequestId": 42, "url": "https://dev.azure.com/myorg/MyProject/_git/my-repo/pullrequest/42"}]` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	pr, err := host.FindPR(context.Background(), "feat/test", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() returned nil PR")
	}
	if pr.Number != "42" {
		t.Fatalf("PR.Number = %q, want 42", pr.Number)
	}
}

func TestFindPR_NoResults(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr list --status active --repository my-repo --project MyProject --org https://dev.azure.com/myorg --source-branch feat/test --target-branch main -o json": {
			stdout: "[]\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	pr, err := host.FindPR(context.Background(), "feat/test", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil", pr)
	}
}

func TestCreatePR(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr create --source-branch feat/test --target-branch main --title fix: test --description body --repository my-repo --project MyProject --org https://dev.azure.com/myorg -o json": {
			stdout: `{"pullRequestId": 99, "url": "https://dev.azure.com/myorg/MyProject/_git/my-repo/pullrequest/99"}` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	pr, err := host.CreatePR(context.Background(), "feat/test", "main", scm.PRContent{
		Title: "fix: test",
		Body:  "body",
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "99" {
		t.Fatalf("CreatePR() PR = %+v, want #99", pr)
	}
}

func TestGetPRState(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr show --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `{"pullRequestId": 42, "status": "completed", "mergeStatus": "succeeded"}` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateMerged)
	}
}

func TestGetPRState_Active(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr show --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `{"pullRequestId": 42, "status": "active", "mergeStatus": "succeeded"}` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	state, _ := host.GetPRState(context.Background(), &scm.PR{Number: "42"})
	if state != scm.PRStateOpen {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateOpen)
	}
}

func TestGetMergeableState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		mergeStatus string
		want        scm.MergeableState
	}{
		{"succeeded", "succeeded", scm.MergeableOK},
		{"conflicts", "conflicts", scm.MergeableConflict},
		{"rejectedByPolicy", "rejectedByPolicy", scm.MergeableConflict},
		{"queued", "queued", scm.MergeablePending},
		{"notSet", "notSet", scm.MergeablePending},
		{"empty", "", scm.MergeablePending},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := New(azTestCmdFactory(map[string]azTestResponse{
				"az repos pr show --id 42 --org https://dev.azure.com/myorg -o json": {
					stdout: fmt.Sprintf(`{"pullRequestId": 42, "status": "active", "mergeStatus": "%s"}`+"\n", tc.mergeStatus),
				},
			}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

			state, _ := host.GetMergeableState(context.Background(), &scm.PR{Number: "42"})
			if state != tc.want {
				t.Fatalf("GetMergeableState() = %q, want %q", state, tc.want)
			}
		})
	}
}

func TestGetChecks(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr policy list --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `[{"status":"approved","configuration":{"isBlocking":false,"isEnabled":true,"type":{"displayName":"Build"},"settings":{"displayName":"Ai Review","buildDefinitionId":351}},"context":{"buildId":85144,"buildIsNotCurrent":false,"lastMergeSourceCommitId":"abc123"}}]` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "Ai Review" {
		t.Fatalf("checks[0].Name = %q, want Ai Review", checks[0].Name)
	}
	if checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0].Bucket = %q, want pass", checks[0].Bucket)
	}
	if checks[0].CompletedAt.IsZero() {
		t.Fatalf("checks[0].CompletedAt should be non-zero for terminal status")
	}
}

func TestGetChecks_Rejected(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr policy list --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `[{"status":"rejected","configuration":{"isBlocking":true,"isEnabled":true,"type":{"displayName":"Build"},"settings":{"displayName":"CI Build","buildDefinitionId":352}},"context":{"buildId":85145,"buildIsNotCurrent":false,"lastMergeSourceCommitId":"abc123"}}]` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[0].Bucket = %q, want fail", checks[0].Bucket)
	}
}

func TestGetChecks_NoPolicies(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr policy list --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: "[]\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("len(checks) = %d, want 0", len(checks))
	}
}

func TestGetReviewPass(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr policy list --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `[{"status":"approved","configuration":{"isBlocking":false,"isEnabled":true,"type":{"displayName":"Build"},"settings":{"displayName":"Ai Review","buildDefinitionId":351}},"context":{"buildId":85144,"buildIsNotCurrent":false,"lastMergeSourceCommitId":"abc123"}}]` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	pass, err := host.GetReviewPass(context.Background(), &scm.PR{Number: "42"}, "Ai Review")
	if err != nil {
		t.Fatalf("GetReviewPass() error = %v", err)
	}
	if !pass.Ran {
		t.Fatal("pass.Ran = false, want true")
	}
	if !pass.Complete {
		t.Fatal("pass.Complete = false, want true")
	}
	if pass.ForSHA != "abc123" {
		t.Fatalf("pass.ForSHA = %q, want abc123", pass.ForSHA)
	}
}

func TestGetReviewPass_Running(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr policy list --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `[{"status":"running","configuration":{"isBlocking":false,"isEnabled":true,"type":{"displayName":"Build"},"settings":{"displayName":"Ai Review","buildDefinitionId":351}},"context":{"buildId":85144,"buildIsNotCurrent":false,"lastMergeSourceCommitId":"abc123"}}]` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	pass, _ := host.GetReviewPass(context.Background(), &scm.PR{Number: "42"}, "Ai Review")
	if !pass.Ran {
		t.Fatal("pass.Ran = false, want true")
	}
	if pass.Complete {
		t.Fatal("pass.Complete = true, want false")
	}
}

func TestGetReviewPass_NotFound(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos pr policy list --id 42 --org https://dev.azure.com/myorg -o json": {
			stdout: `[]` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	pass, _ := host.GetReviewPass(context.Background(), &scm.PR{Number: "42"}, "Ai Review")
	if pass.Ran {
		t.Fatal("pass.Ran = true, want false")
	}
}

func TestGetReviewThreads(t *testing.T) {
	t.Parallel()

	host := New(azTestCmdFactory(map[string]azTestResponse{
		"az repos show --repository my-repo --org https://dev.azure.com/myorg -o json": {
			stdout: `{"id": "repo-guid-123"}` + "\n",
		},
		"az devops invoke --area git --resource pullRequestThreads --route-parameters project=MyProject repositoryId=repo-guid-123 pullRequestId=42 --org https://dev.azure.com/myorg --api-version 7.1 -o json": {
			stdout: `{"value":[
				{"id":282696,"status":"fixed","threadContext":{"filePath":"/scripts/run.sh","rightFileStart":{"line":42}},"comments":[{"author":{"displayName":"Product Build Service (talroo)"},"commentType":"text","content":"BETA: fix this"}]},
				{"id":282707,"status":null,"threadContext":null,"comments":[{"author":{"displayName":"Microsoft.VisualStudio.Services.TFS"},"commentType":"system","content":"ref updated"}]},
				{"id":282708,"status":"active","threadContext":{"filePath":"/src/main.go","rightFileStart":{"line":10}},"comments":[{"author":{"displayName":"Product Build Service (talroo)"},"commentType":"text","content":"unresolved issue"}]}
			]}` + "\n",
		},
	}), nil, "https://dev.azure.com/myorg", "MyProject", "my-repo")

	threads, err := host.GetReviewThreads(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetReviewThreads() error = %v", err)
	}
	// system comment filtered out, so 2 text threads
	if len(threads) != 2 {
		t.Fatalf("len(threads) = %d, want 2", len(threads))
	}

	// First thread: fixed, bot
	if threads[0].ID != "282696" {
		t.Fatalf("threads[0].ID = %q, want 282696", threads[0].ID)
	}
	if !threads[0].Resolved {
		t.Fatal("threads[0].Resolved = false, want true (fixed)")
	}
	if threads[0].File != "/scripts/run.sh" {
		t.Fatalf("threads[0].File = %q, want /scripts/run.sh", threads[0].File)
	}
	if threads[0].Line != 42 {
		t.Fatalf("threads[0].Line = %d, want 42", threads[0].Line)
	}
	if threads[0].Author != "Product Build Service (talroo)" {
		t.Fatalf("threads[0].Author = %q", threads[0].Author)
	}

	// Second thread: active, unresolved
	if threads[1].ID != "282708" {
		t.Fatalf("threads[1].ID = %q, want 282708", threads[1].ID)
	}
	if threads[1].Resolved {
		t.Fatal("threads[1].Resolved = true, want false (active)")
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	host := New(nil, nil, "", "", "")
	caps := host.Capabilities()
	if !caps.MergeableState {
		t.Error("MergeableState should be true")
	}
	if !caps.FailedCheckLogs {
		t.Error("FailedCheckLogs should be true")
	}
	if !caps.ReviewComments {
		t.Error("ReviewComments should be true")
	}
}

func TestProvider(t *testing.T) {
	t.Parallel()

	host := New(nil, nil, "", "", "")
	if host.Provider() != scm.ProviderAzureDevOps {
		t.Fatalf("Provider() = %q, want %q", host.Provider(), scm.ProviderAzureDevOps)
	}
}

func TestNormalizePRState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  scm.PRState
	}{
		{"active", scm.PRStateOpen},
		{"completed", scm.PRStateMerged},
		{"abandoned", scm.PRStateClosed},
		{"ACTIVE", scm.PRStateOpen},
	}
	for _, tc := range cases {
		if got := normalizePRState(tc.input); got != tc.want {
			t.Errorf("normalizePRState(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestPolicyStatusToBucket(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  scm.CheckBucket
	}{
		{"approved", scm.CheckBucketPass},
		{"rejected", scm.CheckBucketFail},
		{"queued", scm.CheckBucketPending},
		{"running", scm.CheckBucketPending},
		{"notApplicable", scm.CheckBucketSkip},
		{"", ""},
	}
	for _, tc := range cases {
		if got := policyStatusToBucket(tc.input); got != tc.want {
			t.Errorf("policyStatusToBucket(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestIsThreadResolved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  bool
	}{
		{"active", false},
		{"pending", false},
		{"", false},
		{"fixed", true},
		{"closed", true},
		{"wontFix", true},
		{"byDesign", true},
	}
	for _, tc := range cases {
		if got := isThreadResolved(tc.input); got != tc.want {
			t.Errorf("isThreadResolved(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- test infrastructure ---

type azTestResponse struct {
	stdout string
	stderr string
	code   int
}

func azTestCmdFactory(responses map[string]azTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = azTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzureDevOpsHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"AZ_TEST_HELPER=1",
			"AZ_TEST_STDOUT="+response.stdout,
			"AZ_TEST_STDERR="+response.stderr,
			fmt.Sprintf("AZ_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestAzureDevOpsHelperProcess(t *testing.T) {
	if os.Getenv("AZ_TEST_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("AZ_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("AZ_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	code := 0
	if c := os.Getenv("AZ_TEST_EXIT_CODE"); c != "" {
		if n, err := fmt.Sscanf(c, "%d", &code); err != nil || n != 1 {
			code = 0
		}
	}
	if code != 0 {
		os.Exit(code)
	}
	// drain stdin so callers that pipe data don't get SIGPIPE
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// ensure time is imported
var _ = time.Now
