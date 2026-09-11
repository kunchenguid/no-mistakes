package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func retainedCIRef(runID string) string { return "refs/no-mistakes/ci-repair/" + runID }

// BindRetainedCIRepair records only an exact, clean correction after publication
// failed at attestation. The ref keeps it reachable independently of HEAD.
func BindRetainedCIRepair(ctx context.Context, database *db.DB, run *db.Run, stepID, gateDir, workDir, head string) error {
	if run.ReviewApprovedHeadSHA == nil || gateDir == "" || stepID == "" {
		return fmt.Errorf("retained CI repair lacks gate or review authority")
	}
	r := db.RetainedCIRepair{RunID: run.ID, StepID: stepID, RepoID: run.RepoID, Branch: run.Branch,
		RecordedHead: run.HeadSHA, ReviewedHead: *run.ReviewApprovedHeadSHA, RetainedHead: head}
	if err := verifyRetainedCIGit(ctx, r, gateDir, workDir); err != nil {
		return err
	}
	existing, err := database.RetainedCIRepair(run.ID)
	if err != nil {
		return err
	}
	if existing != nil && *existing != r {
		return fmt.Errorf("another retained CI repair is already bound")
	}
	ref := retainedCIRef(run.ID)
	// A conflicting or symbolic existing anchor must never be overwritten.
	if _, err := git.Run(ctx, workDir, "symbolic-ref", "-q", ref); err == nil {
		return fmt.Errorf("retained CI anchor is symbolic")
	}
	if current, err := git.Run(ctx, workDir, "rev-parse", "--verify", ref); err == nil {
		current = strings.TrimSpace(current)
		if current == r.RecordedHead && existing == nil {
			// The previous repair settled and its binding was cleared. Advance
			// only that exact published anchor, never an unknown retained head.
			if _, err := git.Run(ctx, workDir, "update-ref", "--no-deref", ref, head, current); err != nil {
				return err
			}
		} else if current != head {
			return fmt.Errorf("retained CI anchor moved")
		}
	} else if _, err := git.Run(ctx, workDir, "update-ref", "--no-deref", ref, head, strings.Repeat("0", 40)); err != nil {
		return err
	}
	return database.BindRetainedCIRepair(r)
}

// VerifyRetainedCIRepair is shared by parked-run restoration and explicit retry.
// No binding means no exception: prose findings and ancestry alone are not proof.
func VerifyRetainedCIRepair(ctx context.Context, database *db.DB, run *db.Run, gateDir, workDir string) (*db.RetainedCIRepair, error) {
	r, err := database.RetainedCIRepair(run.ID)
	if err != nil || r == nil {
		return r, err
	}
	current, err := database.GetRun(run.ID)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Status != types.RunRunning || current.RepoID != r.RepoID || current.Branch != r.Branch ||
		run.RepoID != r.RepoID || run.Branch != r.Branch || current.HeadSHA != r.RecordedHead ||
		current.ReviewApprovedHeadSHA == nil || *current.ReviewApprovedHeadSHA != r.ReviewedHead || run.HeadSHA != r.RecordedHead {
		return nil, fmt.Errorf("retained CI repair owner or authority changed")
	}
	step, err := database.GetStepResult(r.StepID)
	if err != nil {
		return nil, err
	}
	if step == nil || step.RunID != run.ID || step.StepName != types.StepCI ||
		(step.Status != types.StepStatusFixReview && step.Status != types.StepStatusFixing && step.Status != types.StepStatusRunning) {
		return nil, fmt.Errorf("retained CI repair has no active CI gate")
	}
	if err := verifyRetainedCIGit(ctx, *r, gateDir, workDir); err != nil {
		return nil, err
	}
	ref := retainedCIRef(run.ID)
	if _, err := git.Run(ctx, workDir, "symbolic-ref", "-q", ref); err == nil {
		return nil, fmt.Errorf("retained CI anchor is symbolic")
	}
	anchor, err := git.Run(ctx, workDir, "rev-parse", "--verify", ref)
	if err != nil || strings.TrimSpace(anchor) != r.RetainedHead {
		return nil, fmt.Errorf("retained CI anchor is missing or moved")
	}
	return r, nil
}

func verifyRetainedCIGit(ctx context.Context, r db.RetainedCIRepair, gateDir, workDir string) error {
	common, err := git.Run(ctx, workDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	commonPath, err := filepath.EvalSymlinks(strings.TrimSpace(common))
	if err != nil {
		return err
	}
	gatePath, err := filepath.EvalSymlinks(gateDir)
	if err != nil || commonPath != gatePath {
		return fmt.Errorf("retained CI worktree belongs to another gate")
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil || head != r.RetainedHead {
		return fmt.Errorf("retained CI head moved")
	}
	dirty, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil || strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("retained CI worktree is dirty or unreadable")
	}
	for _, ancestor := range []string{r.RecordedHead, r.ReviewedHead} {
		if _, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", ancestor, head); err != nil {
			return fmt.Errorf("retained CI repair does not continue recorded review authority")
		}
	}
	return nil
}
