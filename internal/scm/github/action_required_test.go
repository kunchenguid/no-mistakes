package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// heldWorkflowRunResponses is the shape GitHub reports for a pull request whose
// workflows are held for maintainer approval: the head commit carries no check
// runs at all, and the workflow runs themselves conclude action_required.
func heldWorkflowRunResponses(runsJSON string, extra map[string]githubTestResponse) map[string]githubTestResponse {
	responses := map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stderr: "no checks reported on the 'feature' branch\n",
			code:   1,
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: runsJSON,
		},
	}
	for key, response := range extra {
		responses[key] = response
	}
	return responses
}

const heldWorkflowRun = `[{"total_count":1,"workflow_runs":[
	{"id":101,"name":"CI","display_title":"","status":"completed","conclusion":"action_required","updated_at":"2026-09-24T12:34:56Z"}
]}]` + "\n"

// A workflow run held for maintainer approval ran no jobs, so it is a wait on a
// human rather than a verdict on the commit. Reporting it as a failing check
// sends the CI step's auto-fix rounds after checks that never executed.
func TestGetChecksTreatsHeldWorkflowRunWithNoJobsAsAwaitingApproval(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(heldWorkflowRunResponses(heldWorkflowRun, map[string]githubTestResponse{
		"gh run view 101 --repo test/repo --json jobs": {stdout: `{"jobs":[]}` + "\n"},
	})), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want 1: %+v", len(checks), checks)
	}
	got := checks[0]
	if got.Bucket != scm.CheckBucketPending {
		t.Errorf("held workflow run bucket = %q, want %q", got.Bucket, scm.CheckBucketPending)
	}
	if !got.AwaitingApproval {
		t.Errorf("held workflow run AwaitingApproval = false, want true: %+v", got)
	}
	if got.State != "ACTION_REQUIRED" {
		t.Errorf("held workflow run State = %q, want ACTION_REQUIRED", got.State)
	}
}

// An action_required run that DID execute jobs is not the maintainer-approval
// hold, so its classification is unchanged.
func TestGetChecksKeepsActionRequiredWorkflowRunWithJobsFailing(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(heldWorkflowRunResponses(heldWorkflowRun, map[string]githubTestResponse{
		"gh run view 101 --repo test/repo --json jobs": {
			stdout: `{"jobs":[{"databaseId":9001,"name":"build","status":"completed","conclusion":"action_required"}]}` + "\n",
		},
	})), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want 1: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Bucket != scm.CheckBucketFail || got.AwaitingApproval {
		t.Fatalf("action_required run with jobs = %+v, want fail bucket and AwaitingApproval false", got)
	}
}

// The job read is positive evidence, so a run whose jobs cannot be read keeps
// the classification it has today rather than being guessed as a hold.
func TestGetChecksKeepsActionRequiredWorkflowRunFailingWhenJobsUnreadable(t *testing.T) {
	t.Parallel()

	// No "gh run view 101" mapping: the job read fails.
	host := New(githubTestCmdFactory(heldWorkflowRunResponses(heldWorkflowRun, nil)), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want 1: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Bucket != scm.CheckBucketFail || got.AwaitingApproval {
		t.Fatalf("unreadable action_required run = %+v, want fail bucket and AwaitingApproval false", got)
	}
}

// A workflow run that actually failed is unaffected: no job read is made for it
// and it stays a failing check.
func TestGetChecksKeepsGenuineWorkflowRunFailureFailing(t *testing.T) {
	t.Parallel()

	runs := `[{"total_count":1,"workflow_runs":[
		{"id":101,"name":"CI","display_title":"","status":"completed","conclusion":"failure","updated_at":"2026-09-24T12:34:56Z"}
	]}]` + "\n"
	host := New(githubTestCmdFactory(heldWorkflowRunResponses(runs, nil)), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want 1: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Bucket != scm.CheckBucketFail || got.AwaitingApproval {
		t.Fatalf("failed workflow run = %+v, want fail bucket and AwaitingApproval false", got)
	}
}

// Only a PRESENT, empty job list is evidence that the run executed nothing.
// A response whose `jobs` key is missing, or explicitly null, carries no job
// data at all: that is unreadable, not proof of a hold, so the run keeps the
// failing classification it had before this behaviour existed.
func TestGetChecksKeepsActionRequiredWorkflowRunFailingWhenJobsAreAbsent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"jobs key missing", `{}` + "\n"},
		{"jobs explicitly null", `{"jobs":null}` + "\n"},
		{"jobs missing alongside other fields", `{"databaseId":101}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := New(githubTestCmdFactory(heldWorkflowRunResponses(heldWorkflowRun, map[string]githubTestResponse{
				"gh run view 101 --repo test/repo --json jobs": {stdout: tc.body},
			})), nil, "", "test/repo")

			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
			if err != nil {
				t.Fatalf("GetChecks() error = %v", err)
			}
			if len(checks) != 1 {
				t.Fatalf("GetChecks() returned %d checks, want 1: %+v", len(checks), checks)
			}
			if got := checks[0]; got.Bucket != scm.CheckBucketFail || got.AwaitingApproval {
				t.Fatalf("action_required run with absent job data = %+v, want fail bucket and AwaitingApproval false", got)
			}
		})
	}
}
