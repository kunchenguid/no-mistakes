package origin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRepoSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"https://origin.cursor.com/owner/repo.git", "owner/repo"},
		{"git@origin.cursor.com:owner/repo.git", "owner/repo"},
		{"https://cursor.com/codebase/owner/repo/pull/6", "owner/repo"},
		{"https://cursor.com/codebase/owner/repo", "owner/repo"},
		{"https://example.com/owner/repo.git", "owner/repo"},
		{"", ""},
		{"https://cursor.com/changelog", ""},
	}
	for _, tc := range cases {
		if got := RepoSlug(tc.in); got != tc.want {
			t.Errorf("RepoSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseOriginPRURL(t *testing.T) {
	t.Parallel()

	num, err := parseOriginPRURL("https://cursor.com/codebase/owner/repo/pull/6")
	if err != nil || num != "6" {
		t.Fatalf("parseOriginPRURL(web) = (%q, %v), want 6", num, err)
	}
	if _, err := parseOriginPRURL("https://cursor.com/docs"); err == nil {
		t.Fatal("parseOriginPRURL(docs) = nil error, want error")
	}
	if _, err := parseOriginPRURL("https://cursor.com/codebase/owner/repo/pull/6?x=1"); err == nil {
		t.Fatal("parseOriginPRURL(query) = nil error, want error")
	}
}

func TestOriginCheckBucket(t *testing.T) {
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
		{"completed", "neutral", scm.CheckBucketSkip},
		{"completed", "unknown", ""},
		{"queued", "", scm.CheckBucketPending},
		{"in_progress", "", scm.CheckBucketPending},
		{"", "", scm.CheckBucketPending},
	}
	for _, tc := range cases {
		if got := originCheckBucket(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("originCheckBucket(%q, %q) = %q, want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
}

func TestNormalizeOriginPRState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want scm.PRState
	}{
		{"open", scm.PRStateOpen},
		{"draft", scm.PRStateOpen},
		{"merged", scm.PRStateMerged},
		{"closed", scm.PRStateClosed},
	}
	for _, tc := range cases {
		if got := normalizeOriginPRState(tc.raw); got != tc.want {
			t.Errorf("normalizeOriginPRState(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	caps := New(nil, nil, "", false).Capabilities()
	if !caps.MergeableState {
		t.Fatal("Capabilities().MergeableState = false, want true")
	}
	if caps.FailedCheckLogs {
		t.Fatal("Capabilities().FailedCheckLogs = true, want false")
	}
}

func TestFetchFailedCheckLogsUnsupported(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, nil, "", false).FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "1"}, "", "", nil); err != scm.ErrUnsupported {
		t.Fatalf("FetchFailedCheckLogs() error = %v, want scm.ErrUnsupported", err)
	}
}

func TestAvailableFailsWhenCLIMissing(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(nil), func() bool { return false }, "owner/repo", false)
	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error when origin CLI is missing")
	}
}

func TestAvailableReturnsErrorOnAuthFailure(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin auth status": {stderr: "not logged in\n", code: 1},
	}), func() bool { return true }, "owner/repo", false)
	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want error on auth failure")
	}
}

func TestAvailableOK(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin auth status": {stdout: "Token: valid\n"},
	}), func() bool { return true }, "owner/repo", false)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
}

func listKey(head string) string { return listKeyState(head, "open") }

func listKeyState(head, state string) string {
	return "origin pr list --repo owner/repo --head " + head + " --state " + state + " --json " + listJSONFields + " --limit 100"
}

func TestFindPRMatchesOpenAndDraft(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {
			stdout: `[{"number":3,"url":"https://cursor.com/codebase/owner/repo/pull/3","status":"closed","headRef":"feature/x","baseRef":"master"},` +
				`{"number":7,"url":"https://cursor.com/codebase/owner/repo/pull/7","status":"draft","headRef":"feature/x","baseRef":"master"}]`,
		},
	}), nil, "owner/repo", false)

	pr, err := host.FindPR(context.Background(), "feature/x", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "7" {
		t.Fatalf("FindPR() = %+v, want draft PR #7", pr)
	}
	if pr.BaseBranch != "master" {
		t.Fatalf("FindPR() BaseBranch = %q, want master", pr.BaseBranch)
	}
}

func TestFindPRFiltersByBaseWhenRequested(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {
			stdout: `[{"number":1,"url":"https://cursor.com/codebase/owner/repo/pull/1","status":"open","headRef":"feature/x","baseRef":"release"}]`,
		},
	}), nil, "owner/repo", false)

	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil (base mismatch)", pr)
	}
}

