package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func unboundCheckReadFixture(t *testing.T, ag agent.Agent, status string, rewrite bool) (*pipeline.StepContext, scm.Host, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "set-url", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	foreign := gitCmd(t, dir, "commit-tree", gitCmd(t, dir, "rev-parse", "HEAD^{tree}"), "-p", headSHA, "-m", "rewritten outside the run")
	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"build","state":"`+status+`"}]}`)
	api.pipelinesJSON = `{"values":[]}`
	if rewrite {
		api.beforeStatuses = func() {
			api.beforeStatuses = nil
			gitCmd(t, dir, "push", "origin", foreign+":refs/heads/feature")
		}
	}
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	client, err := bitbucket.NewClientFromEnv(sctx.Env)
	if err != nil {
		t.Fatal(err)
	}
	host := bitbucket.NewHost(client, bitbucket.RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	return sctx, host, foreign
}

func TestCIStep_UnboundApprovalReadPreservesForeignHeadOverride(t *testing.T) {
	t.Parallel()
	for _, rewrite := range []bool{false, true} {
		name := "owned head passes cleanly"
		if rewrite {
			name = "rewrite inside read requires override"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sctx, _, foreign := unboundCheckReadFixture(t, &mockAgent{name: "test"}, "SUCCESSFUL", rewrite)
			reason, err := (&CIStep{}).VerifyApprovalOverride(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				if !strings.Contains(reason, "not for the commit being delivered") || !strings.Contains(reason, shortSHA(foreign)) {
					t.Fatalf("approval lost the unvalidated head: %q", reason)
				}
			} else if reason != "" {
				t.Fatalf("owned green head received an override: %q", reason)
			}
		})
	}
}

func TestCIStep_UnboundRepairReadRejectsForeignFailureSnapshot(t *testing.T) {
	t.Parallel()
	for _, rewrite := range []bool{false, true} {
		name := "owned failure reaches fixer"
		if rewrite {
			name = "foreign failure never reaches fixer"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"summary":"external failure","code_change_needed":false}`)}, nil
			}}
			sctx, host, _ := unboundCheckReadFixture(t, ag, "FAILED", rewrite)
			sctx.PreviousFindings = ciGateFindingsJSON("build")
			outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "42", URL: *sctx.Run.PRURL})
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				if outcome != nil || len(ag.calls) != 0 {
					t.Fatalf("foreign failure triggered repair: outcome=%#v, agent calls=%d", outcome, len(ag.calls))
				}
			} else if len(ag.calls) != 1 || outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("owned failure did not reach fixer: outcome=%#v, agent calls=%d", outcome, len(ag.calls))
			}
		})
	}
}
