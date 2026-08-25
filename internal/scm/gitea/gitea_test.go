package gitea

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
	host := New(nil, nil, "", "", "")
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

	host := New(nil, nil, "", "", "")
	if _, err := host.GetMergeableState(context.Background(), &scm.PR{Number: "1"}); err != scm.ErrUnsupported {
		t.Fatalf("GetMergeableState() error = %v, want scm.ErrUnsupported", err)
	}
}

func TestAvailableFailsClosedWithoutConfiguredLogin(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(nil), func() bool { return true }, "gitea.example.com", "", "owner/repo")
	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error when no tea login is configured for the host")
	}
}

func TestAvailableScopesToConfiguredLogin(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work /user": {stdout: `{"login":"someuser"}`},
	}), func() bool { return true }, "gitea.example.com", "work", "owner/repo")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestAvailableReturnsErrorOnAuthFailure(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea api --login work /user": {stderr: "invalid username, password or token\n", code: 1},
	}), func() bool { return true }, "gitea.example.com", "work", "owner/repo")

	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error on auth failure")
	}
}

func TestAvailableReturnsErrorWhenCLIMissing(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(nil), func() bool { return false }, "gitea.example.com", "work", "owner/repo")
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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[{"index":"1","title":"a","state":"open","url":"https://gitea.example.com/owner/repo/pulls/1","head":"feature/x","base":"release/1.0"}]`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

	pr := &scm.PR{URL: "http://gitea.example.com/owner/repo/pulls/7"}
	if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "updated", Body: "body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
	}), nil, "gitea.example.com", "work", "owner/repo")

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
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"}]`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123","html_url":"http://gitea.example.com/owner/repo/actions/runs/10/jobs/10","completed_at":"2026-08-20T06:34:50Z"},` +
				`{"id":11,"name":"build2","status":"completed","conclusion":"success","head_sha":"abc123","html_url":"http://gitea.example.com/owner/repo/actions/runs/10/jobs/11","completed_at":"2026-08-20T06:34:51Z"}]}`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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

func TestGetChecksReturnsNilWhenLatestRunPredatesTheCurrentHeadSHA(t *testing.T) {
	t.Parallel()

	// The branch-filtered run's jobs belong to an older commit than the PR's
	// current head: CI has not caught up with the latest push yet, so this
	// must not be reported as the (possibly all-green) old commit's checks.
	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"newsha"}`,
		},
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"}]`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"success","head_sha":"oldsha"}]}`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[]`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","branch":"feature/x","event":"push"}]`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"success","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","branch":"feature/x","event":"push"}]`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"success","head_sha":"zzz999"}]}`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"}]`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/10/jobs": {
			stdout: `{"jobs":[{"id":10,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea actions runs logs 10 --job 10 --repo owner/repo --login work": {
			stdout: "Logs for job 10:\n---\nexit status 1\nJob 'build' failed\n",
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "7"}, "", "", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "exit status 1\nJob 'build' failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want stripped log body", logs)
	}
}

func TestFetchFailedCheckLogsSelectsHighestIDRunWhenListOrderIsNotNewestFirst(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls 7 --repo owner/repo --login work --output json": {
			stdout: `{"index":7,"state":"open","head":"feature/x","headSha":"abc123"}`,
		},
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","branch":"feature/x","event":"push"}]`,
		},
		"tea api --login work /repos/owner/repo/actions/runs/25/jobs": {
			stdout: `{"jobs":[{"id":25,"name":"build","status":"completed","conclusion":"failure","head_sha":"abc123"}]}`,
		},
		"tea actions runs logs 25 --job 25 --repo owner/repo --login work": {
			stdout: "Logs for job 25:\n---\nexit status 1\nJob 'build' failed\n",
		},
	}), nil, "gitea.example.com", "work", "owner/repo")

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
		"tea actions runs list --repo owner/repo --login work --branch feature/x --output json": {
			stdout: `[{"id":"10","status":"completed","branch":"feature/x","event":"push"},` +
				`{"id":"25","status":"completed","branch":"feature/x","event":"push"}]`,
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
	}), nil, "gitea.example.com", "work", "owner/repo")

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

	host := New(giteaTestCmdFactory(nil), nil, "gitea.example.com", "work", "owner/repo")
	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "7"}, "", "", nil)
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want empty", logs)
	}
}

type giteaTestResponse struct {
	stdout string
	stderr string
	code   int
}

func giteaTestCmdFactory(responses map[string]giteaTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
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
