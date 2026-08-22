package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	fallbackRepo    = "test/repo"
	fallbackHead    = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	fallbackOther   = "0000000000000000000000000000000000000000"
	fallbackPRView  = "gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid"
	fallbackBaseCmd = "gh pr view 123 --repo test/repo --json baseRefName --jq .baseRefName"
)

func fallbackRunsCmd(headSHA string) string {
	return "gh api --method GET repos/test/repo/actions/runs -f head_sha=" + headSHA + " -f per_page=100 --paginate --slurp"
}

func fallbackJobsCmd(runID int64) string {
	return fmt.Sprintf("gh api --method GET repos/test/repo/actions/runs/%d/jobs -f filter=latest -f per_page=100 --paginate --slurp", runID)
}

func fallbackRequiredCmd(branch string) string {
	return "gh api --method GET repos/test/repo/branches/" + branch + "/protection/required_status_checks"
}

func slurped(pages ...string) githubTestResponse {
	return githubTestResponse{stdout: "[" + strings.Join(pages, ",") + "]\n"}
}

// rollupForbidden is what GitHub answers when the credential may read the
// repository but not the Checks API context behind the status rollup.
func rollupForbidden() githubTestResponse {
	return githubTestResponse{
		stderr: "gh: Resource not accessible by personal access token (HTTP 403)",
		code:   1,
	}
}

// fallbackResponses is the command set a healthy fallback poll needs: the PR
// head read, a rollup that refuses, one workflow run at that head, its jobs,
// the PR base branch, and that branch's required checks.
func fallbackResponses(runsPage, jobsPage, requiredChecks string) map[string]githubTestResponse {
	return map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): rollupForbidden(),
		fallbackRunsCmd(fallbackHead):                             slurped(runsPage),
		fallbackJobsCmd(101):                                      slurped(jobsPage),
		fallbackBaseCmd:                                           {stdout: "main\n"},
		fallbackRequiredCmd("main"):                               {stdout: requiredChecks},
	}
}

func fallbackPR() *scm.PR {
	return &scm.PR{Number: "123", HeadSHA: fallbackHead}
}

const (
	oneGreenRunPage = `{"total_count":1,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"success","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7,"event":"pull_request","html_url":"https://github.com/test/repo/actions/runs/101"}]}`
	oneGreenJobPage = `{"total_count":1,"jobs":[{"id":201,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"build","status":"completed","conclusion":"success","completed_at":"2026-08-21T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/101/job/201"}]}`
	buildRequired   = `{"strict":true,"contexts":["build"],"checks":[{"context":"build","app_id":15368}]}`
)

// A rollup failure that is not capability evidence must keep the old behavior:
// surface the error, never derive narrower Actions evidence from it. No Actions
// command is registered here, so reaching the fallback would fail differently.
func TestGetChecksRollupFailureWithoutCapabilityEvidenceDoesNotFallBack(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): {
			stderr: "gh: Something went wrong (HTTP 502)",
			code:   1,
		},
	}), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if err == nil {
		t.Fatal("GetChecks() error = nil, want the rollup read failure")
	}
	if errors.Is(err, ErrRollupUnavailable) {
		t.Fatalf("GetChecks() error = %v, want it not classified as rollup-unavailable", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("GetChecks() error = %v, want the provider failure preserved", err)
	}
}

func TestGetChecksFallsBackToActionsJobsAtExactHead(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(fallbackResponses(oneGreenRunPage, oneGreenJobPage, buildRequired)), nil, "", fallbackRepo)

	checks, err := host.GetChecks(context.Background(), fallbackPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want one job-level check", checks)
	}
	check := checks[0]
	if check.Name != "build" || check.Bucket != scm.CheckBucketPass || check.State != "SUCCESS" {
		t.Fatalf("check = %+v, want a passing build job", check)
	}
	if check.Link != "https://github.com/test/repo/actions/runs/101/job/201" {
		t.Fatalf("check.Link = %q, want the job link", check.Link)
	}
	if check.CompletedAt.IsZero() {
		t.Fatal("check.CompletedAt is zero, want the job completion time")
	}
}

// A run still in flight is pending evidence, not unavailable evidence, and it
// needs no required-check certification: pending can never be read as green.
// The required-check commands are deliberately absent.
func TestGetChecksFallbackPendingJobStaysPending(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): rollupForbidden(),
		fallbackRunsCmd(fallbackHead):                             slurped(`{"total_count":1,"workflow_runs":[{"id":101,"name":"CI","status":"in_progress","conclusion":"","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7}]}`),
		fallbackJobsCmd(101):                                      slurped(`{"total_count":1,"jobs":[{"id":201,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"build","status":"in_progress","conclusion":""}]}`),
	}), nil, "", fallbackRepo)

	checks, err := host.GetChecks(context.Background(), fallbackPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks = %+v, want a single pending check", checks)
	}
}

