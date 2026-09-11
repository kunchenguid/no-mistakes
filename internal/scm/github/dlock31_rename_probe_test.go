package github

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// TestDLOCK31RenameIdentity exercises discovery, never a live gh process.
func TestDLOCK31RenameIdentity(t *testing.T) {
	const oldRepo = "Main-Jammers-Incorporated/deadlock-skin-marketplace"
	const canonicalRepo = "Main-Jammers-Incorporated/soul-swap"
	const repositoryID = 1345880885
	for _, tc := range []struct {
		name string
		id   int
	}{
		{"same_repository", repositoryID},
		{"different_repository", repositoryID + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := map[string]githubTestResponse{
				"gh pr list --head fm/soul-swap-pr7-port --repo " + oldRepo + " --state open --json number,url,baseRefName": {
					stdout: `[{"number":23,"url":"https://github.com/` + canonicalRepo + `/pull/23","baseRefName":"main"}]`,
				},
				"gh api --hostname github.com repos/" + oldRepo: {
					stdout: fmt.Sprintf(`{"id":%d,"full_name":%q}`, repositoryID, canonicalRepo),
				},
				"gh api --hostname github.com repos/" + canonicalRepo: {
					stdout: fmt.Sprintf(`{"id":%d,"full_name":%q}`, tc.id, canonicalRepo),
				},
			}
			host := New(githubTestCmdFactory(responses), func() bool { return true }, "github.com", oldRepo)
			pr, err := host.FindPR(context.Background(), "fm/soul-swap-pr7-port", "")
			if tc.id != repositoryID {
				if err == nil || pr != nil || !strings.Contains(err.Error(), "repository") {
					t.Fatalf("different repository accepted or wrong failure: PR=%+v error=%v", pr, err)
				}
				return
			}
			if err != nil || pr == nil {
				t.Fatalf("same GitHub ID %d rename rejected: %v", repositoryID, err)
			}
			if pr.Number != "23" || pr.URL != "https://github.com/"+canonicalRepo+"/pull/23" {
				t.Fatalf("wrong discovered PR: %+v", pr)
			}
		})
	}
}

func TestDLOCK31RenameFailsClosed(t *testing.T) {
	const oldRepo = "owner/old"
	const newRepo = "owner/new"
	for _, tc := range []struct {
		name, response, host string
		code                 int
	}{
		{name: "missing_id", response: `{"full_name":"owner/new"}`},
		{name: "zero_id", response: `{"id":0,"full_name":"owner/new"}`},
		{name: "fractional_id", response: `{"id":1.5,"full_name":"owner/new"}`},
		{name: "string_id", response: `{"id":"1345880885","full_name":"owner/new"}`},
		{name: "malformed", response: `not json`},
		{name: "wrong_canonical_name", response: `{"id":1345880885,"full_name":"owner/elsewhere"}`},
		{name: "auth_failure", code: 1},
		{name: "different_host", host: "attacker.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostname := "github.com"
			if tc.host != "" {
				hostname = tc.host
			}
			factory := githubTestCmdFactory(map[string]githubTestResponse{
				"gh pr list --head feature --repo " + oldRepo + " --state open --json number,url,baseRefName": {stdout: `[{"number":23,"url":"https://` + hostname + `/owner/new/pull/23"}]`},
				"gh api --hostname github.com repos/" + oldRepo:                                               {stdout: tc.response, code: tc.code},
				"gh api --hostname github.com repos/" + newRepo:                                               {stdout: `{"id":1345880885,"full_name":"owner/new"}`},
			})
			calls := 0
			recorded := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls++
				return factory(ctx, name, args...)
			}
			pr, err := New(recorded, func() bool { return true }, "github.com", oldRepo).FindPR(context.Background(), "feature", "")
			if err == nil || pr != nil {
				t.Fatalf("unverified rename accepted: PR=%+v error=%v", pr, err)
			}
			if tc.host != "" && calls != 1 {
				t.Fatalf("cross-host URL triggered identity lookup: calls=%d", calls)
			}
		})
	}
}
