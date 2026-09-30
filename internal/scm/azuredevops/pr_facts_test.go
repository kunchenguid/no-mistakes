package azuredevops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const azFactsSHA = "0123456789abcdef0123456789abcdef01234567"

const azFacts42 = `{"pullRequestId":42,"status":"active","sourceRefName":"refs/heads/feature","targetRefName":"refs/heads/develop","lastMergeSourceCommit":{"commitId":"` + azFactsSHA + `"},"repository":{"name":"myrepo","project":{"name":"myproject"}}}`

func TestReadPRFactsBindsExactRecordedIdentityAndLiveBase(t *testing.T) {
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	})
	pr := &scm.PR{Number: "42", URL: webPRURL(testOrg, testProject, testRepo, "", "42"), BaseBranch: "main"}
	facts, err := h.ReadPRFacts(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if facts.PR.Number != "42" || facts.PR.URL != pr.URL || facts.State != scm.PRStateOpen ||
		facts.SourceRepository != webPRURL(testOrg, testProject, testRepo, "", "") ||
		facts.SourceBranch != "feature" || facts.HeadSHA != azFactsSHA || facts.PR.HeadSHA != azFactsSHA ||
		facts.BaseBranch != "develop" || facts.PR.BaseBranch != "develop" {
		t.Fatalf("incomplete Azure PR facts: %+v", facts)
	}
}

func TestReadPRFactsRejectsIncompleteAndInconsistentData(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"wrong number", strings.Replace(azFacts42, `"pullRequestId":42`, `"pullRequestId":43`, 1)},
		{"missing head", strings.Replace(azFacts42, `"commitId":"`+azFactsSHA+`"`, `"commitId":""`, 1)},
		{"short head", strings.Replace(azFacts42, azFactsSHA, "1234", 1)},
		{"bad source ref", strings.Replace(azFacts42, "refs/heads/feature", "refs/tags/feature", 1)},
		{"bad target ref", strings.Replace(azFacts42, "refs/heads/develop", "develop", 1)},
		{"unknown status", strings.Replace(azFacts42, `"status":"active"`, `"status":"unknown"`, 1)},
		{"foreign target", strings.Replace(azFacts42, `"name":"myrepo"`, `"name":"other"`, 1)},
		{"bad fork", strings.Replace(azFacts42, `"repository":`, `"forkSource":{"repository":{}},"repository":`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json": {stdout: tc.body},
			})
			if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
				t.Fatal("ReadPRFacts accepted incomplete or inconsistent response")
			}
		})
	}
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json": {stdout: azFacts42},
	})
	if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: webPRURL(testOrg, testProject, testRepo, "", "43")}); err == nil {
		t.Fatal("ReadPRFacts accepted mismatched persisted URL")
	}
}

func TestFindOpenPRFactsReturnsEveryPagedCandidateAndAcceptsPushURL(t *testing.T) {
	const pageSize = 100
	first := make([]string, pageSize)
	for i := range first {
		first[i] = strings.Replace(azFacts42, `"pullRequestId":42`, fmt.Sprintf(`"pullRequestId":%d`, i+1), 1)
	}
	last := strings.Replace(azFacts42, `"pullRequestId":42`, `"pullRequestId":101`, 1)
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --top 100 --skip 0 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json":   {stdout: "[" + strings.Join(first, ",") + "]"},
		"az repos pr list --source-branch feature --status active --top 100 --skip 100 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: "[" + last + "]"},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json":                                      {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	})
	facts, err := h.FindOpenPRFacts(context.Background(), "git@ssh.dev.azure.com:v3/MyOrg/MyProject/MyRepo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 101 || facts[0].PR.Number != "1" || facts[100].PR.Number != "101" || facts[100].BaseBranch != "develop" {
		t.Fatalf("candidate list = %+v", facts)
	}
}

