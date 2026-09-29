package github

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const testPRFactsJSON = `{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"open","merged":false,"head":{"ref":"feature","sha":"0123456789abcdef0123456789abcdef01234567","repo":{"full_name":"test/repo"}},"base":{"ref":"develop"}}`

func TestReadPRFactsBindsHeadSourceAndLiveBase(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42": {stdout: testPRFactsJSON},
	}), nil, "", "test/repo")

	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.PR.Number != "42" || facts.SourceRepository != "test/repo" || facts.SourceBranch != "feature" || facts.HeadSHA != "0123456789abcdef0123456789abcdef01234567" || facts.BaseBranch != "develop" || facts.State != scm.PRStateOpen {
		t.Fatalf("incomplete PR facts: %+v", facts)
	}
}

func TestFindOpenPRFactsReturnsEveryCandidateForExactSource(t *testing.T) {
	second := strings.ReplaceAll(testPRFactsJSON, `"number":42`, `"number":43`)
	second = strings.ReplaceAll(second, "/pull/42", "/pull/43")
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100 --paginate --slurp": {
			stdout: `[[` + testPRFactsJSON + `,` + second + `]]`,
		},
	}), nil, "", "test/repo")

	facts, err := host.FindOpenPRFacts(context.Background(), "test/repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("FindOpenPRFacts returned %d candidates, want both for ambiguity detection", len(facts))
	}
}

func TestReadPRFactsRejectsWrongIdentityAndMissingHead(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42": {stdout: testPRFactsJSON},
	}), nil, "", "test/repo")
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/99"}); err == nil {
		t.Fatal("accepted a different recorded PR URL")
	}
	missingHead := strings.ReplaceAll(testPRFactsJSON, "0123456789abcdef0123456789abcdef01234567", "")
	host = New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42": {stdout: missingHead},
	}), nil, "", "test/repo")
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
		t.Fatal("accepted a PR without an exact head")
	}
}

func TestFindOpenPRFactsRejectsMalformedLaterPage(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100 --paginate --slurp": {
			stdout: `[[` + testPRFactsJSON + `],null]`,
		},
	}), nil, "", "test/repo")
	if _, err := host.FindOpenPRFacts(context.Background(), "test/repo", "feature"); err == nil {
		t.Fatal("accepted an incomplete paginated PR list")
	}
}
