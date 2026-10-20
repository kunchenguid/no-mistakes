package gitea

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestSplitOwnerRepo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"testadmin/testrepo", "testadmin", "testrepo", true},
		{"/testadmin/testrepo/", "testadmin", "testrepo", true},
		{"testadmin", "", "", false},
		{"", "", "", false},
		{"testadmin/", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := splitOwnerRepo(tc.in)
		if owner != tc.wantOwner || repo != tc.wantRepo || ok != tc.wantOK {
			t.Errorf("splitOwnerRepo(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.in, owner, repo, ok, tc.wantOwner, tc.wantRepo, tc.wantOK)
		}
	}
}

func TestGiteaStatusBucket(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status     string
		conclusion string
		want       scm.CheckBucket
	}{
		{"completed", "success", scm.CheckBucketPass},
		{"completed", "failure", scm.CheckBucketFail},
		{"completed", "cancelled", scm.CheckBucketCancel},
		{"completed", "canceled", scm.CheckBucketCancel},
		{"completed", "skipped", scm.CheckBucketSkip},
		{"completed", "unknown-conclusion", ""},
		{"queued", "", scm.CheckBucketPending},
		{"in_progress", "", scm.CheckBucketPending},
		{"pending", "", scm.CheckBucketPending},
		{"waiting", "", scm.CheckBucketPending},
		{"", "", scm.CheckBucketPending},
	}
	for _, tc := range cases {
		if got := giteaStatusBucket(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("giteaStatusBucket(%q, %q) = %q, want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
}

func TestNormalizeGiteaPRState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want scm.PRState
	}{
		{"open", scm.PRStateOpen},
		{"Open", scm.PRStateOpen},
		{"closed", scm.PRStateClosed},
	}
	for _, tc := range cases {
		if got := normalizeGiteaPRState(tc.raw); got != tc.want {
			t.Errorf("normalizeGiteaPRState(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCapabilitiesDeclinesMergeableState(t *testing.T) {
	t.Parallel()

	// Gitea's PR `mergeable` field has a documented upstream reliability bug
	// (go-gitea/gitea#25849): it can stick `false` after a conflict is
	// actually resolved. Trusting it is worse than declining the capability.
	host := New(nil, nil, "", scm.TeaAccount{}, "")
	caps := host.Capabilities()
	if caps.MergeableState {
		t.Fatal("Capabilities().MergeableState = true, want false")
	}
	if !caps.FailedCheckLogs {
		t.Fatal("Capabilities().FailedCheckLogs = false, want true")
	}
}

func TestGetMergeableStateReturnsErrUnsupported(t *testing.T) {
	t.Parallel()

	host := New(nil, nil, "", scm.TeaAccount{}, "")
	if _, err := host.GetMergeableState(context.Background(), &scm.PR{Number: "1"}); err != scm.ErrUnsupported {
		t.Fatalf("GetMergeableState() error = %v, want scm.ErrUnsupported", err)
	}
}

func TestAvailableFailsClosedWithoutConfiguredLogin(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(nil), func() bool { return true }, "gitea.example.com", scm.TeaAccount{}, "owner/repo")
	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error when no tea login is configured for the host")
	}
}

func TestAvailableScopesToConfiguredLogin(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work /user": {stdout: `{"login":"someuser"}`},
	}), func() bool { return true }, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestAvailableReturnsErrorOnAuthFailure(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work /user": {stderr: "invalid username, password or token\n", code: 1},
	}), func() bool { return true }, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error on auth failure")
	}
}

func TestAvailableReturnsErrorWhenCLIMissing(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(nil), func() bool { return false }, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")
	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error when tea CLI is missing")
	}
}

func TestFindPRMatchesByHeadBranch(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[{"index":"3","title":"other","state":"open","url":"https://gitea.example.com/owner/repo/pulls/3","head":"other-branch","base":"main"},` +
				`{"index":"7","title":"mine","state":"open","url":"https://gitea.example.com/owner/repo/pulls/7","head":"feature/x","base":"main"}]`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "7" || pr.URL != "https://gitea.example.com/owner/repo/pulls/7" {
		t.Fatalf("FindPR() = %+v, want PR #7", pr)
	}
	if pr.BaseBranch != "main" {
		t.Fatalf("FindPR() BaseBranch = %q, want main", pr.BaseBranch)
	}
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[{"index":"1","title":"a","state":"open","url":"https://gitea.example.com/owner/repo/pulls/1","head":"feature/x","base":"release/1.0"}]`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil (base branch mismatch)", pr)
	}
}

