package azuredevops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetChecksNativeCommitAssociation(t *testing.T) {
	t.Parallel()
	head, merge := strings.Repeat("a", 40), strings.Repeat("b", 40)
	policyKey := "az repos pr policy list --id 42 --organization " + testOrg + " --output json"
	buildKey := "az pipelines runs show --id 9 --organization " + testOrg + " --project " + testProject + " --output json"
	statusKey := "az devops invoke --area git --resource pullRequestStatuses --route-parameters project=" + testProject + " repositoryId=" + testRepo + " pullRequestId=42 --organization " + testOrg + " --api-version 7.1 --output json"
	iterationKey := strings.Replace(statusKey, "pullRequestStatuses", "pullRequestIterations", 1)
	build := fmt.Sprintf(`{"id":9,"status":"completed","result":"succeeded","definition":{"id":7},"project":{"name":%q},"repository":{"id":"repo-id","type":"TfsGit"},"sourceBranch":"refs/pull/42/merge","sourceVersion":%q}`, testProject, merge)
	statuses := `{"count":1,"value":[{"id":3,"iterationId":2,"state":"succeeded","context":{"name":"lint","genre":"ci"},"createdBy":{"id":"account"}}]}`
	iterations := fmt.Sprintf(`{"count":1,"value":[{"id":2,"sourceRefCommit":{"commitId":%q}}]}`, head)
	buildPolicy := `{"status":"approved","configuration":{"type":{"displayName":"Build"},"settings":{"buildDefinitionId":7}},"context":{"buildId":9}}`
	statusPolicy := `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci","authorId":"account"}}}`
	for _, tc := range []struct {
		name, policy, build, statuses, iterations, live, mergeStatus string
		want                                                         string
		fail                                                         bool
	}{
		{name: "native build", policy: buildPolicy, build: build, want: head},
		{name: "native build still running", policy: buildPolicy, build: strings.Replace(build, `"status":"completed"`, `"status":"inProgress"`, 1)},
		{name: "native build failure contradicts approval", policy: buildPolicy, build: strings.Replace(build, `"result":"succeeded"`, `"result":"failed"`, 1)},
		{name: "stale build", policy: buildPolicy, build: strings.Replace(build, merge, strings.Repeat("c", 40), 1)},
		{name: "wrong definition", policy: `{"status":"approved","configuration":{"type":{"displayName":"Build"},"settings":{"buildDefinitionId":8}},"context":{"buildId":9}}`, build: build},
		{name: "lookup failure", policy: buildPolicy, fail: true},
		{name: "source lag", policy: buildPolicy, build: build, live: strings.Repeat("c", 40), fail: true},
		{name: "latest native status", policy: statusPolicy, statuses: `{"count":2,"value":[{"id":4,"iterationId":2,"state":"succeeded","updatedDate":"2026-04-24T05:15:00Z","context":{"name":"lint","genre":"ci"},"createdBy":{"id":"account"}},{"id":3,"iterationId":1,"state":"succeeded","updatedDate":"2026-04-24T04:15:00Z","context":{"name":"lint","genre":"ci"},"createdBy":{"id":"account"}}]}`, iterations: iterations, want: head},
		{name: "newer native failure", policy: statusPolicy, statuses: `{"count":2,"value":[{"id":3,"iterationId":1,"state":"succeeded","updatedDate":"2026-04-24T04:15:00Z","context":{"name":"lint","genre":"ci"},"createdBy":{"id":"account"}},{"id":4,"iterationId":2,"state":"failed","updatedDate":"2026-04-24T05:15:00Z","context":{"name":"lint","genre":"ci"},"createdBy":{"id":"account"}}]}`},
		{name: "native status", policy: statusPolicy, statuses: statuses, iterations: iterations, want: head},
		{name: "stale status", policy: statusPolicy, statuses: statuses, iterations: strings.Replace(iterations, head, strings.Repeat("c", 40), 1), want: strings.Repeat("c", 40)},
		{name: "code independent status", policy: `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci"}}}`, statuses: strings.Replace(statuses, `"iterationId":2`, `"iterationId":0`, 1)},
		{name: "wrong status identity", policy: `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci","authorId":"other"}}}`, statuses: statuses},
		{name: "missing build association", policy: `{"status":"approved","configuration":{"type":{"displayName":"Build"}}}`},
		{name: "wrong build repository", policy: buildPolicy, build: strings.Replace(build, `"repo-id"`, `"other-repo"`, 1)},
		{name: "wrong build PR", policy: buildPolicy, build: strings.Replace(build, "refs/pull/42/merge", "refs/pull/43/merge", 1)},
		{name: "merge computation lag", policy: buildPolicy, build: build, mergeStatus: "queued"},
		{name: "status lookup failure", policy: `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci"}}}`, fail: true},
		{name: "iteration lookup failure", policy: `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci"}}}`, statuses: statuses, fail: true},
		{name: "ambiguous statuses", policy: `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci"}}}`, statuses: strings.Replace(strings.Replace(statuses, `"count":1`, `"count":2`, 1), `}]}`, `},{"id":4,"iterationId":3,"state":"succeeded","context":{"name":"lint","genre":"ci"}}]}`, 1)},
		{name: "duplicate iterations", policy: `{"status":"approved","configuration":{"type":{"displayName":"Status"},"settings":{"statusName":"lint","statusGenre":"ci"}}}`, statuses: statuses, iterations: fmt.Sprintf(`{"count":2,"value":[{"id":2,"sourceRefCommit":{"commitId":%q}},{"id":2,"sourceRefCommit":{"commitId":%q}}]}`, head, head)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live := tc.live
			if live == "" {
				live = head
			}
			mergeStatus := tc.mergeStatus
			if mergeStatus == "" {
				mergeStatus = "succeeded"
			}
			responses := map[string]azdoTestResponse{
				policyKey: {stdout: "[" + tc.policy + "]"},
				"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: fmt.Sprintf(`{"pullRequestId":42,"status":"active","sourceRefName":"refs/heads/feature","targetRefName":"refs/heads/develop","mergeStatus":%q,"lastMergeSourceCommit":{"commitId":%q},"lastMergeCommit":{"commitId":%q},"repository":{"id":"repo-id","name":%q,"project":{"name":%q}}}`, mergeStatus, head, merge, testRepo, testProject)},
				"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: fmt.Sprintf(`[{"name":"refs/heads/feature","objectId":%q}]`, live)},
			}
			if tc.build != "" {
				responses[buildKey] = azdoTestResponse{stdout: tc.build}
			}
			if tc.statuses != "" {
				responses[statusKey] = azdoTestResponse{stdout: tc.statuses}
			}
			if tc.iterations != "" {
				responses[iterationKey] = azdoTestResponse{stdout: tc.iterations}
			}
			checks, err := newTestHost(responses).GetChecks(context.Background(), &scm.PR{Number: "42"})
			if tc.fail {
				if err == nil {
					t.Fatalf("expected closed failure, got %+v", checks)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) != 1 || checks[0].HeadSHA != tc.want {
				t.Fatalf("checks=%+v, want head %q", checks, tc.want)
			}
		})
	}
}