func TestFindPRReturnsNilWhenNoneLive(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {stdout: `[]`},
	}), nil, "owner/repo", false)

	pr, err := host.FindPR(context.Background(), "feature/x", "")
	if err != nil || pr != nil {
		t.Fatalf("FindPR() = (%+v, %v), want nil", pr, err)
	}
}

func TestFindPRErrorsOnMalformedJSON(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {stdout: `[{"number":7`},
	}), nil, "owner/repo", false)

	pr, err := host.FindPR(context.Background(), "feature/x", "")
	if err == nil || pr != nil {
		t.Fatalf("FindPR() = (%+v, %v), want JSON error", pr, err)
	}
}

func TestFindPRErrorsOnNonJSONOutput(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {stdout: "a new origin release is available\n"},
	}), nil, "owner/repo", false)

	pr, err := host.FindPR(context.Background(), "feature/x", "")
	if err == nil || pr != nil {
		t.Fatalf("FindPR() = (%+v, %v), want non-JSON error", pr, err)
	}
}

func TestCreatePRPassesStatusOpenAndRelists(t *testing.T) {
	t.Parallel()

	var saw []string
	factory := originTestCmdFactory(map[string]originTestResponse{
		"origin pr create --repo owner/repo --head feature/x --base master --status open --title feat: x --body-file -": {
			stdout: "https://cursor.com/codebase/owner/repo/pull/9\n",
		},
		listKey("feature/x"): {
			stdout: `[{"number":9,"url":"https://cursor.com/codebase/owner/repo/pull/9","status":"open","headRef":"feature/x","baseRef":"master"}]`,
		},
	})
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		saw = append(saw, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		return factory(ctx, name, args...)
	}, nil, "owner/repo", false)

	pr, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "feat: x", Body: "body"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "9" {
		t.Fatalf("CreatePR() = %+v, want PR #9", pr)
	}
	if len(saw) == 0 || !strings.Contains(saw[0], "--status open") {
		t.Fatalf("CreatePR argv = %v, want --status open on create", saw)
	}
	if strings.Contains(saw[0], "--status draft") {
		t.Fatalf("CreatePR argv = %v, did not want draft", saw)
	}
}

func TestCreatePRDraftStatus(t *testing.T) {
	t.Parallel()

	factory := originTestCmdFactory(map[string]originTestResponse{
		"origin pr create --repo owner/repo --head feature/x --base master --status draft --title feat: x --body-file -": {
			stdout: "created\n",
		},
		listKey("feature/x"): {
			stdout: `[{"number":4,"url":"https://cursor.com/codebase/owner/repo/pull/4","status":"draft","headRef":"feature/x","baseRef":"master"}]`,
		},
	})
	host := New(factory, nil, "owner/repo", true)
	pr, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "feat: x", Body: "body"})
	if err != nil || pr == nil || pr.Number != "4" {
		t.Fatalf("CreatePR() = (%+v, %v), want draft PR #4", pr, err)
	}
}

func TestCreatePRFallsBackToStdoutURL(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr create --repo owner/repo --head feature/x --base master --status open --title feat: x --body-file -": {
			stdout: "Opening change\nhttps://cursor.com/codebase/owner/repo/pull/11\n",
		},
		listKey("feature/x"): {stdout: `[]`},
	}), nil, "owner/repo", false)

	pr, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "feat: x", Body: "body"})
	if err != nil || pr == nil || pr.Number != "11" {
		t.Fatalf("CreatePR() = (%+v, %v), want URL-fallback PR #11", pr, err)
	}
}

func TestGetPRStateAndMergeable(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr view 6 --repo owner/repo --json " + viewJSONFields: {
			stdout: `{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open","title":"t","description":"d","headRef":"feat","baseRef":"master","headSha":"abc","mergeability":{"mergeable":true,"hasMergeConflicts":false}}`,
		},
	}), nil, "owner/repo", false)

	pr := &scm.PR{Number: "6"}
	state, err := host.GetPRState(context.Background(), pr)
	if err != nil || state != scm.PRStateOpen {
		t.Fatalf("GetPRState() = (%q, %v), want OPEN", state, err)
	}
	merge, err := host.GetMergeableState(context.Background(), pr)
	if err != nil || merge != scm.MergeableOK {
		t.Fatalf("GetMergeableState() = (%q, %v), want MERGEABLE", merge, err)
	}
	base, err := host.GetPRBaseBranch(context.Background(), pr)
	if err != nil || base != "master" {
		t.Fatalf("GetPRBaseBranch() = (%q, %v), want master", base, err)
	}
	content, err := host.GetPRContent(context.Background(), pr)
	if err != nil || content.Title != "t" || content.Body != "d" {
		t.Fatalf("GetPRContent() = (%+v, %v)", content, err)
	}
}