func TestFindPRReturnsNilWhenNoneOpen(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[]`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil", pr)
	}
}

func TestFindPRReturnsErrorOnMalformedJSON(t *testing.T) {
	t.Parallel()

	// A decoding failure on nonempty output must surface as an error, not be
	// swallowed into a "not found" result: otherwise the PR step reads a
	// provider response failure as an absent PR and attempts a duplicate
	// create, or reports a misleading creation error.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[{"index":"7","title":"mine"`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err == nil {
		t.Fatalf("FindPR() error = nil, want error for malformed JSON output; pr = %+v", pr)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil PR alongside the error", pr)
	}
}

func TestFindPRReturnsErrorOnNonJSONOutput(t *testing.T) {
	t.Parallel()

	// Nonempty stdout that exits 0 but contains neither '[' nor '{' (e.g. a
	// stray banner or plain-text notice) must also surface as an error, not
	// be swallowed into a "not found" result via bytesTrimToJSON's empty
	// return - the same duplicate-create hazard as malformed JSON.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: "a new release of tea is available\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err == nil {
		t.Fatalf("FindPR() error = nil, want error for non-JSON output; pr = %+v", pr)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil PR alongside the error", pr)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stderr: "gitea unavailable\n",
			code:   1,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "tea pulls list") {
		t.Fatalf("FindPR() error = %v, want tea pulls list context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestCreatePRUsesFindPRForStructuredResult(t *testing.T) {
	t.Parallel()

	// `tea pulls create` has no --output json flag; the robust path is to
	// re-list the PR by head branch after creating it rather than trust a
	// text-scrape of the human-readable create output (which can itself
	// contain http(s) URLs inside the PR body).
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls create --repo owner/repo --login work --head feature/x --base main --title t --description See http://example.com/evidence for details": {
			stdout: "  # #9 t (open)\n\n  See http://example.com/evidence for details\n\n  http://gitea.example.com/owner/repo/pulls/9\n",
		},
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[{"index":"9","title":"t","state":"open","url":"http://gitea.example.com/owner/repo/pulls/9","head":"feature/x","base":"main"}]`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.CreatePR(context.Background(), "feature/x", "main", scm.PRContent{Title: "t", Body: "See http://example.com/evidence for details"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "9" || pr.URL != "http://gitea.example.com/owner/repo/pulls/9" {
		t.Fatalf("CreatePR() = %+v, want PR #9", pr)
	}
}

func TestCreatePRFallsBackToOutputScanWhenRelistFails(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls create --repo owner/repo --login work --head feature/x --base main --title t --description body": {
			stdout: "  # #9 t (open)\n\n  body\n\n  http://gitea.example.com/owner/repo/pulls/9\n",
		},
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stderr: "boom\n",
			code:   1,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.CreatePR(context.Background(), "feature/x", "main", scm.PRContent{Title: "t", Body: "body"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.URL != "http://gitea.example.com/owner/repo/pulls/9" || pr.Number != "9" {
		t.Fatalf("CreatePR() = %+v, want PR #9 from output scan fallback", pr)
	}
}

func TestCreatePRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls create --repo owner/repo --login work --head feature/x --base main --title t --description body": {
			stderr: "gitea unavailable\n",
			code:   1,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr, err := host.CreatePR(context.Background(), "feature/x", "main", scm.PRContent{Title: "t", Body: "body"})
	if err == nil {
		t.Fatal("CreatePR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "tea pulls create") {
		t.Fatalf("CreatePR() error = %v, want tea pulls create context", err)
	}
	if pr != nil {
		t.Fatalf("CreatePR() PR = %+v, want nil", pr)
	}
}

func TestUpdatePRUsesNumberWhenPresent(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls edit 7 --repo owner/repo --login work --title updated --description body": {
			stdout: "updated\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr := &scm.PR{Number: "7", URL: "http://gitea.example.com/owner/repo/pulls/7"}
	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "updated", Body: "body"})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() returned unexpected PR: %+v", updated)
	}
}

func TestUpdatePRFallsBackToNumberFromURL(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls edit 7 --repo owner/repo --login work --title updated --description body": {
			stdout: "updated\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	pr := &scm.PR{URL: "http://gitea.example.com/owner/repo/pulls/7"}
	if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "updated", Body: "body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
}

func TestSetPRBaseBranchPatchesPullViaTeaAPI(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work --method PATCH --field base=epic/feature /repos/owner/repo/pulls/7": {
			stdout: `{"index":7,"base":{"ref":"epic/feature"}}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	if err := host.SetPRBaseBranch(context.Background(), &scm.PR{Number: "7"}, "epic/feature"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
}

func TestSetPRBaseBranchFallsBackToNumberFromURL(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work --method PATCH --field base=epic/feature /repos/owner/repo/pulls/7": {
			stdout: `{"index":7}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	if err := host.SetPRBaseBranch(context.Background(), &scm.PR{URL: "https://gitea.example.com/owner/repo/pulls/7"}, "epic/feature"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
}

func TestSetPRBaseBranchFailsClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(nil), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")
	if err := host.SetPRBaseBranch(context.Background(), &scm.PR{}, "epic/feature"); err == nil {
		t.Fatal("SetPRBaseBranch() with no PR identity: expected error, got nil")
	}
}

func TestGetPRStateReportsMergedBeforeClosed(t *testing.T) {
	t.Parallel()

	// Gitea reports state="closed" AND hasMerged=true for a merged PR; a
	// merged PR must never be reported as plain CLOSED.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"closed","hasMerged":true}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateMerged)
	}
}

func TestGetPRStateReportsClosedWhenNotMerged(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"closed","hasMerged":false}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateClosed {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateClosed)
	}
}

func TestGetPRStateReportsOpen(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","hasMerged":false}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateOpen {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateOpen)
	}
}

func TestGetChecksFetchesJobsForMatchingHeadRun(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123","html_url":"http://gitea.example.com/owner/repo/actions/runs/10/jobs/10","completed_at":"2026-08-20T06:34:50Z"},` +
				`{"id":11,"name":"build2","status":"completed","conclusion":"success","head_sha":"abc123","html_url":"http://gitea.example.com/owner/repo/actions/runs/10/jobs/11","completed_at":"2026-08-20T06:34:51Z"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2: %+v", len(checks), checks)
	}
	byName := map[string]scm.Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["build"].Bucket != scm.CheckBucketFail {
		t.Fatalf("build bucket = %q, want fail", byName["build"].Bucket)
	}
	if byName["build2"].Bucket != scm.CheckBucketPass {
		t.Fatalf("build2 bucket = %q, want pass", byName["build2"].Bucket)
	}
	wantCompletedAt := time.Date(2026, 8, 20, 6, 34, 50, 0, time.UTC)
	if !byName["build"].CompletedAt.Equal(wantCompletedAt) {
		t.Fatalf("build CompletedAt = %v, want %v", byName["build"].CompletedAt, wantCompletedAt)
	}
	if byName["build"].Link != "http://gitea.example.com/owner/repo/actions/runs/10/jobs/10" {
		t.Fatalf("build Link = %q, want job html_url", byName["build"].Link)
	}
}

// TestGetChecksIgnoresAnotherBranchsRunThatSharesTheHeadSHA guards against
// cross-branch/cross-PR misattribution: a higher-ID push-triggered run on an
// unrelated branch must never be treated as a match candidate for this PR,
// even though runJobsMatchingHeadSHA would otherwise try it first (highest
// ID first). The unrelated run's jobs endpoint is deliberately left
// unstubbed, so if the branch pre-filter in runJobsMatchingHeadSHA failed to
// drop it, this test would fail on the resulting "unexpected command" error
// instead of just picking the wrong result.
func TestGetChecksIgnoresAnotherBranchsRunThatSharesTheHeadSHA(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","head_branch":"other-branch","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"success","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1: %+v", len(checks), checks)
	}
	if checks[0].Name != "build" {
		t.Fatalf("checks[0].Name = %q, want build (run 10, this PR's branch)", checks[0].Name)
	}
}

// TestGetChecksWithMissingHeadSHANeverMatchesAnotherPRsRun guards the one
// case highest-ID-first search cannot disambiguate by commit at all: the
// PR's own head SHA is unavailable (view.HeadSHA empty), so
// runJobsMatchingHeadSHA falls back to the newest run in its already-scoped
// candidate list. Before matching scoped on pull_requests[].number, that
// candidate list was every pull_request-triggered run in the repo (branch
// name cannot screen them - see listRuns), so a higher-ID run belonging to a
// completely different PR (11) could be selected and reported as PR 7's own
// checks, including a green PR 11 did not earn for PR 7. PR 11's run is the
// higher id and is listed first, and its jobs endpoint is deliberately left
// unstubbed: if PR-number scoping ever regressed back to a bare highest-ID
// search, this test fails on an "unexpected command" error rather than
// silently reporting the wrong PR's result.
func TestGetChecksWithMissingHeadSHANeverMatchesAnotherPRsRun(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":""}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"25","status":"completed","event":"pull_request","pull_requests":[{"number":11}]},` +
				`{"id":"10","status":"completed","event":"pull_request","pull_requests":[{"number":7}]}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"success","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("GetChecks() = %+v, want PR 7's own run (id 10), never PR 11's (id 25)", checks)
	}
}

// TestGetChecksFindsPullRequestTriggeredRunByPRNumber reproduces a real
// Gitea instance's behavior for a pull_request-triggered Actions run: the
// run carries no usable branch name of its own (Gitea's run-list endpoints
// either report head_branch as the literal "#<PR number>" or omit it
// entirely - confirmed against a live Gitea 1.27.3 server), so the one
// authoritative signal Gitea provides for this case is the run's own
// pull_requests[].number. Before listRuns started reading that field, such a
// run was invisible to GetChecks no matter how long the PR's checks ran,
// which read as "no CI checks reported yet" forever rather than a passing or
// failing result.
func TestGetChecksFindsPullRequestTriggeredRunByPRNumber(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","event":"pull_request","pull_requests":[{"number":7}]}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"check","status":"completed","conclusion":"success","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1: %+v", len(checks), checks)
	}
	if checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("check bucket = %q, want pass", checks[0].Bucket)
	}
}

func TestGetChecksReturnsNilWhenLatestRunPredatesTheCurrentHeadSHA(t *testing.T) {
	t.Parallel()

	// The branch-filtered run's jobs belong to an older commit than the PR's
	// current head: CI has not caught up with the latest push yet, so this
	// must not be reported as the (possibly all-green) old commit's checks.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"newsha"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"success","head_sha":"oldsha"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if checks != nil {
		t.Fatalf("GetChecks() = %+v, want nil (stale run)", checks)
	}
}

func TestGetChecksReturnsNilWhenNoRunsExistYet(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if checks != nil {
		t.Fatalf("GetChecks() = %+v, want nil", checks)
	}
}

func TestGetChecksSelectsHighestIDRunWhenListOrderIsNotNewestFirst(t *testing.T) {
	t.Parallel()

	// Both runs share the PR's current head_sha (e.g. a manual UI re-run), and
	// the list response deliberately puts the higher-ID (newer) run second, so
	// only explicit ID selection - not array order - can find it.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"success","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("GetChecks() = %+v, want the newer run's passing check", checks)
	}
}

func TestGetChecksSearchesPastAHigherIDRunForAnotherCommit(t *testing.T) {
	t.Parallel()

	// Run 25 has a higher ID than run 10 but belongs to a different commit
	// (e.g. a manual re-run of an older push landed after the PR's real head
	// commit triggered run 10). Selecting the highest-ID run alone would find
	// no match for the PR's current head SHA and wrongly report no checks;
	// the matching lower-ID run must still be found.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"success","head_sha":"zzz999"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("GetChecks() = %+v, want run 10's failing check (the run matching the PR head SHA)", checks)
	}
}

func TestFetchFailedCheckLogsStripsHeaderAndTargetsFailedJob(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea actions runs logs 10 --job 10 --repo owner/repo --login work": {
			stdout: "Logs for job 10:\n---\nexit status 1\nJob 'build' failed\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "7"}, "", "", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "exit status 1\nJob 'build' failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want stripped log body", logs)
	}
}

func TestFetchFailedCheckTargetLogsAggregatesEverySelectedJob(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"},{"id":11,"name":"lint","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea actions runs logs 10 --job 10 --repo owner/repo --login work": {stdout: "Logs for job 10:\n---\nbuild failed\n"},
		"tea actions runs logs 10 --job 11 --repo owner/repo --login work": {stdout: "Logs for job 11:\n---\nlint failed\n"},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "7"}, "", "abc123", []scm.CheckTarget{{Name: "build", ProviderID: "gitea-job:10"}, {Name: "lint", ProviderID: "gitea-job:11"}})
	if err != nil {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v", err)
	}
	if len(logs) != 2 || logs[0].Output != "build failed" || logs[1].Output != "lint failed" {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, want target-separated logs", logs)
	}
}

func TestFetchFailedCheckTargetLogsReturnsPartialLogsWithRetrievalError(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json":         {stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50":     {stdout: `{"workflow_runs":[{"id":"10","head_branch":"feature/x","event":"push"}]}`},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs":      {stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"},{"id":11,"name":"lint","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`},
		"tea actions runs logs 10 --job 10 --repo owner/repo --login work": {stdout: "Logs for job 10:\n---\nbuild failed\n"},
		"tea actions runs logs 10 --job 11 --repo owner/repo --login work": {stderr: "expired", code: 1},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "7"}, "", "abc123", []scm.CheckTarget{{ProviderID: "gitea-job:10"}, {ProviderID: "gitea-job:11"}})
	if err != nil || len(logs) != 2 || logs[0].Output != "build failed" || logs[1].Err == nil || !strings.Contains(logs[1].Err.Error(), "job 11") {
		t.Fatalf("FetchFailedCheckTargetLogs() = (%+v, %v), want retained partial logs and job 11 error", logs, err)
	}
}