func TestGetChecksFallbackFailedJobIsNonGreen(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): rollupForbidden(),
		fallbackRunsCmd(fallbackHead):                             slurped(`{"total_count":1,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"failure","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7}]}`),
		fallbackJobsCmd(101):                                      slurped(`{"total_count":2,"jobs":[{"id":201,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"build","status":"completed","conclusion":"success"},{"id":202,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"test","status":"completed","conclusion":"failure"}]}`),
	}), nil, "", fallbackRepo)

	checks, err := host.GetChecks(context.Background(), fallbackPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	failing := 0
	for _, check := range checks {
		if check.Failing() {
			failing++
		}
	}
	if len(checks) != 2 || failing != 1 {
		t.Fatalf("checks = %+v, want two checks with one failing", checks)
	}
}

// Actions cannot see check runs published by other apps, so a required check
// with no Actions result at this head leaves the evidence incomplete.
func TestGetChecksFallbackRequiredCheckWithoutMappingStaysUnavailable(t *testing.T) {
	t.Parallel()

	required := `{"strict":true,"contexts":["build","coverage/project"]}`
	host := New(githubTestCmdFactory(fallbackResponses(oneGreenRunPage, oneGreenJobPage, required)), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsEvidenceMissing) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want missing Actions evidence", err)
	}
	if !strings.Contains(err.Error(), "coverage/project") {
		t.Fatalf("GetChecks() error = %v, want the unmapped required check named", err)
	}
}

// Two green Actions results for one required identity - the routine shape when
// a workflow runs for both the push and the pull_request event - must never be
// resolved into a pass.
func TestGetChecksFallbackAmbiguousMappingNeverPasses(t *testing.T) {
	t.Parallel()

	runs := `{"total_count":2,"workflow_runs":[` +
		`{"id":101,"name":"CI","status":"completed","conclusion":"success","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7,"event":"pull_request"},` +
		`{"id":102,"name":"CI","status":"completed","conclusion":"success","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7,"event":"push"}]}`
	responses := fallbackResponses(runs, oneGreenJobPage, buildRequired)
	responses[fallbackJobsCmd(102)] = slurped(`{"total_count":1,"jobs":[{"id":202,"run_id":102,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"build","status":"completed","conclusion":"success"}]}`)
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsEvidenceAmbiguous) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want ambiguous Actions evidence", err)
	}
}

func TestGetChecksFallbackRejectsRunOnAnotherCommit(t *testing.T) {
	t.Parallel()

	runs := `{"total_count":1,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"success","head_sha":"` + fallbackOther + `","run_attempt":1,"workflow_id":7}]}`
	host := New(githubTestCmdFactory(fallbackResponses(runs, oneGreenJobPage, buildRequired)), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsHeadMismatch) || !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("GetChecks() error = %v, want a head mismatch", err)
	}
}

// A green job left behind by attempt 1 must not certify a commit whose current
// attempt is 2.
func TestGetChecksFallbackRejectsSupersededAttemptJob(t *testing.T) {
	t.Parallel()

	runs := `{"total_count":1,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"success","head_sha":"` + fallbackHead + `","run_attempt":2,"workflow_id":7}]}`
	host := New(githubTestCmdFactory(fallbackResponses(runs, oneGreenJobPage, buildRequired)), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsHeadMismatch) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want the superseded attempt refused", err)
	}
	if !strings.Contains(err.Error(), "attempt 1") {
		t.Fatalf("GetChecks() error = %v, want the stale attempt named", err)
	}
}

func TestGetChecksFallbackFollowsRunAndJobPagination(t *testing.T) {
	t.Parallel()

	runPage1 := `{"total_count":2,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"success","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7}]}`
	runPage2 := `{"total_count":2,"workflow_runs":[{"id":102,"name":"Docs","status":"completed","conclusion":"success","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":8}]}`
	jobPage1 := `{"total_count":2,"jobs":[{"id":201,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"build","status":"completed","conclusion":"success"}]}`
	jobPage2 := `{"total_count":2,"jobs":[{"id":202,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"lint","status":"completed","conclusion":"success"}]}`

	responses := map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): rollupForbidden(),
		fallbackRunsCmd(fallbackHead):                             slurped(runPage1, runPage2),
		fallbackJobsCmd(101):                                      slurped(jobPage1, jobPage2),
		fallbackJobsCmd(102):                                      slurped(`{"total_count":1,"jobs":[{"id":203,"run_id":102,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"docs","status":"completed","conclusion":"success"}]}`),
		fallbackBaseCmd:                                           {stdout: "main\n"},
		fallbackRequiredCmd("main"):                               {stdout: `{"strict":true,"contexts":["build","lint","docs"]}`},
	}
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	checks, err := host.GetChecks(context.Background(), fallbackPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("checks = %+v, want every job from every page", checks)
	}
}

