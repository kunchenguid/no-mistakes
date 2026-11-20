package steps

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type retryStep struct {
	name types.StepName
	fn   func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (s *retryStep) Name() types.StepName { return s.name }

func (s *retryStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.fn == nil {
		return &pipeline.StepOutcome{}, nil
	}
	return s.fn(sctx)
}

// An operator approves a Test no-go with a reason, the run pushes, and the PR
// step then fails. The delivery retry carries the approved Test step, which
// keeps none of its source's findings, and its PR body must still list that
// Test exception with the operator's reason.
func TestDeliveryRetryKeepsTheApprovedTestExceptionInTheAttestation(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	const head, reason = "1111111111111111111111111111111111111111", "provider sandbox is down; refund path reviewed by hand"
	noGo := `{"findings":[{"id":"test-1","severity":"error","description":"refund fails"}],"verdict":"no-go"}`
	names := []types.StepName{types.StepReview, types.StepTest, types.StepPush, types.StepPR, types.StepCI}

	source, err := database.InsertRun(repo.ID, "feature", head, "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		sr, err := database.InsertStepResult(source.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		switch name {
		case types.StepTest:
			if err := database.ParkStepForApproval(source.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 10, &noGo); err != nil {
				t.Fatal(err)
			}
			if err := database.SetTestApprovalReason(sr.ID, reason); err != nil {
				t.Fatal(err)
			}
			err = database.CompleteStep(sr.ID, 0, 10, "")
		case types.StepPR:
			err = database.FailStep(sr.ID, "gh: GraphQL: Bad Gateway", 10)
		case types.StepCI:
		default:
			err = database.CompleteStep(sr.ID, 0, 10, "")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunErrorStatus(source.ID, "step pr failed", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(source.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}

	continuation, err := database.InsertRun(repo.ID, "feature", head, "base")
	if err != nil {
		t.Fatal(err)
	}
	var pipelineMD string
	steps := make([]pipeline.Step, len(names))
	for i, name := range names {
		step := &retryStep{name: name}
		if name == types.StepPR {
			step.fn = func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				pipelineMD, _, _, _ = (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
				return &pipeline.StepOutcome{}, nil
			}
		}
		steps[i] = step
	}
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), &config.Config{}, nil, steps, nil)
	exec.SetRetrySource(source.ID)
	if err := exec.Execute(context.Background(), continuation, repo, t.TempDir()); err != nil {
		t.Fatalf("execute retry: %v", err)
	}
	carried, err := database.GetStepsByRun(continuation.ID)
	if err != nil || carried[1].StepName != types.StepTest || carried[1].CarriedFrom == nil || carried[1].FindingsJSON != nil {
		t.Fatalf("Test was not carried without findings, so this does not exercise the retry: %+v %v", carried, err)
	}

	got := parsePipelineAttestationForTest(t, pipelineMD).Exceptions
	want := pipelineAttestationException{Step: types.StepTest, Kind: "test-exception", Condition: "live validation verdict: no-go", Reason: reason}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("retried PR attestation exceptions = %+v, want [%+v]\n%s", got, want, pipelineMD)
	}
}
