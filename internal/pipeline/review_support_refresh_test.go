package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type refreshingTestOwner struct {
	calls int
	claim types.Finding
}

func (s *refreshingTestOwner) Name() types.StepName                         { return types.StepTest }
func (s *refreshingTestOwner) Execute(c *StepContext) (*StepOutcome, error) { return s.proof(c) }
func (s *refreshingTestOwner) RefreshReviewSupport(c *StepContext) (*StepOutcome, error) {
	s.calls++
	c.LogFile(strings.Repeat("x", 1100000))
	c.Log("bounded configured-command output")
	return s.proof(c)
}
func (s *refreshingTestOwner) proof(c *StepContext) (*StepOutcome, error) {
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		return nil, err
	}
	zero := 0
	item := s.claim
	support := *item.Support
	item.Support = &support
	item.ID = types.ReviewSupportClaimID(s.claim)
	item.Category = types.FindingCategoryReviewSupportResolved
	item.Severity = types.FindingSeverityInfo
	item.Support.OwnerResult = &types.FindingOwnerResult{ReviewFindingID: s.claim.ID, HeadSHA: receipt.LocalHeadSHA, TargetSHA: receipt.TargetSHA, DiffDigest: receipt.DiffDigest, Generation: receipt.Generation, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero}
	encoded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	return &StepOutcome{Findings: encoded}, err
}

func TestExecutorRefreshesPendingTestProofAfterForwardDocumentCommit(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{LocalHeadSHA: head, TargetBranch: "main", TargetSHA: head, MergeBaseSHA: head, DiffDigest: fmt.Sprintf("%064d", 1)}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{ID: "pending-test", Category: types.FindingCategoryReviewSupportPending, Severity: types.FindingSeverityInfo, Action: types.ActionNoOp, Description: "historical configured test failure", Support: &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "trusted test"}}}
	encoded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	owner := &refreshingTestOwner{claim: claim}
	executor := NewExecutor(database, p, &config.Config{}, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(c *StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: encoded, ReviewApprovedHeadSHA: c.Run.HeadSHA}, nil
		}}, owner,
		&adaptiveCallStep{name: types.StepDocument, fn: func(c *StepContext) (*StepOutcome, error) {
			writeTestFile(t, dir, "docs.md", "updated docs\n")
			execGit(t, dir, "add", "docs.md")
			execGit(t, dir, "commit", "-m", "document")
			advanced, err := git.HeadSHA(c.Ctx, dir)
			if err != nil {
				return nil, err
			}
			c.Run.HeadSHA = advanced
			if err := c.DB.UpdateRunHeadSHA(c.Run.ID, advanced); err != nil {
				return nil, err
			}
			receipt.LocalHeadSHA = advanced
			receipt.DiffDigest = fmt.Sprintf("%064d", 2)
			_, err = c.DB.AdvanceRunPRContext(c.Run.ID, receipt)
			return &StepOutcome{}, err
		}}, newPassStep(types.StepCI)}, func(event ipc.Event) {
		if event.Type == ipc.EventLogChunk && event.Content != nil && len(*event.Content) > 65536 {
			t.Errorf("refresh emitted oversized log chunk: %d bytes", len(*event.Content))
		}
	})
	if err := executor.Execute(context.Background(), run, repo, dir); err != nil {
		t.Fatalf("forward edit could not finish with fresh proof: %v", err)
	}
	if owner.calls != 1 {
		t.Fatalf("fresh owner executions=%d, want 1", owner.calls)
	}
	log, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "test.log"))
	if err != nil || !strings.Contains(string(log), strings.Repeat("x", 1100000)) {
		t.Fatalf("full configured-command output missing from durable log: %v", err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatal(err)
	}
}
