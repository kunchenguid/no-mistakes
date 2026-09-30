package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type contractSequenceStep struct {
	name types.StepName
	fn   func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (s contractSequenceStep) Name() types.StepName { return s.name }
func (s contractSequenceStep) Execute(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return s.fn(c)
}

func TestTestRepairCommitRestartsThroughReview(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{Test: "test -f fixed.txt"})
	c.Repo.UpstreamURL = dir
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	review := contractSequenceStep{types.StepReview, func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		calls++
		f := types.Findings{}
		if calls == 1 {
			f.Items = []types.Finding{pendingReviewTestClaim(c.Config.Commands.Test)}
		}
		raw, _ := types.MarshalFindingsJSON(f)
		return &pipeline.StepOutcome{Findings: raw, ReviewApprovedHeadSHA: c.Run.HeadSHA}, nil
	}}
	c.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, o agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(o.Prompt, "Fix the failing tests") {
			if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("fixed\n"), 0644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: []byte(`{"summary":"fix the test"}`)}, nil
		}
		raw, _ := json.Marshal(cleanReviewFindings())
		return &agent.Result{Output: raw}, nil
	}}
	var ex *pipeline.Executor
	fixes := 0
	ex = pipeline.NewExecutor(c.DB, p, c.Config, c.Agent, []pipeline.Step{review, &TestStep{}}, func(e ipc.Event) {
		if e.StepName != nil && *e.StepName == types.StepTest && e.Status != nil && (*e.Status == string(types.StepStatusAwaitingApproval) || *e.Status == string(types.StepStatusFixReview)) {
			fixes++
			if fixes == 1 {
				if err := ex.Respond(types.StepTest, types.ActionFix, nil); err != nil {
					t.Error(err)
				}
			}
		}
	})
	ex.SetPRContextGuard(GuardPRContext)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := ex.Execute(ctx, c.Run, c.Repo, dir)
	if err != nil || calls < 2 {
		t.Fatalf("Test repair failed before requested Review restart: review calls=%d, fixes=%d, error=%v", calls, fixes, err)
	}
}

func TestSecondAutomaticReviewFixUsesAdvancedReceipt(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	c.Repo.UpstreamURL = dir
	c.Config.AutoFix = config.AutoFix{Review: 2}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	reviews, fixes := 0, 0
	c.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, o agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(o.Prompt, "Investigate previous review findings") {
			fixes++
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte(fmt.Sprintf("feature code\nfix %d\n", fixes)), 0644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: []byte(`{"summary":"fix reported issue"}`)}, nil
		}
		reviews++
		f := cleanReviewFindings()
		if reviews < 3 {
			f.Items = []types.Finding{{ID: "review-1", Severity: types.FindingSeverityError, Action: types.ActionAutoFix, File: "feature.txt", Line: 1, Description: "still needs fix", Support: &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "feature.txt", Line: 1, Quote: "feature code"}}}}
		}
		raw, _ := json.Marshal(f)
		return &agent.Result{Output: raw}, nil
	}}
	ex := pipeline.NewExecutor(c.DB, p, c.Config, c.Agent, []pipeline.Step{&ReviewStep{}}, nil)
	ex.SetPRContextGuard(GuardPRContext)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := ex.Execute(ctx, c.Run, c.Repo, dir)
	if err != nil || fixes != 2 {
		t.Fatalf("second Review auto-fix blocked by stale in-step receipt: reviews=%d fixes=%d error=%v", reviews, fixes, err)
	}
}

func TestNamedCIProofRejectsRetainedGitLabPipeline(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	c.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitLab}
	candidate, err := readPRComparison(c, pipeline.PRTargetSelection{TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	candidate.PRURL = "https://gitlab.example.com/test/repo/-/merge_requests/42"
	candidate.SourceRepo = "test/repo"
	candidate.SourceBranch = "feature"
	candidate.ForgeHeadSHA = head
	if _, err = c.DB.BindRunPRContext(c.Run.ID, candidate, types.StepReview); err != nil {
		t.Fatal(err)
	}
	c.PRContext, err = c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	review, err := c.DB.InsertStepResult(c.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	claim := pendingReviewCIClaim("gitlab-job:771", head)
	raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err = c.DB.SetStepFindings(review.ID, raw); err != nil {
		t.Fatal(err)
	}
	if err = c.DB.CompleteReviewStep(review.ID, c.Run.ID, head, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	old := strings.Repeat("a", 40)
	facts := fmt.Sprintf(`{"iid":42,"web_url":%q,"state":"closed","source_project_id":7,"target_project_id":7,"source_branch":"feature","target_branch":"main","sha":%q}`, candidate.PRURL, head)
	responses := map[string]string{
		"glab api --hostname gitlab.example.com --method GET projects/test%2Frepo/merge_requests/42": facts,
		"glab api --hostname gitlab.example.com --method GET projects/7":                             `{"id":7,"path_with_namespace":"test/repo"}`,
		"glab mr view 42 --output json":                              fmt.Sprintf(`{"sha":%q,"head_pipeline":{"id":77,"sha":%q}}`, head, old),
		"glab api --paginate projects/test%2Frepo/pipelines/77/jobs": fmt.Sprintf(`[{"id":771,"name":"test","status":"success","commit":{"id":%q}}]`, old),
	}
	host := gitlab.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := name + " " + strings.Join(args, " ")
		if key == "glab ci status --mr 42 --output json" {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "echo 'unknown flag: --mr' >&2; exit 1")
		}
		value, ok := responses[key]
		if !ok {
			t.Errorf("unexpected provider read: %s", key)
		}
		return exec.CommandContext(ctx, "/usr/bin/printf", "%s", value)
	}, func() bool { return true }, "gitlab.example.com", "test/repo")
	results, err := resolveCIReviewSupport(c, host, &scm.PR{Number: "42", URL: candidate.PRURL})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionUnresolved {
		t.Fatalf("old provider pipeline accepted as exact current-head proof: %+v", results)
	}
}