func TestReadPRFactsRejectsStalePRMergeSnapshot(t *testing.T) {
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"1123456789abcdef0123456789abcdef01234567"}]`},
	})
	if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
		t.Fatal("ReadPRFacts accepted stale lastMergeSourceCommit")
	}
}

func TestReadPRFactsUsesForkSourceRepository(t *testing.T) {
	fork := strings.Replace(azFacts42, `"repository":`, `"forkSource":{"repository":{"name":"fork","project":{"name":"team"}}},"repository":`, 1)
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                  {stdout: fork},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project team --repository fork --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	})
	facts, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != "https://dev.azure.com/myorg/team/_git/fork" {
		t.Fatalf("source repository = %q", facts.SourceRepository)
	}
}

func TestReadPRFactsRejectsMissingOrAmbiguousLiveSourceRef(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", `[]`},
		{"prefix only", `[{"name":"refs/heads/feature/other","objectId":"` + azFactsSHA + `"}]`},
		{"duplicate", `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"},{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
				"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: tc.body},
			})
			if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
				t.Fatal("ReadPRFacts accepted missing or ambiguous live source ref")
			}
		})
	}
}

func TestFindOpenPRFactsRejectsCorruptAndDuplicateCandidates(t *testing.T) {
	command := "az repos pr list --source-branch feature --status active --top 100 --skip 0 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json"
	for _, tc := range []struct{ name, body string }{
		{"null", "null"},
		{"object", azFacts42},
		{"missing head", "[" + strings.Replace(azFacts42, azFactsSHA, "", 1) + "]"},
		{"wrong source branch", "[" + strings.Replace(azFacts42, "refs/heads/feature", "refs/heads/other", 1) + "]"},
		{"duplicate", "[" + azFacts42 + "," + azFacts42 + "]"},
		{"foreign target", "[" + azFacts42 + "," + strings.Replace(azFacts42, `"name":"myrepo"`, `"name":"other"`, 1) + "]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHost(map[string]azdoTestResponse{command: {stdout: tc.body}})
			if _, err := h.FindOpenPRFacts(context.Background(), webPRURL(testOrg, testProject, testRepo, "", ""), "feature"); err == nil {
				t.Fatal("FindOpenPRFacts accepted corrupt or ambiguous result")
			}
		})
	}
}

func TestFindOpenPRFactsAcceptsProvenEmptyAndRejectsForeignSource(t *testing.T) {
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --top 100 --skip 0 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: "[]"},
	})
	facts, err := h.FindOpenPRFacts(context.Background(), webPRURL(testOrg, testProject, testRepo, "", ""), "feature")
	if err != nil || len(facts) != 0 {
		t.Fatalf("empty result = %+v, %v", facts, err)
	}
	if _, err := h.FindOpenPRFacts(context.Background(), "https://dev.azure.com/other/myproject/_git/myrepo", "feature"); err == nil {
		t.Fatal("FindOpenPRFacts accepted foreign source repository")
	}
}

func TestReadPRFactsMergedCommitFailsClosed(t *testing.T) {
	const mergeSHA = "abcdef0123456789abcdef0123456789abcdef01"
	merged := strings.Replace(azFacts42, `"status":"active"`, `"status":"completed"`, 1)
	for _, tc := range []struct {
		name, sha       string
		omit, wantError bool
	}{
		{"valid", mergeSHA, false, false},
		{"missing", "", true, true},
		{"empty", "", false, true},
		{"short", "abcdef", false, true},
		{"nonhex", strings.Repeat("z", 40), false, true},
		{"whitespace", " " + mergeSHA, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := merged
			if !tc.omit {
				payload = strings.Replace(payload, `{`, `{`+fmt.Sprintf(`"lastMergeCommit":{"commitId":%q},`, tc.sha), 1)
			}
			host := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json": {stdout: payload},
			})
			facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted merged PR without a valid merge commit: %+v", facts)
				}
				return
			}
			if err != nil || facts.State != scm.PRStateMerged || facts.MergeCommitSHA != mergeSHA {
				t.Fatalf("merged facts = %+v, %v", facts, err)
			}
		})
	}
}
