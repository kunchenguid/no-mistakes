package github

import (
	"context"
	"testing"
)

// Workflow-run discovery is fail-closed like external review discovery: a page
// without its workflow_runs collection, or a run without a positive id, is
// malformed, never an empty or passing CI result.
func TestGetWorkflowRunChecksRejectsMalformedDiscovery(t *testing.T) {
	t.Parallel()

	for name, payload := range map[string]string{
		"missing collection": `[{"total_count":0}]`,
		"null collection":    `[{"total_count":0,"workflow_runs":null}]`,
		"negative run id":    `[{"total_count":1,"workflow_runs":[{"id":-1,"name":"build","status":"completed","conclusion":"success"}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/o/r/actions/runs -f head_sha=abc -f per_page=100 --paginate --slurp": {stdout: payload},
			}), nil, "", "o/r")
			if checks, err := host.getWorkflowRunChecks(context.Background(), "abc"); err == nil {
				t.Fatalf("accepted malformed discovery %s: checks=%+v", payload, checks)
			}
		})
	}
}

func TestGetWorkflowRunChecksAcceptsEmptyCollection(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/o/r/actions/runs -f head_sha=abc -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
	}), nil, "", "o/r")
	checks, err := host.getWorkflowRunChecks(context.Background(), "abc")
	if err != nil || len(checks) != 0 {
		t.Fatalf("empty discovery = %+v, %v; want no checks and no error", checks, err)
	}
}
