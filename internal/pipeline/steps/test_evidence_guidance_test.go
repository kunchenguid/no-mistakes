package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// The emitted Test prompt must promise an evidence branch only when the PR
// step can publish it, and must keep evidence out of the code branch.
func TestTestStep_EvidencePublicationFollowsTheForgeAndSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remote          string
		provider              scm.Provider
		attach, store, branch bool
	}{
		{"GitLab default attachments", "https://gitlab.com/team/widgets.git", scm.ProviderGitLab, true, false, false},
		{"GitLab branch and implied attachments", "https://gitlab.com/team/widgets.git", scm.ProviderGitLab, false, true, true},
		{"GitLab publication disabled", "https://gitlab.com/team/widgets.git", scm.ProviderGitLab, false, false, false},
		{"GitHub branch and attachments", "https://github.com/team/widgets.git", scm.ProviderGitHub, true, true, true},
		{"GHES has no branch links", "https://ghe.example/team/widgets.git", scm.ProviderGitHub, true, true, false},
		{"forge profile owns provider", "git@gitlab-alias:team/widgets.git", scm.ProviderGitLab, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence","artifacts":[],"scenarios":[{"name":"user sees the change","result":"pass","live":true,"evidence":"manual evidence check","reason":""}],"verdict":"go"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Repo.UpstreamURL = tc.remote
			sctx.ForgeContext = &forgecontext.Context{Provider: tc.provider, Host: scm.ExtractHost(tc.remote)}
			sctx.Config.Test.Evidence = config.Evidence{AttachMedia: tc.attach, StoreInRepo: tc.store, Branch: "team/ci/evidence"}
			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			prompt := ag.calls[0].Prompt
			if strings.Contains(prompt, "committed and pushed automatically") {
				t.Fatal("prompt promised to commit evidence to the code branch")
			}
			if strings.Contains(prompt, "GitLab project uploads") || strings.Contains(prompt, "GitHub user-attachments") {
				t.Fatal("default evidence guidance gained an unnecessary media-publication bullet")
			}
			if strings.Contains(prompt, "team/ci/evidence") != tc.branch {
				t.Fatalf("evidence branch promise disagrees with actual link support, want branch=%v:\n%s", tc.branch, prompt)
			}
		})
	}
}
