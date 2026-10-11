package testgit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func BindRunComparison(t testing.TB, database *db.DB, run *db.Run, workDir, target, prURL, sourceRepo, sourceBranch string) *db.PRContext {
	t.Helper()
	gitPath, err := RealGit()
	if err != nil {
		t.Fatal(err)
	}
	gitOutput := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(gitPath, append([]string{"-C", workDir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	head := gitOutput("rev-parse", "HEAD")
	if head != run.HeadSHA {
		t.Fatalf("fixture worktree head %s differs from run head %s", head, run.HeadSHA)
	}
	targetSHA := gitOutput("rev-parse", target)
	candidate, err := RunComparisonCandidate(run, workDir, target, targetSHA, prURL, sourceRepo, sourceBranch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.BindRunPRContext(run.ID, candidate, types.StepReview); err != nil {
		t.Fatal(err)
	}
	receipt, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func RunComparisonCandidate(run *db.Run, workDir, targetBranch, targetSHA, prURL, sourceRepo, sourceBranch string) (db.PRContextCandidate, error) {
	gitPath, err := RealGit()
	if err != nil {
		return db.PRContextCandidate{}, err
	}
	gitOutput := func(args ...string) (string, error) {
		out, err := exec.Command(gitPath, append([]string{"-C", workDir}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	head, err := gitOutput("rev-parse", "HEAD")
	if err != nil {
		return db.PRContextCandidate{}, err
	}
	if run == nil || head != run.HeadSHA {
		return db.PRContextCandidate{}, fmt.Errorf("fixture worktree head %s differs from run head", head)
	}
	mergeBase, err := gitOutput("merge-base", targetSHA, head)
	if err != nil {
		return db.PRContextCandidate{}, err
	}
	patch, err := exec.Command(gitPath, "-C", workDir, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", targetSHA+"..."+head).Output()
	if err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("fixture PR diff: %w", err)
	}
	digest := sha256.Sum256(patch)
	forgeHead := ""
	if prURL != "" {
		forgeHead = head
	}
	return db.PRContextCandidate{PRURL: prURL, SourceRepo: sourceRepo, SourceBranch: sourceBranch,
		ForgeHeadSHA: forgeHead, LocalHeadSHA: head, TargetBranch: targetBranch, TargetSHA: targetSHA,
		MergeBaseSHA: mergeBase, DiffDigest: hex.EncodeToString(digest[:])}, nil
}