func TestFetchFailedCheckTargetLogsReportsMissingSelectedJob(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json":     {stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {stdout: `{"workflow_runs":[{"id":"10","head_branch":"feature/x","event":"push"}]}`},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs":  {stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "7"}, "", "abc123", []scm.CheckTarget{{ProviderID: "gitea-job:999"}})
	if err != nil || len(logs) != 1 || logs[0].Err == nil || !strings.Contains(logs[0].Err.Error(), "gitea-job:999") {
		t.Fatalf("FetchFailedCheckTargetLogs() = (%+v, %v), want explicit missing-target error", logs, err)
	}
}

func TestFetchFailedCheckLogsSelectsHighestIDRunWhenListOrderIsNotNewestFirst(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea actions runs logs 25 --job 25 --repo owner/repo --login work": {
			stdout: "Logs for job 25:\n---\nexit status 1\nJob 'build' failed\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "7"}, "", "", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "exit status 1\nJob 'build' failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want stripped log body from the higher-ID run", logs)
	}
}

func TestFetchFailedCheckLogsSearchesPastAHigherIDRunForAnotherCommit(t *testing.T) {
	t.Parallel()

	// Same trap as TestGetChecksSearchesPastAHigherIDRunForAnotherCommit: run
	// 25 (higher ID) belongs to a different commit than the run's head SHA,
	// so logs must come from run 10, the run that actually matches - never
	// another commit's same-named job logs sent to the auto-fix agent.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":"10","status":"completed","head_branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","head_branch":"feature/x","event":"push"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"failure","head_sha":"zzz999"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea actions runs logs 10 --job 10 --repo owner/repo --login work": {
			stdout: "Logs for job 10:\n---\nexit status 1\nJob 'build' failed\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "7"}, "", "abc123", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "exit status 1\nJob 'build' failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want stripped log body from run 10 (the run matching the head SHA)", logs)
	}
}

