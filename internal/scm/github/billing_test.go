package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestBillingFailuresRequireExactJobAndAnnotationEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		edit            func(map[string]any)
		annotations     string
		jobError        bool
		annotationError bool
		want            bool
	}{
		{name: "never started billing refusal", want: true},
		{name: "annotation extra prose is discarded", annotations: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + ` private-marker","raw_details":"private-marker","path":"/Users/private-marker"}]]`, want: true},
		{name: "one second test failure", edit: func(j map[string]any) {
			j["started_at"] = "2026-10-28T00:00:00Z"
			j["completed_at"] = "2026-10-28T00:00:01Z"
			j["steps"] = []any{map[string]any{"number": 1, "conclusion": "success"}, map[string]any{"number": 2, "conclusion": "failure"}}
		}},
		{name: "missing steps", edit: func(j map[string]any) { delete(j, "steps") }},
		{name: "null steps", edit: func(j map[string]any) { j["steps"] = nil }},
		{name: "queued job", edit: func(j map[string]any) { j["status"] = "queued" }},
		{name: "successful job", edit: func(j map[string]any) { j["conclusion"] = "success" }},
		{name: "wrong job", edit: func(j map[string]any) { j["id"] = 3 }},
		{name: "wrong run", edit: func(j map[string]any) { j["run_id"] = 9 }},
		{name: "wrong head", edit: func(j map[string]any) { j["head_sha"] = "old-head" }},
		{name: "wrong check", edit: func(j map[string]any) { j["check_run_url"] = "https://api.github.com/repos/test/repo/check-runs/23" }},
		{name: "wrong repository", edit: func(j map[string]any) { j["check_run_url"] = "https://api.github.com/repos/other/repo/check-runs/22" }},
		{name: "missing check link", edit: func(j map[string]any) { delete(j, "check_run_url") }},
		{name: "bare check id is not a URL", edit: func(j map[string]any) { j["check_run_url"] = "22" }},
		{name: "unreadable job", jobError: true},
		{name: "unreadable annotations", annotationError: true},
		{name: "no annotations", annotations: `[[]]`},
		{name: "null annotations", annotations: `null`},
		{name: "malformed annotations", annotations: `{`},
		{name: "missing annotation fields", annotations: `[[{}]]`},
		{name: "unrelated annotation", annotations: `[[{"annotation_level":"failure","message":"Test failed in one second"}]]`},
		{name: "billing mention is not a refusal", annotations: `[[{"annotation_level":"failure","message":"Test expected: ` + billingRefusalAnnotation + `"}]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := map[string]any{
				"id": 2, "run_id": 1, "head_sha": "head", "status": "completed", "conclusion": "failure",
				"steps": []any{}, "check_run_url": "https://api.github.com/repos/test/repo/check-runs/22",
			}
			if tc.edit != nil {
				tc.edit(job)
			}
			payload, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			annotations := tc.annotations
			if annotations == "" {
				annotations = `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + `"}]]`
			}
			responses := map[string]githubTestResponse{
				"gh api repos/test/repo/actions/jobs/2":                               {stdout: string(payload)},
				"gh api repos/test/repo/check-runs/22/annotations --paginate --slurp": {stdout: annotations},
			}
			if tc.jobError {
				responses["gh api repos/test/repo/actions/jobs/2"] = githubTestResponse{stderr: "HTTP 403", code: 1}
			}
			if tc.annotationError {
				responses["gh api repos/test/repo/check-runs/22/annotations --paginate --slurp"] = githubTestResponse{stderr: "HTTP 403", code: 1}
			}
			host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
			checks := []scm.Check{{Name: "build", ProviderID: "github-check-run:22", Bucket: scm.CheckBucketFail, Kind: scm.CheckKindRun, State: "FAILURE", App: "github-actions", Link: "https://github.com/test/repo/actions/runs/1/job/2"}}
			host.markBillingFailures(context.Background(), checks, "head")
			if got := checks[0].InfrastructureFailure; (got != "") != tc.want || tc.want && got != billingRefusalAnnotation {
				t.Fatalf("diagnosis=%q, want billing=%v", got, tc.want)
			}
			if !checks[0].Failing() || checks[0].State != "FAILURE" || checks[0].PreRunFailure {
				t.Fatalf("classification changed the failure verdict: %+v", checks[0])
			}
		})
	}
}

func TestBillingFailuresNeverBorrowSameNamedJobEvidence(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/actions/jobs/2":                               {stdout: `{"id":2,"run_id":1,"head_sha":"head","status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/22"}`},
		"gh api repos/test/repo/check-runs/22/annotations --paginate --slurp": {stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + `"}]]`},
		// An empty run job list cannot establish that job 3 never executed.
		"gh run view 1 --repo test/repo --json jobs": {stdout: `{"jobs":[]}`},
	}), nil, "", "test/repo")
	checks := []scm.Check{
		{Name: "build", ProviderID: "github-check-run:22", Bucket: scm.CheckBucketFail, Link: "https://github.com/test/repo/actions/runs/1/job/2"},
		{Name: "build", ProviderID: "github-check-run:23", Bucket: scm.CheckBucketFail, Link: "https://github.com/test/repo/actions/runs/1/job/3"},
		{Name: "build", ProviderID: "github-workflow-run:1", Bucket: scm.CheckBucketFail, Link: "https://github.com/test/repo/actions/runs/1"},
	}
	host.markBillingFailures(context.Background(), checks, "head")
	if checks[0].InfrastructureFailure != billingRefusalAnnotation || checks[1].InfrastructureFailure != "" || checks[2].InfrastructureFailure != "" {
		t.Fatalf("classification borrowed evidence between jobs: %+v", checks)
	}
}

func TestBillingFailuresCheckRunURLIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		host string
		repo string
		url  string
		want bool
	}{
		{name: "canonical repository casing", repo: "TEST/REPO", url: "https://api.github.com/repos/test/repo/check-runs/22", want: true},
		{name: "response repository casing", repo: "test/repo", url: "https://api.github.com/repos/TEST/REPO/check-runs/22", want: true},
		{name: "explicit public host", host: "github.com", repo: "TEST/REPO", url: "https://api.github.com/repos/test/repo/check-runs/22", want: true},
		{name: "public host casing", host: "GITHUB.COM", repo: "TEST/REPO", url: "https://API.GITHUB.COM/repos/test/repo/check-runs/22", want: true},
		{name: "enterprise cloud", host: "contoso.ghe.com", repo: "contoso.ghe.com/TEST/REPO", url: "https://api.contoso.ghe.com/repos/test/repo/check-runs/22", want: true},
		{name: "enterprise cloud casing", host: "CONTOSO.GHE.COM", repo: "contoso.ghe.com/TEST/REPO", url: "https://API.CONTOSO.GHE.COM/repos/test/repo/check-runs/22", want: true},
		{name: "enterprise server", host: "github.example.com", repo: "github.example.com/TEST/REPO", url: "https://github.example.com/api/v3/repos/test/repo/check-runs/22", want: true},
		{name: "enterprise server casing", host: "GITHUB.EXAMPLE.COM", repo: "github.example.com/test/repo", url: "https://GITHUB.EXAMPLE.COM/api/v3/repos/TEST/REPO/check-runs/22", want: true},
		{name: "cloud browser origin", host: "contoso.ghe.com", repo: "test/repo", url: "https://contoso.ghe.com/api/v3/repos/test/repo/check-runs/22"},
		{name: "another cloud tenant", host: "contoso.ghe.com", repo: "test/repo", url: "https://api.other.ghe.com/repos/test/repo/check-runs/22"},
		{name: "cloud server API path", host: "contoso.ghe.com", repo: "test/repo", url: "https://api.contoso.ghe.com/api/v3/repos/test/repo/check-runs/22"},
		{name: "server cloud API origin", host: "github.example.com", repo: "test/repo", url: "https://api.github.example.com/repos/test/repo/check-runs/22"},
		{name: "server missing API prefix", host: "github.example.com", repo: "test/repo", url: "https://github.example.com/repos/test/repo/check-runs/22"},
		{name: "public origin on enterprise", host: "github.example.com", repo: "test/repo", url: "https://api.github.com/repos/test/repo/check-runs/22"},
		{name: "lookalike origin", repo: "test/repo", url: "https://api.github.com.example.com/repos/test/repo/check-runs/22"},
		{name: "wrong owner", repo: "TEST/REPO", url: "https://api.github.com/repos/other/repo/check-runs/22"},
		{name: "wrong repository", repo: "TEST/REPO", url: "https://api.github.com/repos/test/other/check-runs/22"},
		{name: "wrong check", repo: "TEST/REPO", url: "https://api.github.com/repos/test/repo/check-runs/23"},
		{name: "insecure scheme", repo: "test/repo", url: "http://api.github.com/repos/test/repo/check-runs/22"},
		{name: "userinfo", repo: "test/repo", url: "https://user@api.github.com/repos/test/repo/check-runs/22"},
		{name: "unexpected port", repo: "test/repo", url: "https://api.github.com:8443/repos/test/repo/check-runs/22"},
		{name: "query", repo: "test/repo", url: "https://api.github.com/repos/test/repo/check-runs/22?other=23"},
		{name: "fragment", repo: "test/repo", url: "https://api.github.com/repos/test/repo/check-runs/22#23"},
		{name: "extra path", repo: "test/repo", url: "https://api.github.com/repos/test/repo/check-runs/22/annotations"},
		{name: "malformed URL", repo: "test/repo", url: "https://api.github.com/repos/test/repo/check-runs/%zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slug := tc.repo
			if parts := strings.Split(slug, "/"); len(parts) == 3 {
				slug = strings.Join(parts[1:], "/")
			}
			payload, err := json.Marshal(map[string]any{
				"id": 2, "run_id": 1, "head_sha": "head", "status": "completed", "conclusion": "failure",
				"steps": []any{}, "check_run_url": tc.url,
			})
			if err != nil {
				t.Fatal(err)
			}
			command := "gh api "
			linkHost := tc.host
			if tc.host != "" {
				command += "--hostname " + tc.host + " "
			} else {
				linkHost = "github.com"
			}
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				command + "repos/" + slug + "/actions/jobs/2":                               {stdout: string(payload)},
				command + "repos/" + slug + "/check-runs/22/annotations --paginate --slurp": {stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + `"}]]`},
				command + "repos/" + slug + "/check-runs/23/annotations --paginate --slurp": {stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + `"}]]`},
			}), nil, tc.host, tc.repo)
			checks := []scm.Check{{ProviderID: "github-check-run:22", Bucket: scm.CheckBucketFail, Kind: scm.CheckKindRun, State: "FAILURE", App: "github-actions", Link: "https://" + linkHost + "/" + strings.ToLower(slug) + "/actions/runs/1/job/2"}}
			host.markBillingFailures(context.Background(), checks, "head")
			want := ""
			if tc.want {
				want = billingRefusalAnnotation
			}
			if checks[0].InfrastructureFailure != want || !checks[0].Failing() || checks[0].State != "FAILURE" || checks[0].PreRunFailure {
				t.Fatalf("check=%+v, want diagnosis=%q with failure preserved", checks[0], want)
			}
		})
	}
}

