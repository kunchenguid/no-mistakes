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

// The emitted Test prompt describes opted-in GitLab publication while other
// providers retain their existing evidence-branch guidance.
func TestTestStep_EvidencePublicationFollowsTheForgeAndSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remote          string
		provider              scm.Provider
		attach, store, branch bool
	}{
		{"GitLab opt-in attachments", "https://gitlab.com/team/widgets.git", scm.ProviderGitLab, true, false, false},
		{"GitLab branch and implied attachments", "https://gitlab.com/team/widgets.git", scm.ProviderGitLab, false, true, true},
		{"GitLab publication disabled", "https://gitlab.com/team/widgets.git", scm.ProviderGitLab, false, false, false},
		{"GitHub branch and attachments", "https://github.com/team/widgets.git", scm.ProviderGitHub, true, true, true},
		{"GHES retains legacy branch guidance", "https://ghe.example/team/widgets.git", scm.ProviderGitHub, true, true, true},
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
			sctx.Config.Providers.GitLab.EvidencePublication = tc.provider == scm.ProviderGitLab
			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			prompt := ag.calls[0].Prompt
			if strings.Contains(prompt, "team/ci/evidence") != tc.branch {
				t.Fatalf("evidence branch guidance disagrees with the selected mode, want branch=%v:\n%s", tc.branch, prompt)
			}
		})
	}
}