func TestFetchFailedCheckLogsReturnsEmptyForNoFailingNames(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(nil), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")
	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "7"}, "", "", nil)
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want empty", logs)
	}
}

// The tea 1.0 tests below mirror the tea 0.x ones above with tea 1.0's argv
// and JSON shapes (gitea/tea#1126): --host instead of --login, --json=<fields>
// instead of --output json/--fields, `pr`/`run` instead of `pulls`/`actions
// runs`, integer `number`/`id`, and `head_sha`/`merged` on the PR.

const tea1Instance = "https://gitea.example.com"

func tea1Host(responses map[string]giteaTestResponse) *Host {
	responses[tea1Probe] = giteaTestResponse{}
	return New(giteaTestCmdFactory(responses), nil, "gitea.example.com", scm.TeaAccount{Login: "work", Instance: tea1Instance}, "owner/repo")
}

func TestGenerationProbeRefusesAnUnrecognizedTea(t *testing.T) {
	t.Parallel()

	// A binary that passes neither generation's probe must fail closed with
	// a clear error before any PR call, never be guessed at.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		tea0Probe:                    {stderr: "unknown flag: --output", code: 2},
		"tea api --login work /user": {stdout: `{"login":"someuser"}`},
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {stdout: `[]`},
	}), func() bool { return true }, "gitea.example.com", scm.TeaAccount{Login: "work", Instance: tea1Instance}, "owner/repo")

	for name, call := range map[string]func() error{
		"Available": func() error { return host.Available(context.Background()) },
		"FindPR":    func() error { _, err := host.FindPR(context.Background(), "feature/x", "main"); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "unrecognized tea CLI") {
			t.Fatalf("%s() error = %v, want unrecognized tea CLI", name, err)
		}
	}
}

