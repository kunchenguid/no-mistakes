package bitbucket

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestParseRepoRef(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    RepoRef
		wantErr bool
	}{
		{"https url", "https://bitbucket.org/myws/myrepo.git", RepoRef{Workspace: "myws", RepoSlug: "myrepo"}, false},
		{"https url no dotgit", "https://bitbucket.org/myws/myrepo", RepoRef{Workspace: "myws", RepoSlug: "myrepo"}, false},
		{"scp-like", "git@bitbucket.org:myws/myrepo.git", RepoRef{Workspace: "myws", RepoSlug: "myrepo"}, false},
		{"unsupported host", "https://github.com/myws/myrepo.git", RepoRef{}, true},
		{"missing repo segment", "https://bitbucket.org/myws", RepoRef{}, true},
		{"empty", "", RepoRef{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRepoRef(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRepoRef(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepoRef(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ParseRepoRef(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCapabilitiesDeclinesMergeableState(t *testing.T) {
	h := New(nil, nil, RepoRef{}, false)
	caps := h.Capabilities()
	if caps.MergeableState {
		t.Fatal("expected MergeableState capability to be declined")
	}
	if !caps.FailedCheckLogs {
		t.Fatal("expected FailedCheckLogs capability to be supported")
	}
}

func TestGetMergeableStateReturnsErrUnsupported(t *testing.T) {
	h := New(nil, nil, RepoRef{}, false)
	if _, err := h.GetMergeableState(context.Background(), &scm.PR{}); err != scm.ErrUnsupported {
		t.Fatalf("GetMergeableState() error = %v, want ErrUnsupported", err)
	}
}

func TestAvailableReturnsErrorWhenCLIMissing(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return false }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.Available(context.Background()); err == nil {
		t.Fatal("expected error when twg CLI is not installed")
	}
}

func TestAvailableReturnsErrorOnAuthFailure(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg whoami": {stderr: "not logged in", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.Available(context.Background()); err == nil {
		t.Fatal("expected error when twg is not authenticated")
	}
}

func TestAvailableSucceedsWhenAuthenticated(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg whoami": {stdout: "Andrew Smith\n"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
}

func TestFindPRMatchesOpenPRForSourceBranch(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --dest main --workspace ws --repo repo -o json": {
			stdout: `[{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" || pr.URL != "https://bitbucket.org/ws/repo/pull-requests/42" {
		t.Fatalf("FindPR() = %+v, want matched PR", pr)
	}
}

func TestFindPRReturnsNilWhenNoneOpen(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json": {stdout: "[]"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil", pr)
	}
}

func TestFindPRReturnsErrorOnNonJSONOutput(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json": {stdout: ""},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error for output with no JSON delimiter")
	}
}

func TestFindPRReturnsErrorOnMalformedJSON(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json": {stdout: `[{"id":`},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestFindPRReturnsErrorOnNullListing(t *testing.T) {
	// A literal JSON `null` (e.g. a nil slice marshaled at the top level) must
	// not be read as "no open PRs" - that reading would risk the PR step
	// creating a duplicate PR.
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json": {stdout: "null"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error for a null listing, not a silent 'no open PRs'")
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json": {stderr: "boom", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error propagated from twg CLI failure")
	}
}

func TestCreatePRWritesDescriptionToFileAndParsesResult(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{
		stdout: `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	body := "line one\nline two\n"
	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: body})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "42" || pr.URL != "https://bitbucket.org/ws/repo/pull-requests/42" {
		t.Fatalf("CreatePR() = %+v", pr)
	}
	assertDescriptionRoundTrips(t, rec, body)
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	if !strings.Contains(got, "--title T --source feature") || !strings.Contains(got, "--dest main") || !strings.Contains(got, "--workspace ws --repo repo -o json") {
		t.Fatalf("command = %q, missing expected flags", got)
	}
	if strings.Contains(got, "--draft") {
		t.Fatalf("command = %q, want no --draft when disabled", got)
	}
}

func TestCreatePRAddsDraftFlagWhenConfigured(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{
		stdout: `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, true)

	if _, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: "b"}); err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	if !strings.Contains(got, "--draft") {
		t.Fatalf("command = %q, want --draft when configured", got)
	}
}

func TestUpdatePROmitsTitleWhenEmpty(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{
		stdout: `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: "", Body: "new body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	if strings.Contains(got, "--title") {
		t.Fatalf("command = %q, want no --title when content.Title is empty", got)
	}
	if !strings.Contains(got, "--pull-request 42") {
		t.Fatalf("command = %q, missing --pull-request", got)
	}
	assertDescriptionRoundTrips(t, rec, "new body")
}

func TestUpdatePRFailsClosedWithoutIdentity(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if _, err := h.UpdatePR(context.Background(), &scm.PR{}, scm.PRContent{}); err == nil {
		t.Fatal("expected error for missing PR number")
	}
	if _, err := h.UpdatePR(context.Background(), nil, scm.PRContent{}); err == nil {
		t.Fatal("expected error for nil PR")
	}
}

func TestGetPRStateNormalizesState(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --workspace ws --repo repo -o json": {
			stdout: `{"id":42,"state":"MERGED","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	state, err := h.GetPRState(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want MERGED", state)
	}
}

func TestGetChecksReadsHydratedStatuses(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json": {
			stdout: `{"id":42,"_statuses":[{"key":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/42"}]}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketFail || checks[0].ExecutionID != "42" {
		t.Fatalf("checks[0] = %+v", checks[0])
	}
}

func TestFetchFailedCheckTargetLogsFetchesFailedStepsPerBuild(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json": {
			stdout: `{"id":42,"source":{"commit":{"hash":"abc123"}},"_statuses":[{"name":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/1"}]}`,
		},
		"twg bb pipeline get --pipeline 1 --logs --failed-steps --lines 0 --workspace ws --repo repo": {
			stdout: "error log output\n",
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123def456", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "error log output" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "error log output")
	}
}

func TestFetchFailedCheckTargetLogsRefusesWhenPRSourceCommitIsStale(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json": {
			stdout: `{"id":42,"source":{"commit":{"hash":"oldcommit"}},"_statuses":[{"name":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/1"}]}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "newcommitsha", []string{"build"}); err == nil {
		t.Fatal("expected error when the PR's source commit does not match the requested head")
	}
}

func TestFetchFailedCheckTargetLogsReturnsEmptyForNoTargets(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123", nil)
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want empty", logs)
	}
}

func TestLatestStatusesPicksNewestByTimestampNotArrayOrder(t *testing.T) {
	// The older pass appears first in the array; only the timestamp shows
	// it's stale. If LatestStatuses trusted array order it would keep the
	// pass and drop the newer failure.
	statuses := []CommitStatus{
		{Key: "build", State: "SUCCESSFUL", UpdatedOn: "2026-01-01T00:00:00Z"},
		{Key: "build", State: "FAILED", UpdatedOn: "2026-01-02T00:00:00Z"},
	}
	latest := LatestStatuses(statuses)
	if len(latest) != 1 || latest[0].State != "FAILED" {
		t.Fatalf("LatestStatuses() = %+v, want the newer FAILED status", latest)
	}
}

func TestLatestStatusesPrefersCreatedOnOverUpdatedOnForOrdering(t *testing.T) {
	// The older build (created first) has its updated_on touched after the
	// newer build was created - e.g. Bitbucket re-delivering the same status.
	// Ranking by updated_on would let the older build's SUCCESSFUL status
	// outrank the newer build's FAILED status and report the wrong verdict.
	statuses := []CommitStatus{
		{Key: "build", State: "SUCCESSFUL", CreatedOn: "2026-01-01T00:00:00Z", UpdatedOn: "2026-01-03T00:00:00Z"},
		{Key: "build", State: "FAILED", CreatedOn: "2026-01-02T00:00:00Z", UpdatedOn: "2026-01-02T00:00:00Z"},
	}
	latest := LatestStatuses(statuses)
	if len(latest) != 1 || latest[0].State != "FAILED" {
		t.Fatalf("LatestStatuses() = %+v, want the status with the newer created_on (FAILED)", latest)
	}
}

func TestGetChecksReportsTheNewerStatusRegardlessOfHydrationOrder(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json": {
			stdout: `{"id":42,"_statuses":[` +
				`{"key":"build","state":"SUCCESSFUL","updated_on":"2026-01-01T00:00:00Z"},` +
				`{"key":"build","state":"FAILED","updated_on":"2026-01-02T00:00:00Z"}` +
				`]}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("GetChecks() = %+v, want a single failing check", checks)
	}
}

func TestBytesTrimToJSONSkipsBanner(t *testing.T) {
	if got := bytesTrimToJSON([]byte("notice: update available\n[]")); string(got) != "[]" {
		t.Fatalf("bytesTrimToJSON() = %q, want %q", got, "[]")
	}
	if got := bytesTrimToJSON([]byte("no json here")); got != nil {
		t.Fatalf("bytesTrimToJSON() = %q, want nil", got)
	}
}

// --- test doubles -----------------------------------------------------------

type bbTestResponse struct {
	stdout string
	stderr string
	code   int
}

func bbTestCmdFactory(responses map[string]bbTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = bbTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBBHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"BB_TEST_HELPER=1",
			"BB_TEST_STDOUT="+response.stdout,
			"BB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("BB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

type capturedCmd struct {
	name        string
	args        []string
	descPath    string
	descExists  bool
	descContent string
}

// capturingCmdFactory records every invocation (and the referenced
// --description-file's contents) into rec, and answers each one with the
// same fixed response.
func capturingCmdFactory(rec *[]capturedCmd, response bbTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := capturedCmd{name: name, args: append([]string(nil), args...)}
		for i, a := range args {
			if a == "--description-file" && i+1 < len(args) {
				c.descPath = args[i+1]
				if data, err := os.ReadFile(c.descPath); err == nil {
					c.descContent = string(data)
					c.descExists = true
				}
			}
		}
		*rec = append(*rec, c)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBBHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"BB_TEST_HELPER=1",
			"BB_TEST_STDOUT="+response.stdout,
			"BB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("BB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

// assertDescriptionRoundTrips verifies the recorded twg command passed its PR
// description through a temp file (not inline): the file held exactly body
// while the command ran, no argument contained the raw body, and (since the
// caller always defers removal) it existed at capture time.
func assertDescriptionRoundTrips(t *testing.T, rec []capturedCmd, body string) {
	t.Helper()
	if len(rec) != 1 {
		t.Fatalf("recorded %d commands, want 1", len(rec))
	}
	c := rec[0]
	if c.descPath == "" {
		t.Fatal("--description-file was not passed")
	}
	if !c.descExists {
		t.Fatal("description temp file did not exist while the command ran")
	}
	if c.descContent != body {
		t.Fatalf("description file content = %q, want %q", c.descContent, body)
	}
	for _, a := range c.args {
		if a != body && strings.Contains(a, body) {
			t.Fatalf("raw body leaked into arg %q; it must travel via the temp file", a)
		}
	}
}

func TestBBHelperProcess(t *testing.T) {
	if os.Getenv("BB_TEST_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("BB_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("BB_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("BB_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
