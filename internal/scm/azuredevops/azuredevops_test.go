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

// reportedListPRJSON is the az repos pr list shape from issue #1042: webUrl,
// remoteUrl, and sshUrl are null, while repository.url is the REST endpoint
// and repository/project names identify the repo.
const reportedListPRJSON = `[{"pullRequestId":42,"codeReviewId":42,"status":"active","title":"feature work","sourceRefName":"refs/heads/feature","targetRefName":"refs/heads/main","mergeStatus":"succeeded","url":"https://dev.azure.com/myorg/_apis/git/repositories/11111111-2222-3333-4444-555555555555/pullRequests/42","repository":{"id":"11111111-2222-3333-4444-555555555555","name":"myrepo","url":"https://dev.azure.com/myorg/_apis/git/repositories/11111111-2222-3333-4444-555555555555","project":{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","name":"myproject","state":"wellFormed","visibility":"private"},"size":12345,"remoteUrl":null,"sshUrl":null,"webUrl":null,"isDisabled":false}}]`

func TestFindPRDiscoversExistingPRWithoutWebURL(t *testing.T) {
	t.Parallel()

	matching := `"name":"myrepo","project":{"name":"myproject"}`
	for _, tc := range []struct {
		name    string
		org     string
		output  string
		wantURL string
	}{
		{
			name:   "webUrl null",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{` + matching + `,"webUrl":null}}]`,
		},
		{
			name:   "webUrl omitted",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{` + matching + `}}]`,
		},
		{
			name:   "webUrl empty",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{` + matching + `,"webUrl":""}}]`,
		},
		{
			name:   "reported list payload",
			output: reportedListPRJSON,
		},
		{
			name:   "case-insensitive names",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{"name":"MyRepo","project":{"name":"MyProject"}}}]`,
		},
		{
			name:   "trimmed names",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{"name":"  myrepo  ","project":{"name":"  myproject  "}}}]`,
		},
		{
			name:    "visualstudio org uses configured canonical URL",
			org:     "https://myorg.visualstudio.com",
			output:  `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{` + matching + `}}]`,
			wantURL: "https://myorg.visualstudio.com/myproject/_git/myrepo/pullrequest/42",
		},
		{
			name:   "valid URL with matching names",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{` + matching + `,"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]`,
		},
		{
			name:   "valid URL with case-insensitive names",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{"name":"MyRepo","project":{"name":"MyProject"},"webUrl":"https://dev.azure.com/MyOrg/MyProject/_git/MyRepo"}}]`,
		},
		{
			name:   "valid URL with trimmed names",
			output: `[{"pullRequestId":42,"status":"active","targetRefName":"refs/heads/main","repository":{"name":"  myrepo  ","project":{"name":"  myproject  "},"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			org := testOrg
			if tc.org != "" {
				org = tc.org
			}
			wantURL := "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/42"
			if tc.wantURL != "" {
				wantURL = tc.wantURL
			}
			h := New(azdoTestCmdFactory(map[string]azdoTestResponse{
				"az repos pr list --source-branch feature --status active --target-branch main --organization " + org + " --project " + testProject + " --repository " + testRepo + " --output json": {
					stdout: tc.output + "\n",
				},
			}), func() bool { return true }, org, testProject, testRepo)

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
			if pr.URL != wantURL {
				t.Fatalf("FindPR() URL = %q, want %q", pr.URL, wantURL)
			}
			if pr.BaseBranch != "main" {
				t.Fatalf("FindPR() BaseBranch = %q, want main", pr.BaseBranch)
			}
		})
	}
}

func TestFindPRListsOnceWithoutShowLookup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		base string
		want []string
	}{
		{
			name: "empty base",
			base: "",
			want: []string{"repos", "pr", "list", "--source-branch", "feature", "--status", "active", "--organization", testOrg, "--project", testProject, "--repository", testRepo, "--output", "json"},
		},
		{
			name: "explicit target",
			base: "main",
			want: []string{"repos", "pr", "list", "--source-branch", "feature", "--status", "active", "--target-branch", "main", "--organization", testOrg, "--project", testProject, "--repository", testRepo, "--output", "json"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var rec []capturedCmd
			h := newCapturingHost(&rec, azdoTestResponse{stdout: reportedListPRJSON + "\n"})

			pr, err := h.FindPR(context.Background(), "feature", tc.base)
			if err != nil {
				t.Fatalf("FindPR() error = %v", err)
			}
			if pr == nil || pr.Number != "42" {
				t.Fatalf("FindPR() = %+v, want PR 42", pr)
			}
			if pr.URL != "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/42" {
				t.Fatalf("FindPR() URL = %q, want canonical browsable URL", pr.URL)
			}
			if pr.BaseBranch != "main" {
				t.Fatalf("FindPR() BaseBranch = %q, want main", pr.BaseBranch)
			}
			if len(rec) != 1 {
				t.Fatalf("recorded %d commands, want 1 list call", len(rec))
			}
			if rec[0].name != "az" {
				t.Fatalf("command = %q, want az", rec[0].name)
			}
			got := strings.Join(rec[0].args, " ")
			want := strings.Join(tc.want, " ")
			if got != want {
				t.Fatalf("args = %q, want %q", got, want)
			}
			for _, a := range rec[0].args {
				if a == "show" {
					t.Fatal("FindPR() issued az repos pr show; discovery must stay a single list call")
				}
			}
		})
	}
}