func TestGenerationProbeRunsOnce(t *testing.T) {
	t.Parallel()

	probes := 0
	factory := giteaTestCmdFactory(map[string]giteaTestResponse{
		tea1Probe: {},
		"tea api --host " + tea1Instance + " /user": {stdout: `{"login":"someuser"}`},
	})
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if strings.Contains(strings.Join(args, " "), "--help") {
			probes++
		}
		return factory(ctx, name, args...)
	}, nil, "gitea.example.com", scm.TeaAccount{Instance: tea1Instance}, "owner/repo")

	for i := 0; i < 3; i++ {
		if err := host.Available(context.Background()); err != nil {
			t.Fatalf("Available() error = %v", err)
		}
	}
	if probes != 1 {
		t.Fatalf("probe ran %d times, want 1", probes)
	}
}

// countProbes wraps factory and counts the generation probes (the two
// `--help` argvs) it is asked to build.
func countProbes(factory CmdFactory, probes *atomic.Int32) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if key := name + " " + strings.Join(args, " "); key == tea0Probe || key == tea1Probe {
			probes.Add(1)
		}
		return factory(ctx, name, args...)
	}
}

func TestGenerationProbeRunsOnceAcrossConcurrentCalls(t *testing.T) {
	t.Parallel()

	// Every PR/CI operation on a shared Host resolves the generation first,
	// so concurrent first calls must share one probe, not race to run their
	// own (or race on the cached answer, which -race reports).
	var probes atomic.Int32
	host := New(countProbes(giteaTestCmdFactory(map[string]giteaTestResponse{
		tea1Probe: {},
		"tea api --host " + tea1Instance + " /user": {stdout: `{"login":"someuser"}`},
	}), &probes), nil, "gitea.example.com", scm.TeaAccount{Instance: tea1Instance}, "owner/repo")

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- host.Available(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Available() error = %v", err)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("probe ran %d times across %d concurrent callers, want 1", got, callers)
	}
}