// TestGetPRStateMergedWithObjectMergedBy pins the shape `origin pr view`
// returns for a merged change, where mergedBy is an object rather than a
// login string. The view must still decode so the CI step sees MERGED.
func TestGetPRStateMergedWithObjectMergedBy(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr view 14 --repo owner/repo --json " + viewJSONFields: {
			stdout: `{"number":14,"url":"https://cursor.com/codebase/owner/repo/pull/14","status":"merged","title":"t","description":"d","headRef":"feat","baseRef":"master","headSha":"abc","mergeCommitSha":"def","mergedAt":"2026-09-24T20:56:12Z","mergedBy":{"id":"user_1"},"mergeability":{"mergeable":false,"hasMergeConflicts":false,"mergeability":{"verdict":"blocked","evaluations":[],"blockers":[{"kind":"change-already-merged"}]}}}`,
		},
	}), nil, "owner/repo", false)

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "14"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v, want nil (merged PR JSON must still parse)", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want MERGED", state)
	}
}

func TestGetPRContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open","title":"t"}`,
		`{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open","title":"t","description":null}`,
		`{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open","title":"","description":"d"}`,
		`{"number":7,"url":"https://cursor.com/codebase/owner/repo/pull/7","status":"open","title":"t","description":"d"}`,
	} {
		host := New(originTestCmdFactory(map[string]originTestResponse{
			"origin pr view 6 --repo owner/repo --json " + viewJSONFields: {stdout: raw},
		}), nil, "owner/repo", false)
		if _, err := host.GetPRContent(context.Background(), &scm.PR{Number: "6"}); err == nil {
			t.Errorf("GetPRContent() accepted %s", raw)
		}
	}

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr view 6 --repo owner/repo --json " + viewJSONFields: {
			stdout: `{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open","title":"t","description":""}`,
		},
	}), nil, "owner/repo", false)
	content, err := host.GetPRContent(context.Background(), &scm.PR{Number: "6"})
	if err != nil || content.Body != "" || content.Title != "t" {
		t.Fatalf("GetPRContent(empty body) = (%+v, %v), want explicit empty body", content, err)
	}
}

// Every Origin mergeability shape is a known state: only an explicit conflict
// signal is CONFLICTING, and anything else must not hold the CI monitor pending.
func TestGetMergeableStateShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mergeability string
		want         scm.MergeableState
	}{
		{`{"mergeable":true,"hasMergeConflicts":false}`, scm.MergeableOK},
		{`{"mergeable":false,"hasMergeConflicts":true}`, scm.MergeableConflict},
		{`{"mergeable":false,"hasMergeConflicts":false}`, scm.MergeableOK},
		{`{"mergeable":false}`, scm.MergeableOK},
		{`{"mergeable":true}`, scm.MergeableOK},
		// Live shape: the verdict is nested under mergeability.mergeability.
		{`{"mergeable":false,"hasMergeConflicts":false,"conflictedPaths":[],"mergeability":{"verdict":"blocked","evaluations":[],"blockers":[{"kind":"change-is-draft"}]}}`, scm.MergeableOK},
		{`{"mergeable":true,"hasMergeConflicts":false,"conflictedPaths":[],"mergeability":{"verdict":"mergeable","evaluations":[],"blockers":[]}}`, scm.MergeableOK},
		{`{"mergeable":false,"hasMergeConflicts":true,"conflictedPaths":["a.go"],"mergeability":{"verdict":"blocked","blockers":[]}}`, scm.MergeableConflict},
		// hasMergeConflicts absent: fall back to mergeable, conflictedPaths and the nested verdict.
		{`{"mergeable":false,"mergeability":{"verdict":"conflicts"}}`, scm.MergeableConflict},
		{`{"mergeable":false,"conflictedPaths":["a.go"],"mergeability":{"verdict":"blocked"}}`, scm.MergeableConflict},
		{`{"mergeable":false,"mergeability":{"verdict":"blocked"}}`, scm.MergeableOK},
		{`{"mergeable":true,"mergeability":{"verdict":"mergeable"}}`, scm.MergeableOK},
		{`{}`, scm.MergeableUnknown},
	} {
		host := New(originTestCmdFactory(map[string]originTestResponse{
			"origin pr view 6 --repo owner/repo --json " + viewJSONFields: {
				stdout: `{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open","mergeability":` + tc.mergeability + `}`,
			},
		}), nil, "owner/repo", false)

		got, err := host.GetMergeableState(context.Background(), &scm.PR{Number: "6"})
		if err != nil || got != tc.want {
			t.Errorf("GetMergeableState(%s) = (%q, %v), want %q", tc.mergeability, got, err, tc.want)
		}
	}
}