func TestFindPRAcceptsEquivalentOrganizationURLForms(t *testing.T) {
	t.Parallel()

	h := New(azdoTestCmdFactory(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --target-branch main --organization https://myorg.visualstudio.com --project " + testProject + " --repository " + testRepo + " --output json": {
			stdout: `[{"pullRequestId":42,"status":"active","repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]` + "\n",
		},
	}), func() bool { return true }, "https://myorg.visualstudio.com", testProject, testRepo)

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" {
		t.Fatalf("FindPR() = %+v, want PR 42", pr)
	}
	if pr.URL != "https://myorg.visualstudio.com/myproject/_git/myrepo/pullrequest/42" {
		t.Fatalf("FindPR() URL = %q, want configured canonical URL", pr.URL)
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

func TestFindPRRejectsInvalidResponse(t *testing.T) {
	t.Parallel()

	valid := `{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}`
	names := `"name":"myrepo","project":{"name":"myproject"}`
	for _, tc := range []struct {
		name    string
		output  string
		wantErr string
	}{
		{name: "malformed", output: "not json at all\n"},
		{name: "missing", output: "\n"},
		{name: "null", output: "null\n"},
		{name: "non-array", output: `{"pullRequestId":42,"repository":{` + names + `}}` + "\n"},
		{name: "missing identity", output: "[{}]\n"},
		{name: "later missing identity", output: "[" + valid + ",{}]\n", wantErr: "entry 1"},
		{name: "later missing names", output: "[" + valid + `,{"pullRequestId":43}]` + "\n", wantErr: "entry 1"},
		{name: "later foreign repository", output: "[" + valid + `,{"pullRequestId":43,"repository":{"name":"other","project":{"name":"myproject"}}}]` + "\n", wantErr: "entry 1"},
		{name: "later foreign project", output: "[" + valid + `,{"pullRequestId":43,"repository":{"name":"myrepo","project":{"name":"other"}}}]` + "\n", wantErr: "entry 1"},
		{
			name:   "foreign organization",
			output: `[{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/other/myproject/_git/myrepo"}}]` + "\n",
		},
		{
			name:   "foreign project",
			output: `[{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/myorg/other/_git/myrepo"}}]` + "\n",
		},
		{
			name:   "foreign repository",
			output: `[{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/other"}}]` + "\n",
		},
		{
			name:   "query suffix",
			output: `[{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo?view=files"}}]` + "\n",
		},
		{
			name:   "fragment suffix",
			output: `[{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo#discussion"}}]` + "\n",
		},
		{
			name:   "pull request suffix",
			output: `[{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/99"}}]` + "\n",
		},
		{name: "absent repository", output: `[{"pullRequestId":42}]` + "\n"},
		{name: "null repository", output: `[{"pullRequestId":42,"repository":null}]` + "\n"},
		{name: "missing repository name", output: `[{"pullRequestId":42,"repository":{"project":{"name":"myproject"}}}]` + "\n"},
		{name: "null repository name", output: `[{"pullRequestId":42,"repository":{"name":null,"project":{"name":"myproject"}}}]` + "\n"},
		{name: "blank repository name", output: `[{"pullRequestId":42,"repository":{"name":"","project":{"name":"myproject"}}}]` + "\n"},
		{name: "whitespace repository name", output: `[{"pullRequestId":42,"repository":{"name":"   ","project":{"name":"myproject"}}}]` + "\n"},
		{name: "missing project", output: `[{"pullRequestId":42,"repository":{"name":"myrepo"}}]` + "\n"},
		{name: "null project", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":null}}]` + "\n"},
		{name: "missing project name", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":{}}}]` + "\n"},
		{name: "null project name", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":{"name":null}}}]` + "\n"},
		{name: "blank project name", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":{"name":""}}}]` + "\n"},
		{name: "whitespace project name", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":{"name":"  "}}}]` + "\n"},
		{name: "mismatched repository name", output: `[{"pullRequestId":42,"repository":{"name":"other","project":{"name":"myproject"}}}]` + "\n"},
		{name: "mismatched project name", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":{"name":"other"}}}]` + "\n"},
		{name: "zero pullRequestId with names", output: `[{"pullRequestId":0,"repository":{` + names + `}}]` + "\n"},
		{name: "negative pullRequestId with names", output: `[{"pullRequestId":-1,"repository":{` + names + `}}]` + "\n"},
		{name: "missing pullRequestId with names", output: `[{"repository":{` + names + `}}]` + "\n"},
		{name: "contradictory metadata name", output: `[{"pullRequestId":42,"repository":{"name":"other","project":{"name":"myproject"},"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]` + "\n"},
		{name: "contradictory metadata project", output: `[{"pullRequestId":42,"repository":{"name":"myrepo","project":{"name":"other"},"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo"}}]` + "\n"},
		{name: "whitespace-only URL with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"   "}}]` + "\n"},
		{name: "malformed URL with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"not-a-url"}}]` + "\n"},
		{name: "unsupported scheme with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"ftp://dev.azure.com/myorg/myproject/_git/myrepo"}}]` + "\n"},
		{name: "foreign organization with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"https://dev.azure.com/other/myproject/_git/myrepo"}}]` + "\n"},
		{name: "foreign project with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"https://dev.azure.com/myorg/other/_git/myrepo"}}]` + "\n"},
		{name: "foreign repository with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"https://dev.azure.com/myorg/myproject/_git/other"}}]` + "\n"},
		{name: "query suffix with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo?view=files"}}]` + "\n"},
		{name: "fragment suffix with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo#discussion"}}]` + "\n"},
		{name: "pull request suffix with matching names", output: `[{"pullRequestId":42,"repository":{` + names + `,"webUrl":"https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/99"}}]` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHost(map[string]azdoTestResponse{
				"az repos pr list --source-branch feature --status active --target-branch main --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {
					stdout: tc.output,
				},
			})

			pr, err := h.FindPR(context.Background(), "feature", "main")
			if err == nil {
				t.Fatal("FindPR() error = nil, want parse error")
			}
			if !strings.Contains(err.Error(), "az repos pr list: parse response") {
				t.Fatalf("FindPR() error = %v, want provider parse context", err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("FindPR() error = %v, want %q", err, tc.wantErr)
			}
			if pr != nil {
				t.Fatalf("FindPR() = %+v, want nil on parse failure", pr)
			}
		})
	}
}

func TestCreatePRConstructsURL(t *testing.T) {
	t.Parallel()

	var rec []capturedCmd
	h := newCapturingHost(&rec, azdoTestResponse{
		// az returns an _apis/... url in the top-level field; it must NOT be used.
		stdout: `{"pullRequestId":7,"url":"https://dev.azure.com/myorg/_apis/git/repositories/abc/pullRequests/7"}` + "\n",
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
	// The command still carries the full create scope; the description is now a
	// temp-file reference (@<path>) rather than the inline body.
	if len(rec) != 1 {
		t.Fatalf("recorded %d commands, want 1", len(rec))
	}
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	wantPrefix := "az repos pr create --source-branch feature --target-branch main --title T --description @"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("command = %q, want prefix %q", got, wantPrefix)
	}
	wantSuffix := " --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("command = %q, want suffix %q", got, wantSuffix)
	}
	if rec[0].descContent != "B" {
		t.Fatalf("description file content = %q, want %q", rec[0].descContent, "B")
	}
}

func TestCreatePRAddsDraftFlagWhenConfigured(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		draft     bool
		wantDraft bool
	}{
		{name: "draft", draft: true, wantDraft: true},
		{name: "not draft", draft: false, wantDraft: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var rec []capturedCmd
			h := NewWithDraft(capturingCmdFactory(&rec, azdoTestResponse{
				stdout: `{"pullRequestId":7}` + "\n",
			}), func() bool { return true }, testOrg, testProject, testRepo, tc.draft)

			pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: "B"})
			if err != nil {
				t.Fatalf("CreatePR() error = %v", err)
			}
			if pr.Number != "7" {
				t.Fatalf("CreatePR() number = %q, want 7", pr.Number)
			}
			if len(rec) != 1 {
				t.Fatalf("recorded %d commands, want 1", len(rec))
			}
			got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
			if strings.Contains(got, "--draft true") != tc.wantDraft {
				t.Fatalf("command = %q, want --draft true present = %v", got, tc.wantDraft)
			}
		})
	}
}

func TestCreatePRTruncatesOverlongDescription(t *testing.T) {
	t.Parallel()

	// A body well over Azure DevOps' 4000-character description cap. The
	// connector clamps it before writing the temp file, so az never sees an
	// over-length description ("Invalid argument value. ... must not be longer
	// than 4000 characters").
	body := strings.Repeat("x", 5000)
	clamped := scm.ClampPRBody(body, scm.MaxPRBodyChars(scm.ProviderAzureDevOps))
	if scm.PRBodyLen(clamped) > 4000 {
		t.Fatalf("clamped description left %d units, want <= 4000", scm.PRBodyLen(clamped))
	}

	var rec []capturedCmd
	h := newCapturingHost(&rec, azdoTestResponse{stdout: `{"pullRequestId":7}` + "\n"})

	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: body})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "7" {
		t.Fatalf("CreatePR() number = %q, want 7", pr.Number)
	}
	if len(rec) != 1 {
		t.Fatalf("recorded %d commands, want 1", len(rec))
	}
	if rec[0].descContent != clamped {
		t.Fatalf("description file content len = %d, want clamped len %d", len(rec[0].descContent), len(clamped))
	}
}

// multilineDescriptionBody is a realistic PR body that would break a naive
// approach: a markdown heading, blank lines, prose, a bare horizontal rule
// (`---`), and a diff-style line (`--- a/file.go`). Passed inline on Windows the
// newlines truncate it; split per-line the `---`/`--- a/...` lines get misread
// as az options.
const multilineDescriptionBody = "## Intent\n" +
	"\n" +
	"Route the PR description through a temp file so multi-line bodies survive.\n" +
	"\n" +
	"---\n" +
	"\n" +
	"--- a/file.go\n" +
	"+++ b/file.go\n"

func TestCreatePRWritesMultilineDescriptionToFile(t *testing.T) {
	t.Parallel()

	var rec []capturedCmd
	h := newCapturingHost(&rec, azdoTestResponse{stdout: `{"pullRequestId":7}` + "\n"})

	if _, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: multilineDescriptionBody}); err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	assertDescriptionRoundTrips(t, rec, multilineDescriptionBody)
}

func TestUpdatePRWritesMultilineDescriptionToFile(t *testing.T) {
	t.Parallel()

	var rec []capturedCmd
	h := newCapturingHost(&rec, azdoTestResponse{stdout: `{"pullRequestId":42}` + "\n"})

	if _, err := h.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: "T", Body: multilineDescriptionBody}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	assertDescriptionRoundTrips(t, rec, multilineDescriptionBody)
}