func TestGenerationProbeCancelledIsNotCached(t *testing.T) {
	t.Parallel()

	// A probe cut short by its caller's cancellation says nothing about the
	// binary: it must surface the context error (not "unrecognized tea CLI",
	// and not fall through to the tea 0.x probe), and the next call with a
	// live context must probe again rather than reuse that failed attempt.
	var probes atomic.Int32
	factory := countProbes(giteaTestCmdFactory(map[string]giteaTestResponse{
		tea1Probe: {},
		"tea api --host " + tea1Instance + " /user": {stdout: `{"login":"someuser"}`},
	}), &probes)
	ctx, cancel := context.WithCancel(context.Background())
	host := New(func(c context.Context, name string, args ...string) *exec.Cmd {
		cmd := factory(c, name, args...)
		if c == ctx {
			cancel() // interrupt the probe the cancelled call is running
		}
		return cmd
	}, nil, "gitea.example.com", scm.TeaAccount{Instance: tea1Instance}, "owner/repo")

	if err := host.Available(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Available(cancelled) error = %v, want context.Canceled", err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("cancelled call ran %d probes, want 1 (no tea 0.x probe after cancellation)", got)
	}
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() after cancellation error = %v, want nil", err)
	}
	if got := probes.Load(); got != 2 {
		t.Fatalf("probe ran %d times in total, want 2 (re-probed after cancellation)", got)
	}
}