func TestGetMergeableStateOmittedFieldIsUnknown(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr view 6 --repo owner/repo --json " + viewJSONFields: {
			stdout: `{"number":6,"url":"https://cursor.com/codebase/owner/repo/pull/6","status":"open"}`,
		},
	}), nil, "owner/repo", false)
	got, err := host.GetMergeableState(context.Background(), &scm.PR{Number: "6"})
	if err != nil || got != scm.MergeableUnknown {
		t.Fatalf("GetMergeableState() = (%q, %v), want UNKNOWN", got, err)
	}
}

func TestFindPRFindsDraftWhenOpenListIsEmpty(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKeyState("feature/x", "open"): {stdout: `[]`},
		listKeyState("feature/x", "draft"): {
			stdout: `[{"number":5,"url":"https://cursor.com/codebase/owner/repo/pull/5","status":"draft","headRef":"feature/x","baseRef":"master"}]`,
		},
	}), nil, "owner/repo", false)
	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err != nil || pr == nil || pr.Number != "5" {
		t.Fatalf("FindPR() = (%+v, %v), want draft PR #5 independent of closed history", pr, err)
	}
}

func TestFindPRPicksNewestLiveMatch(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		listKey("feature/x"): {
			stdout: `[{"number":3,"url":"https://cursor.com/codebase/owner/repo/pull/3","status":"open","headRef":"feature/x","baseRef":"master"},{"number":12,"url":"https://cursor.com/codebase/owner/repo/pull/12","status":"draft","headRef":"feature/x","baseRef":"master"}]`,
		},
	}), nil, "owner/repo", false)
	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err != nil || pr == nil || pr.Number != "12" {
		t.Fatalf("FindPR() = (%+v, %v), want newest PR #12", pr, err)
	}
}

func TestCreatePRPrefersPrintedURLOverList(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr create --repo owner/repo --head feature/x --base master --status open --title feat: x --body-file -": {
			stdout: "https://cursor.com/codebase/owner/repo/pull/9\n",
		},
		listKey("feature/x"): {
			stdout: `[{"number":2,"url":"https://cursor.com/codebase/owner/repo/pull/2","status":"open","headRef":"feature/x","baseRef":"master"}]`,
		},
	}), nil, "owner/repo", false)
	pr, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "feat: x", Body: "body"})
	if err != nil || pr == nil || pr.Number != "9" {
		t.Fatalf("CreatePR() = (%+v, %v), want created PR #9", pr, err)
	}
}

func TestGetChecksMapsNeutralToSkip(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr checks 6 --repo owner/repo --json " + checkJSONFields: {
			stdout: `[{"id":"cr_1","name":"Cursor Bugbot","status":"completed","conclusion":"neutral","detailsUrl":null,"startedAt":"2026-09-10T17:21:54Z","completedAt":"2026-09-10T17:21:56Z"}]`,
		},
	}), nil, "owner/repo", false)

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "6"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketSkip || checks[0].Name != "Cursor Bugbot" {
		t.Fatalf("GetChecks() = %+v, want one skipped Bugbot check", checks)
	}
}

func TestUpdatePRAndSetBase(t *testing.T) {
	t.Parallel()

	host := New(originTestCmdFactory(map[string]originTestResponse{
		"origin pr edit 6 --repo owner/repo --title new --body-file -": {stdout: "updated\n"},
		"origin pr edit 6 --repo owner/repo --base develop":            {stdout: "retargeted\n"},
	}), nil, "owner/repo", false)

	pr := &scm.PR{Number: "6"}
	if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "new", Body: "body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if err := host.SetPRBaseBranch(context.Background(), pr, "develop"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
}

func TestPrSelectorRefusesEmptyIdentity(t *testing.T) {
	t.Parallel()

	if _, err := prSelector(&scm.PR{}); err == nil {
		t.Fatal("prSelector(empty) = nil, want error")
	}
}

type originTestResponse struct {
	stdout string
	stderr string
	code   int
}

func originTestCmdFactory(responses map[string]originTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok && strings.Contains(key, " --state draft ") {
			// List fixtures are keyed by the open state; the draft listing
			// answers from the same fixture unless it has its own entry.
			response, ok = responses[strings.Replace(key, " --state draft ", " --state open ", 1)]
		}
		if !ok {
			response = originTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestOriginHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"ORIGIN_TEST_HELPER=1",
			"ORIGIN_TEST_STDOUT="+response.stdout,
			"ORIGIN_TEST_STDERR="+response.stderr,
			fmt.Sprintf("ORIGIN_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestOriginHelperProcess(t *testing.T) {
	if os.Getenv("ORIGIN_TEST_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("ORIGIN_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("ORIGIN_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("ORIGIN_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