func TestGetChecksRetainsSanitizedBillingEvidenceAndFailure(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {stdout: `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/1/job/2"}]`},
		"gh api repos/test/repo/actions/jobs/2":                                       {stdout: `{"id":2,"run_id":1,"status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/22"}`},
		"gh api repos/test/repo/check-runs/22/annotations --paginate --slurp":         {stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + ` private-marker","raw_details":"private-marker"}]]`},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil || len(checks) != 1 || !checks[0].Failing() || checks[0].InfrastructureFailure != billingRefusalAnnotation {
		t.Fatalf("checks=%+v err=%v", checks, err)
	}
	encoded, err := json.Marshal(checks)
	if err != nil || strings.Contains(string(encoded), "private-marker") {
		t.Fatalf("unsanitized evidence: %s err=%v", encoded, err)
	}
}

func TestGetChecksRetainsConfirmedBillingEvidenceAcrossPolls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, changedIdentity, failedRead string
		initiallyUnproven                 bool
	}{
		{name: "job read fails", failedRead: "job"},
		{name: "annotation read fails", failedRead: "annotations"},
		{name: "different head", changedIdentity: "head", failedRead: "job"},
		{name: "different run", changedIdentity: "run", failedRead: "job"},
		{name: "different job", changedIdentity: "job", failedRead: "job"},
		{name: "different check", changedIdentity: "check", failedRead: "job"},
		{name: "initial job read fails", failedRead: "job", initiallyUnproven: true},
		{name: "initial annotation read fails", failedRead: "annotations", initiallyUnproven: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			responses := map[string]githubTestResponse{}
			host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
			for poll := 0; poll < 3; poll++ {
				head, runID, jobID, checkID := "head", 1, 2, 22
				if poll > 0 {
					switch tc.changedIdentity {
					case "head":
						head = "new-head"
					case "run":
						runID = 9
					case "job":
						jobID = 3
					case "check":
						checkID = 23
					}
				}
				responses["gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid"] = githubTestResponse{stdout: head}
				responses[githubCommitChecksCommand("", "test/repo", head)] = githubTestResponse{stdout: githubCommitChecksResponse(fmt.Sprintf(`[{"__typename":"CheckRun","databaseId":%d,"name":"build","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/test/repo/actions/runs/%d/job/%d","checkSuite":{"app":{"slug":"github-actions"}}}]`, checkID, runID, jobID))}
				responses["gh api --method GET repos/test/repo/actions/runs -f head_sha="+head+" -f per_page=100 --paginate --slurp"] = githubTestResponse{stdout: `[{"total_count":0,"workflow_runs":[]}]`}
				jobCommand := fmt.Sprintf("gh api repos/test/repo/actions/jobs/%d", jobID)
				annotationCommand := fmt.Sprintf("gh api repos/test/repo/check-runs/%d/annotations --paginate --slurp", checkID)
				responses[jobCommand] = githubTestResponse{stdout: fmt.Sprintf(`{"id":%d,"run_id":%d,"head_sha":%q,"status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/%d"}`, jobID, runID, head, checkID)}
				responses[annotationCommand] = githubTestResponse{stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + ` private-marker","raw_details":"private-marker"}]]`}
				if poll > 0 || tc.initiallyUnproven {
					command := jobCommand
					if tc.failedRead == "annotations" {
						command = annotationCommand
					}
					responses[command] = githubTestResponse{stderr: "HTTP 403", code: 1}
				}
				checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: head})
				if err != nil || len(checks) != 1 {
					t.Fatalf("poll %d: checks=%+v err=%v", poll, checks, err)
				}
				want := ""
				if !tc.initiallyUnproven && (poll == 0 || tc.changedIdentity == "") {
					want = billingRefusalAnnotation
				}
				if checks[0].InfrastructureFailure != want || !checks[0].Failing() || checks[0].State != "FAILURE" || checks[0].PreRunFailure {
					t.Fatalf("poll %d: check=%+v, want diagnosis=%q with failure preserved", poll, checks[0], want)
				}
			}
		})
	}
}