// A jobs listing whose pages disagree with total_count is short, and a short
// listing is exactly how a failing job would disappear into a pass.
func TestGetChecksFallbackRejectsIncompleteJobPagination(t *testing.T) {
	t.Parallel()

	jobs := `{"total_count":2,"jobs":[{"id":201,"run_id":101,"run_attempt":1,"head_sha":"` + fallbackHead + `","name":"build","status":"completed","conclusion":"success"}]}`
	host := New(githubTestCmdFactory(fallbackResponses(oneGreenRunPage, jobs, buildRequired)), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsAPIFailure) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want the short job listing refused", err)
	}
}

func TestGetChecksFallbackUnreadableRequiredChecksStaysUnavailable(t *testing.T) {
	t.Parallel()

	responses := fallbackResponses(oneGreenRunPage, oneGreenJobPage, "")
	responses[fallbackRequiredCmd("main")] = githubTestResponse{
		stderr: "gh: Must have admin rights to Repository. (HTTP 403)",
		code:   1,
	}
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsEvidenceMissing) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want an unreadable required-check definition", err)
	}
}

// With no required-check definition nothing bounds the check runs Actions
// cannot see, so all-green Actions jobs still do not certify the commit.
func TestGetChecksFallbackUnprotectedBranchNeverCertifies(t *testing.T) {
	t.Parallel()

	responses := fallbackResponses(oneGreenRunPage, oneGreenJobPage, "")
	responses[fallbackRequiredCmd("main")] = githubTestResponse{
		stderr: "gh: Branch not protected (HTTP 404)",
		code:   1,
	}
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsEvidenceMissing) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want no certification without required checks", err)
	}
}

func TestGetChecksFallbackActionsAPIFailureIsUnavailableEvidence(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): rollupForbidden(),
		fallbackRunsCmd(fallbackHead): {
			stderr: "gh: Resource not accessible by personal access token (HTTP 403)",
			code:   1,
		},
	}), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsAPIFailure) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want an Actions API failure", err)
	}
	if !errors.Is(err, ErrRollupUnavailable) {
		t.Fatalf("GetChecks() error = %v, want the primary failure preserved too", err)
	}
}

func TestGetChecksFallbackWithoutAnyRunIsMissingEvidence(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		fallbackPRView: {stdout: fallbackHead + "\n"},
		githubCommitChecksCommand("", fallbackRepo, fallbackHead): rollupForbidden(),
		fallbackRunsCmd(fallbackHead):                             slurped(`{"total_count":0,"workflow_runs":[]}`),
	}), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsEvidenceMissing) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want missing Actions evidence", err)
	}
}

// A run that claims success while exposing no job is not evidence of anything.
func TestGetChecksFallbackJoblessSuccessfulRunIsNotEvidence(t *testing.T) {
	t.Parallel()

	responses := fallbackResponses(oneGreenRunPage, `{"total_count":0,"jobs":[]}`, buildRequired)
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	_, err := host.GetChecks(context.Background(), fallbackPR())
	if !errors.Is(err, ErrActionsEvidenceMissing) || !errors.Is(err, scm.ErrChecksUnavailable) {
		t.Fatalf("GetChecks() error = %v, want a jobless successful run refused", err)
	}
}

// A skipped workflow legitimately has no job, and its own conclusion is the
// evidence. It cannot be mistaken for a pass because the required-check set
// still has to map.
func TestGetChecksFallbackJoblessSkippedRunReportsTheRun(t *testing.T) {
	t.Parallel()

	runs := `{"total_count":1,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"skipped","head_sha":"` + fallbackHead + `","run_attempt":1,"workflow_id":7}]}`
	responses := fallbackResponses(runs, `{"total_count":0,"jobs":[]}`, `{"strict":true,"contexts":["CI"]}`)
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	checks, err := host.GetChecks(context.Background(), fallbackPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketSkip || checks[0].Name != "CI" {
		t.Fatalf("checks = %+v, want the skipped run reported", checks)
	}
}

func TestGetChecksFallbackUsesTheRecordedPRBaseBranch(t *testing.T) {
	t.Parallel()

	responses := fallbackResponses(oneGreenRunPage, oneGreenJobPage, "")
	delete(responses, fallbackBaseCmd)
	delete(responses, fallbackRequiredCmd("main"))
	responses[fallbackRequiredCmd("release")] = githubTestResponse{stdout: buildRequired}
	host := New(githubTestCmdFactory(responses), nil, "", fallbackRepo)

	pr := fallbackPR()
	pr.BaseBranch = "release"
	checks, err := host.GetChecks(context.Background(), pr)
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want the build job certified against the recorded base", checks)
	}
}

func TestClassifyRollupUnavailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"fine-grained token 403", "gh: Resource not accessible by personal access token (HTTP 403)", true},
		{"integration 403", "Resource not accessible by integration", true},
		{"insufficient scopes", "your token has not been granted the required scopes: insufficient scopes", true},
		{"rollup field error", "GraphQL: could not resolve statusCheckRollup", true},
		{"bad gateway", "gh: Something went wrong (HTTP 502)", false},
		{"rate limited", "API rate limit exceeded", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyRollupUnavailable(tc.output, errors.New("exit status 1")); got != tc.want {
				t.Fatalf("classifyRollupUnavailable(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}
