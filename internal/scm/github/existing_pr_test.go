package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const explicitTestURL = "https://github.com/upstream/widgets/pull/168"
const explicitTestHead = "0123456789012345678901234567890123456789"

func explicitTestResponse() map[string]any {
	return map[string]any{
		"number": 168, "html_url": explicitTestURL, "state": "open", "merged": false,
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "upstream/widgets", "html_url": "https://github.com/upstream/widgets"}},
		"head": map[string]any{"ref": "fm/account-context", "sha": explicitTestHead, "repo": map[string]any{"full_name": "contributor/widgets", "html_url": "https://github.com/contributor/widgets"}},
	}
}

func TestValidateExistingPRIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		mutate         func(map[string]any)
		source, branch string
		code           int
		wantError      bool
	}{
		{name: "fork upstream exact match"},
		{name: "different requested repository", mutate: func(p map[string]any) {
			p["base"].(map[string]any)["repo"].(map[string]any)["full_name"] = "other/widgets"
		}, wantError: true},
		{name: "different PR", mutate: func(p map[string]any) { p["number"] = 169 }, wantError: true},
		{name: "different URL", mutate: func(p map[string]any) { p["html_url"] = "https://github.com/contributor/widgets/pull/168" }, wantError: true},
		{name: "different source repo", source: "imposter/widgets", wantError: true},
		{name: "same owner different source repo", source: "contributor/other", wantError: true},
		{name: "different source ref", branch: "fm/other", wantError: true},
		{name: "source ref is case sensitive", branch: "fm/Account-context", wantError: true},
		{name: "missing head", mutate: func(p map[string]any) { delete(p["head"].(map[string]any), "sha") }, wantError: true},
		{name: "deleted source repo", mutate: func(p map[string]any) { p["head"].(map[string]any)["repo"] = nil }, wantError: true},
		{name: "closed", mutate: func(p map[string]any) { p["state"] = "closed" }, wantError: true},
		{name: "merged", mutate: func(p map[string]any) { p["merged"] = true }, wantError: true},
		{name: "unavailable", code: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := explicitTestResponse()
			if tc.mutate != nil {
				tc.mutate(p)
			}
			payload, _ := json.Marshal(p)
			h := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --hostname github.com repos/upstream/widgets/pulls/168": {stdout: string(payload), code: tc.code},
			}), nil, "github.com", "upstream/widgets")
			source, branch := tc.source, tc.branch
			if source == "" {
				source = "contributor/widgets"
			}
			if branch == "" {
				branch = "fm/account-context"
			}
			pr, err := h.ValidateExistingPRIdentity(context.Background(), explicitTestURL, source, branch)
			if (err != nil) != tc.wantError {
				t.Fatalf("PR=%+v error=%v", pr, err)
			}
			if !tc.wantError && (pr.URL != explicitTestURL || pr.BaseBranch != "main" || pr.HeadSHA != explicitTestHead) {
				t.Fatalf("unexpected PR: %+v", pr)
			}
		})
	}
}

// A renamed or transferred repository answers the old URL through GitHub's
// redirect under its new name. The association still refuses it - the stored
// URL derives the integration fetch URL and ref namespace - but the refusal
// names the canonical URL so the operator can re-run with it.
func TestExistingPRIdentityNamesTheCanonicalURL(t *testing.T) {
	t.Parallel()
	const canonical = "https://github.com/newowner/widgets/pull/168"
	p := explicitTestResponse()
	p["html_url"] = canonical
	base := p["base"].(map[string]any)["repo"].(map[string]any)
	base["full_name"] = "newowner/widgets"
	base["html_url"] = "https://github.com/newowner/widgets"
	payload, _ := json.Marshal(p)
	h := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --hostname github.com repos/upstream/widgets/pulls/168": {stdout: string(payload)},
	}), nil, "github.com", "upstream/widgets")
	pr, err := h.ValidateExistingPRIdentity(context.Background(), explicitTestURL, "contributor/widgets", "fm/account-context")
	if err == nil {
		t.Fatalf("accepted a non-canonical URL: %+v", pr)
	}
	if !strings.Contains(err.Error(), canonical) {
		t.Fatalf("refusal did not name the canonical URL: %v", err)
	}
}

func TestExistingPRTargetRejectsAmbiguousURLs(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"168", "https://github.com/upstream/widgets/pull/168/", "http://github.com/upstream/widgets/pull/168", "https://github.com.evil.test/upstream/widgets/pull/168", "https://user@github.com/upstream/widgets/pull/168", explicitTestURL + "?x=y", explicitTestURL + "#issuecomment-1", "https://github.com/upstream/widgets/pull/0168", "https://github.com/upstream/widgets/pull/0", "https://github.com/upstream%2fwidgets/pull/168"} {
		if _, _, err := ExistingPRTarget(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

// Counterfactual: changing just the registered routing (parent + fork) finds
// the upstream PR. Disconfirming case: same-repository origin association works.
// These are representative fixtures, not observations from a live forge.
func TestPRDiscoveryForkOriginCausalBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name, repo, fork, response string
		want                       bool
	}{
		{"fork origin masks upstream PR", "contributor/widgets", "", "[]", false},
		{"parent plus fork finds upstream", "upstream/widgets", "contributor/widgets", `[{"number":168,"url":"https://github.com/upstream/widgets/pull/168","baseRefName":"main","headRefName":"fm/account-context","headRepositoryOwner":{"login":"contributor"}}]`, true},
		{"ordinary same repository works", "upstream/widgets", "", `[{"number":168,"url":"https://github.com/upstream/widgets/pull/168","baseRefName":"main"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := "number,url,baseRefName"
			if tc.fork != "" {
				fields += ",headRefName,headRepositoryOwner"
			}
			command := "gh pr list --head fm/account-context --repo " + tc.repo + " --state open --json " + fields
			h := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{command: {stdout: tc.response}}), nil, "github.com", tc.repo, tc.fork, false)
			pr, err := h.FindPR(ctx, "fm/account-context", "")
			if err != nil || (pr != nil) != tc.want {
				t.Fatalf("PR=%+v err=%v", pr, err)
			}
		})
	}
}