func TestGenerationProbeUnrecognizedIsNotCached(t *testing.T) {
	t.Parallel()

	// "unrecognized tea CLI" must not stick: once the binary answers a probe
	// (e.g. the operator installed a real tea), the next call re-probes and
	// proceeds instead of refusing for the rest of the Host's life.
	var probes atomic.Int32
	var answering atomic.Bool
	unrecognized := giteaTestCmdFactory(map[string]giteaTestResponse{
		tea0Probe: {stderr: "unknown flag: --output", code: 2},
	})
	tea1 := giteaTestCmdFactory(map[string]giteaTestResponse{
		tea1Probe: {},
		"tea api --host " + tea1Instance + " /user": {stdout: `{"login":"someuser"}`},
	})
	host := New(countProbes(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if answering.Load() {
			return tea1(ctx, name, args...)
		}
		return unrecognized(ctx, name, args...)
	}, &probes), nil, "gitea.example.com", scm.TeaAccount{Instance: tea1Instance}, "owner/repo")

	if err := host.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "unrecognized tea CLI") {
		t.Fatalf("Available() error = %v, want unrecognized tea CLI", err)
	}
	if got := probes.Load(); got != 2 {
		t.Fatalf("refused call ran %d probes, want both (2)", got)
	}
	answering.Store(true)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() once tea answers error = %v, want nil", err)
	}
	if got := probes.Load(); got != 3 {
		t.Fatalf("probe ran %d times in total, want 3 (re-probed after the refusal)", got)
	}
}

func TestTea1AvailableScopesToConfiguredInstance(t *testing.T) {
	t.Parallel()

	host := tea1Host(map[string]giteaTestResponse{
		"tea api --host " + tea1Instance + " /user": {stdout: `{"login":"someuser"}`},
	})
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestTea1AvailableRefusesAMissingInstance(t *testing.T) {
	t.Parallel()

	// tea 1.0 does not read tea 0.x logins, and an empty --host would make
	// it fall back to another instance, so a tea 0.x login alone must not
	// satisfy a tea 1.0 binary.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		tea1Probe:                    {},
		"tea api --host  /user":      {stdout: `{"login":"someone-else"}`},
		"tea api --login work /user": {stdout: `{"login":"someuser"}`},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")

	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tea auth login --host https://gitea.example.com") {
		t.Fatalf("Available() error = %v, want a tea 1.0 setup hint", err)
	}
}

