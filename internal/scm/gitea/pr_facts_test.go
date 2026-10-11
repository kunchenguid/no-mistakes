package gitea

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const giteaFactsSHA = "0123456789abcdef0123456789abcdef01234567"
const giteaFactsJSON = `{"number":42,"html_url":"https://gitea.example.com/owner/repo/pulls/42","state":"open","merged":false,"head":{"ref":"feature","sha":"` + giteaFactsSHA + `","repo":{"full_name":"fork/repo"}},"base":{"ref":"develop"}}`

func factsHost(responses map[string]giteaTestResponse) *Host {
	return New(giteaTestCmdFactory(responses), nil, "gitea.example.com", "work", "owner/repo")
}

func TestReadPRFactsBindsRecordedIdentityAndLiveFacts(t *testing.T) {
	host := factsHost(map[string]giteaTestResponse{
		"tea api --login work /repos/owner/repo/pulls/42": {stdout: giteaFactsJSON},
	})
	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://gitea.example.com/owner/repo/pulls/42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.PR.Number != "42" || facts.PR.URL != "https://gitea.example.com/owner/repo/pulls/42" ||
		facts.State != scm.PRStateOpen || facts.SourceRepository != "fork/repo" || facts.SourceBranch != "feature" ||
		facts.HeadSHA != giteaFactsSHA || facts.PR.HeadSHA != giteaFactsSHA || facts.BaseBranch != "develop" || facts.PR.BaseBranch != "develop" {
		t.Fatalf("incomplete PR facts: %+v", facts)
	}
}

func TestReadPRFactsClosedMergedAndRejectsMismatchedIdentity(t *testing.T) {
	merged := strings.Replace(giteaFactsJSON, `"state":"open","merged":false`, `"state":"closed","merged":true,"merge_commit_sha":"abcdef0123456789abcdef0123456789abcdef01"`, 1)
	host := factsHost(map[string]giteaTestResponse{
		"tea api --login work /repos/owner/repo/pulls/42": {stdout: merged},
	})
	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
	if err != nil || facts.State != scm.PRStateMerged {
		t.Fatalf("merged PR facts = %+v, %v", facts, err)
	}
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://gitea.example.com/owner/repo/pulls/99"}); err == nil {
		t.Fatal("accepted a different recorded PR URL")
	}
}

func TestReadPRFactsFailsClosedOnIncompleteOrMalformedData(t *testing.T) {
	cases := map[string]string{
		"missing source":    strings.Replace(giteaFactsJSON, `"full_name":"fork/repo"`, `"full_name":""`, 1),
		"missing sha":       strings.Replace(giteaFactsJSON, giteaFactsSHA, "", 1),
		"wrong number":      strings.Replace(giteaFactsJSON, `"number":42`, `"number":43`, 1),
		"wrong url":         strings.Replace(giteaFactsJSON, `/pulls/42`, `/pulls/43`, 1),
		"incoherent merged": strings.Replace(giteaFactsJSON, `"merged":false`, `"merged":true`, 1),
		"null":              `null`,
		"malformed":         `{`,
		"trailing response": giteaFactsJSON + ` {}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			host := factsHost(map[string]giteaTestResponse{
				"tea api --login work /repos/owner/repo/pulls/42": {stdout: payload},
			})
			if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
				t.Fatal("accepted incomplete or malformed PR facts")
			}
		})
	}
}

func TestFindOpenPRFactsPaginatesAndReturnsEveryExactSourceCandidate(t *testing.T) {
	other := strings.Replace(giteaFactsJSON, `"number":42`, `"number":43`, 1)
	other = strings.Replace(other, `/pulls/42`, `/pulls/43`, 1)
	second := strings.Replace(giteaFactsJSON, `"number":42`, `"number":44`, 1)
	second = strings.Replace(second, `/pulls/42`, `/pulls/44`, 1)
	other = strings.Replace(other, `"full_name":"fork/repo"`, `"full_name":"else/repo"`, 1)
	host := factsHost(map[string]giteaTestResponse{
		"tea api --login work /repos/owner/repo/pulls?state=open&sort=oldest&limit=50&page=1": {stdout: `[` + giteaFactsJSON + `,` + other + `]`},
		"tea api --login work /repos/owner/repo/pulls?state=open&sort=oldest&limit=50&page=2": {stdout: `[` + second + `]`},
		"tea api --login work /repos/owner/repo/pulls?state=open&sort=oldest&limit=50&page=3": {stdout: `[]`},
	})
	facts, err := host.FindOpenPRFacts(context.Background(), "Fork/Repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0].PR.Number != "42" || facts[1].PR.Number != "44" {
		t.Fatalf("candidates = %+v, want both exact-source PRs", facts)
	}
}

func TestFindOpenPRFactsFailsClosedOnMalformedUnrelatedCandidateAndDuplicate(t *testing.T) {
	bad := strings.Replace(giteaFactsJSON, `"full_name":"fork/repo"`, `"full_name":""`, 1)
	for name, first := range map[string]string{
		"incomplete unrelated": `[` + bad + `]`,
		"duplicate":            `[` + giteaFactsJSON + `,` + giteaFactsJSON + `]`,
		"null list":            `null`,
		"non-list":             `{}`,
		"trailing response":    `[] {}`,
	} {
		t.Run(name, func(t *testing.T) {
			host := factsHost(map[string]giteaTestResponse{
				"tea api --login work /repos/owner/repo/pulls?state=open&sort=oldest&limit=50&page=1": {stdout: first},
				"tea api --login work /repos/owner/repo/pulls?state=open&sort=oldest&limit=50&page=2": {stdout: `[]`},
			})
			if _, err := host.FindOpenPRFacts(context.Background(), "fork/repo", "feature"); err == nil {
				t.Fatal("accepted incomplete or ambiguous list")
			}
		})
	}
}

func TestFindOpenPRFactsAcceptsProvenEmptyList(t *testing.T) {
	host := factsHost(map[string]giteaTestResponse{
		"tea api --login work /repos/owner/repo/pulls?state=open&sort=oldest&limit=50&page=1": {stdout: `[]`},
	})
	facts, err := host.FindOpenPRFacts(context.Background(), "fork/repo", "feature")
	if err != nil || len(facts) != 0 {
		t.Fatalf("empty list = %+v, %v", facts, err)
	}
}

func TestReadPRFactsMergedCommitFailsClosed(t *testing.T) {
	const mergeSHA = "abcdef0123456789abcdef0123456789abcdef01"
	merged := strings.Replace(giteaFactsJSON, `"state":"open","merged":false`, `"state":"closed","merged":true`, 1)
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
				payload = strings.Replace(payload, `{`, `{`+fmt.Sprintf(`"merge_commit_sha":%q,`, tc.sha), 1)
			}
			host := factsHost(map[string]giteaTestResponse{
				"tea api --login work /repos/owner/repo/pulls/42": {stdout: payload},
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