// assertDescriptionRoundTrips verifies the recorded az command passed its PR
// description through a temp file (not inline): the --description value is an
// @<file> reference, the file held exactly body while the command ran, no
// argument contained a newline (which cmd.exe would truncate) or the raw body,
// and the temp file was cleaned up once the call returned.
func assertDescriptionRoundTrips(t *testing.T, rec []capturedCmd, body string) {
	t.Helper()
	if len(rec) != 1 {
		t.Fatalf("recorded %d commands, want 1", len(rec))
	}
	c := rec[0]
	if !strings.HasPrefix(c.descPath, "@") {
		t.Fatalf("--description value = %q, want an @<file> reference", c.descPath)
	}
	if !c.descExists {
		t.Fatal("description temp file did not exist while the command ran")
	}
	if c.descContent != body {
		t.Fatalf("description file content = %q, want %q", c.descContent, body)
	}
	for _, a := range c.args {
		if strings.Contains(a, "\n") {
			t.Fatalf("arg %q contains a newline; cmd.exe would truncate it at the first line", a)
		}
		if strings.Contains(a, body) {
			t.Fatalf("raw body leaked into arg %q; it must travel via the temp file", a)
		}
	}
	path := strings.TrimPrefix(c.descPath, "@")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file %q still exists after the call (stat err = %v); cleanup is missing", path, err)
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

// capturedCmd records one az invocation for tests that need to inspect how the
// PR description was passed. descContent is read from the @<file> reference at
// invocation time, while the temp file still exists (it is removed once the call
// returns), so tests can assert the body round-tripped exactly.
type capturedCmd struct {
	name        string
	args        []string
	descPath    string // the value following --description, including the leading "@"
	descContent string // contents of the temp file the description referenced
	descExists  bool   // whether that file existed and was readable during the run
}

func newCapturingHost(rec *[]capturedCmd, response azdoTestResponse) *Host {
	return New(capturingCmdFactory(rec, response), func() bool { return true }, testOrg, testProject, testRepo)
}

// capturingCmdFactory records every invocation (and the referenced description
// file's contents) into rec, and answers each one with the same fixed response.
func capturingCmdFactory(rec *[]capturedCmd, response azdoTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := capturedCmd{name: name, args: append([]string(nil), args...)}
		for i, a := range args {
			if a == "--description" && i+1 < len(args) {
				c.descPath = args[i+1]
				if p := strings.TrimPrefix(c.descPath, "@"); p != c.descPath {
					if data, err := os.ReadFile(p); err == nil {
						c.descContent = string(data)
						c.descExists = true
					}
				}
			}
		}
		*rec = append(*rec, c)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzdoHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"AZDO_TEST_HELPER=1",
			"AZDO_TEST_STDOUT="+response.stdout,
			"AZDO_TEST_STDERR="+response.stderr,
			fmt.Sprintf("AZDO_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
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
