package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRContextGuardExternalCIOwnerRejectsDifferentPRHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.ExternalCIOwner = types.ExternalCIOwnerControllerShipPR
	selection := pipeline.PRTargetSelection{
		PRURL: "https://github.com/example/repo/pull/7", TargetBranch: "main",
		ForgeHeadSHA: strings.Repeat("a", 40),
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false); err == nil || !strings.Contains(err.Error(), "differs from local head") {
		t.Fatalf("guard error = %v, want exact PR head refusal", err)
	}
}

func TestPRContextGuardRequiresPublishedHeadBeforePRMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prHead   func(*testing.T, string, string, string) string
		wantFail bool
	}{
		{name: "exact head", prHead: func(_ *testing.T, _ string, _ string, head string) string { return head }},
		{name: "older ancestor", prHead: func(_ *testing.T, _ string, base string, _ string) string { return base }, wantFail: true},
		{name: "rebased sibling", prHead: func(t *testing.T, dir, base, _ string) string {
			gitCmd(t, dir, "checkout", "-b", "older-pr-head", base)
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "older PR head")
			oldHead := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "feature")
			return oldHead
		}, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			selection := pipeline.PRTargetSelection{
				PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo", SourceBranch: "feature",
				ForgeHeadSHA: tc.prHead(t, dir, base, head), TargetBranch: "main",
			}
			if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
				t.Fatal(err)
			}
			before, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = guardPRContextWithSelection(sctx, types.StepPR, selection, false)
			if tc.wantFail {
				if err == nil || !strings.Contains(err.Error(), "differs from local head") {
					t.Fatalf("PR mutation guard error = %v, want exact-head refusal", err)
				}
				after, readErr := sctx.DB.GetRunPRContext(sctx.Run.ID)
				if readErr != nil || *after != *before {
					t.Fatalf("PR receipt changed on refusal: before=%+v after=%+v err=%v", before, after, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact published head refused: %v", err)
			}
		})
	}
}

func TestPRContextGuardRejectsUnattachedExistingPRBeforeCreation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	selection := pipeline.PRTargetSelection{
		SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: base, TargetBranch: "main",
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err == nil || !strings.Contains(err.Error(), "differs from local head") {
		t.Fatalf("unattached existing PR guard error = %v, want exact-head refusal", err)
	}
}

func TestPRContextGuardPinsActualTargetAndStopsAfterTargetMoves(t *testing.T) {
	dir, mainSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "develop", mainSHA)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, mainSHA, headSHA, config.Commands{})
	selection := pipeline.PRTargetSelection{TargetBranch: "develop"}
	first, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false)
	if err != nil || first.RestartFrom != "" {
		t.Fatalf("first context = %+v, %v", first, err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt == nil || receipt.TargetBranch != "develop" || receipt.LocalHeadSHA != headSHA || receipt.TargetSHA != mainSHA || receipt.DiffDigest == "" {
		t.Fatalf("incorrect durable comparison: %+v, %v", receipt, err)
	}
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "checkout", "develop")
	if err := os.WriteFile(filepath.Join(dir, "target-move.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target-move.txt")
	gitCmd(t, dir, "commit", "-m", "move target")
	gitCmd(t, dir, "checkout", "feature")
	_, err = guardPRContextWithSelection(sctx, types.StepTest, selection, false)
	if err == nil || !strings.Contains(err.Error(), "start a new run") {
		t.Fatalf("target movement error = %v, want a new run", err)
	}
	updated, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || updated.ReviewApprovedHeadSHA == nil {
		t.Fatalf("completed review history was reset: %+v, %v", updated, err)
	}
	resetStep, err := sctx.DB.GetStepResult(step.ID)
	if err != nil || resetStep.Status != types.StepStatusCompleted {
		t.Fatalf("review step was replayed: %+v, %v", resetStep, err)
	}
}

func TestPRContextGuardAdvancesAfterDocumentEditWithoutReplayingTest(t *testing.T) {
	dir, base, reviewedHead := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, reviewedHead, config.Commands{})
	selection := pipeline.PRTargetSelection{TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "documentation.md"), []byte("documented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "documentation.md")
	gitCmd(t, dir, "commit", "-m", "document change")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx.Run.HeadSHA = newHead
	sctx.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(sctx, types.StepDocument, selection, false)
	if err != nil || decision.RestartFrom != "" {
		t.Fatalf("forward document edit = %+v, %v; want no restart", decision, err)
	}
	stored, err := sctx.DB.GetStepResult(review.ID)
	if err != nil || stored.Status != types.StepStatusCompleted {
		t.Fatalf("review was reset after document edit: %+v, %v", stored, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != reviewedHead {
		t.Fatalf("review anchor lost: %+v, %v", run, err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt.LocalHeadSHA != newHead {
		t.Fatalf("new comparison not bound: %+v, %v", receipt, err)
	}
}

func TestPRContextGuardRevalidatesAfterPipelinePushAdvancesHead(t *testing.T) {
	dir, base, reviewedHead := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, reviewedHead, config.Commands{})
	selection := pipeline.PRTargetSelection{TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, `{"findings":[{"severity":"info","description":"new test file written by agent","action":"no-op"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "formatted.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "formatted.txt")
	gitCmd(t, dir, "commit", "-m", "format source")
	publishedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
		HeadSHA: publishedHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = publishedHead
	sctx.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(sctx, types.StepPush, selection, false)
	if err != nil || decision.RestartFrom != "" {
		t.Fatalf("pipeline push context = %+v, %v; want no restart", decision, err)
	}
	stored, err := sctx.DB.GetStepResult(review.ID)
	if err != nil || stored.Status != types.StepStatusCompleted {
		t.Fatalf("review was replayed for the published head: %+v, %v", stored, err)
	}
	storedTest, err := sctx.DB.GetStepResult(testStep.ID)
	if err != nil || storedTest.Status != types.StepStatusCompleted || storedTest.FindingsJSON == nil {
		t.Fatalf("test evidence was replayed for the published head: %+v, %v", storedTest, err)
	}
}

func TestPRContextGuardAcceptsPublishedCIRepairHead(t *testing.T) {
	for _, afterStep := range []bool{false, true} {
		t.Run(map[bool]string{false: "before CI", true: "after CI"}[afterStep], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			selection := pipeline.PRTargetSelection{TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("fixed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "repair.txt")
			gitCmd(t, dir, "commit", "-m", "ci repair")
			publishedHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
				HeadSHA: publishedHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
			}); err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA = publishedHead
			sctx.PRContextAfterStep = afterStep
			decision, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false)
			if err != nil || decision.RestartFrom != "" {
				t.Fatalf("published CI repair context = %+v, %v; want forward receipt", decision, err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil || receipt.LocalHeadSHA != publishedHead {
				t.Fatalf("published repair receipt = %+v, %v", receipt, err)
			}
		})
	}
}