func TestTea1PRLifecycle(t *testing.T) {
	t.Parallel()

	host := tea1Host(map[string]giteaTestResponse{
		"tea pr list --repo owner/repo --host " + tea1Instance + " --json=number,state,url,head,base": {
			stdout: `[{"number":3,"state":"open","url":"https://gitea.example.com/owner/repo/pulls/3","head":"other","base":"main"},` +
				`{"number":9,"state":"open","url":"https://gitea.example.com/owner/repo/pulls/9","head":"feature/x","base":"main"}]`,
		},
		"tea pr create --repo owner/repo --host " + tea1Instance + " --head feature/x --base main --title t --body See http://example.com/x": {
			stdout: "https://gitea.example.com/owner/repo/pulls/9\n",
		},
		"tea pr edit 9 --repo owner/repo --host " + tea1Instance + " --title t2 --body b2": {},
		// --raw-field, not tea 1.0's typed --field: a branch named 123 stays a string.
		"tea api --host " + tea1Instance + " --method PATCH --raw-field base=123 /repos/owner/repo/pulls/9": {stdout: `{}`},
		"tea pr view 9 --repo owner/repo --host " + tea1Instance + " --json=number,state,url,head,base,head_sha,merged": {
			stdout: `{"number":9,"state":"closed","url":"https://gitea.example.com/owner/repo/pulls/9","head":"feature/x","base":"main","head_sha":"abc123","merged":true}`,
		},
	})
	ctx := context.Background()

	pr, err := host.CreatePR(ctx, "feature/x", "main", scm.PRContent{Title: "t", Body: "See http://example.com/x"})
	if err != nil || pr == nil || pr.Number != "9" || pr.URL != "https://gitea.example.com/owner/repo/pulls/9" || pr.BaseBranch != "main" {
		t.Fatalf("CreatePR() = (%+v, %v), want PR #9 from the re-list", pr, err)
	}
	if _, err := host.UpdatePR(ctx, pr, scm.PRContent{Title: "t2", Body: "b2"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if err := host.SetPRBaseBranch(ctx, pr, "123"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
	if state, err := host.GetPRState(ctx, pr); err != nil || state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = (%q, %v), want MERGED", state, err)
	}
}

func TestTea1FindPRReturnsErrorOnMalformedJSON(t *testing.T) {
	t.Parallel()

	// tea 0.x's string index must not parse as tea 1.0's integer number.
	host := tea1Host(map[string]giteaTestResponse{
		"tea pr list --repo owner/repo --host " + tea1Instance + " --json=number,state,url,head,base": {
			stdout: `[{"number":"9","head":"feature/x","base":"main"}]`,
		},
	})
	if pr, err := host.FindPR(context.Background(), "feature/x", "main"); err == nil || !strings.Contains(err.Error(), "tea pr list") {
		t.Fatalf("FindPR() = (%+v, %v), want a tea pr list JSON error", pr, err)
	}
}

func TestTea1ChecksAndLogsUseTheHighestMatchingRun(t *testing.T) {
	t.Parallel()

	host := tea1Host(map[string]giteaTestResponse{
		"tea pr view 7 --repo owner/repo --host " + tea1Instance + " --json=number,state,url,head,base,head_sha,merged": {
			stdout: `{"number":7,"state":"open","head":"feature/x","head_sha":"abc123","merged":false}`,
		},
		"tea api --host " + tea1Instance + " /repos/owner/repo/actions/runs?limit=50": {
			stdout: `{"workflow_runs":[{"id":10,"event":"push","head_branch":"feature/x"},{"id":25,"event":"push","head_branch":"feature/x"},{"id":9,"event":"push","head_branch":"feature/x"}]}`,
		},
		"tea api --host " + tea1Instance + " /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":250,"name":"build","status":"completed","conclusion":"success","head_sha":"zzz999"}]}`,
		},
		"tea api --host " + tea1Instance + " /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":100,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		// tea 1.0 sends its "Job N" banner to stderr; stdout is the log, and
		// a `---` line inside it is log content, not a banner to strip.
		"tea run logs 10 --job 100 --repo owner/repo --host " + tea1Instance: {
			stdout: "step 1\n---\nexit status 1\n",
			stderr: "Job 100: build\n",
		},
	})
	ctx := context.Background()

	checks, err := host.GetChecks(ctx, &scm.PR{Number: "7"})
	if err != nil || len(checks) != 1 || checks[0].Bucket != scm.CheckBucketFail || checks[0].ProviderID != "gitea-job:100" {
		t.Fatalf("GetChecks() = (%+v, %v), want run 10's failing job", checks, err)
	}
	logs, err := host.FetchFailedCheckLogs(ctx, &scm.PR{Number: "7"}, "", "", []string{"build"})
	if err != nil || logs != "step 1\n---\nexit status 1" {
		t.Fatalf("FetchFailedCheckLogs() = (%q, %v), want the whole log", logs, err)
	}
}

type giteaTestResponse struct {
	stdout string
	stderr string
	code   int
}

const (
	tea1Probe = "tea pr list --json=number --help"
	tea0Probe = "tea pulls list --output json --help"
)

// giteaTestCmdFactory answers each command from responses and fails any other
// with exit 1. tea 0.x's generation probe succeeds unless responses overrides
// it, so a test that names no generation exercises tea 0.x; a tea 1.0 test
// answers tea1Probe, which is tried first.
func giteaTestCmdFactory(responses map[string]giteaTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok && key != tea0Probe {
			response = giteaTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGiteaHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"GITEA_TEST_HELPER=1",
			"GITEA_TEST_STDOUT="+response.stdout,
			"GITEA_TEST_STDERR="+response.stderr,
			fmt.Sprintf("GITEA_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestGiteaHelperProcess(t *testing.T) {
	if os.Getenv("GITEA_TEST_HELPER") != "1" {
		return
	}

	if _, err := fmt.Fprint(os.Stdout, os.Getenv("GITEA_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("GITEA_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("GITEA_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
