package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// The CI step tells a third-party review bot's check from the repository's own
// Actions jobs by the check suite's app slug, which only the commit rollup
// query reports. A CheckRun without a check suite, and a StatusContext, carry
// no app identity at all.
func TestGetChecksCarriesCheckSuiteAppSlug(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","databaseId":2,"name":"test","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/2","checkSuite":{"app":{"slug":"github-actions"}}},
				{"__typename":"CheckRun","databaseId":3,"name":"Greptile Review","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://greptile.com/","checkSuite":{"app":{"slug":"greptile-apps"}}},
				{"__typename":"CheckRun","databaseId":4,"name":"orphan","status":"COMPLETED","conclusion":"SUCCESS"},
				{"__typename":"StatusContext","id":"status-5","context":"ci/external","state":"SUCCESS","targetUrl":"https://ci.example/1"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":0,"workflow_runs":[]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	want := map[string]struct{ app, id string }{
		"test":            {app: "github-actions", id: "github-check-run:2"},
		"Greptile Review": {app: "greptile-apps", id: "github-check-run:3"},
		"orphan":          {id: "github-check-run:4"},
		"ci/external":     {id: "github-status:status-5"},
	}
	if len(checks) != len(want) {
		t.Fatalf("checks = %+v, want %d", checks, len(want))
	}
	for _, check := range checks {
		expected, ok := want[check.Name]
		if !ok {
			t.Fatalf("unexpected check %+v", check)
		}
		if check.App != expected.app || check.ProviderID != expected.id {
			t.Fatalf("check %q identity = (%q, %q), want (%q, %q)", check.Name, check.App, check.ProviderID, expected.app, expected.id)
		}
	}
}
