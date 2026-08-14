package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolveRunTargetExplicitAndDefault(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, mainSHA := setupTestGitRepo(t, p, d, "target-resolution-repo")

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "test")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("test target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "test.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "test target")
	testSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/test")

	branch, sha, err := resolveRunTarget(context.Background(), p.RepoDir(repo.ID), repo, "test")
	if err != nil {
		t.Fatalf("resolve explicit target: %v", err)
	}
	if branch != "test" || sha != testSHA {
		t.Fatalf("explicit target = %s@%s, want test@%s", branch, sha, testSHA)
	}

	branch, sha, err = resolveRunTarget(context.Background(), p.RepoDir(repo.ID), repo, "")
	if err != nil {
		t.Fatalf("resolve omitted target: %v", err)
	}
	if branch != "main" || sha != mainSHA {
		t.Fatalf("omitted target = %s@%s, want main@%s", branch, sha, mainSHA)
	}
}

func TestResolveRunTargetRejectsUnsafeAndMissingBranches(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, _ := setupTestGitRepo(t, p, d, "invalid-target-repo")

	for _, target := range []string{"HEAD", "../test", "missing"} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			if _, _, err := resolveRunTarget(context.Background(), p.RepoDir(repo.ID), repo, target); err == nil {
				t.Fatalf("resolveRunTarget(%q) succeeded, want refusal", target)
			}
		})
	}
}

func TestPushReceivedInvalidTargetStopsBeforeRunAndPipeline(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{review} })
	repo, headSHA := setupTestGitRepo(t, p, d, "invalid-target-ingress-repo")

	mgr := NewRunManager(d, p, func() []pipeline.Step { return []pipeline.Step{review} })
	_, err := mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate:   p.RepoDir(repo.ID),
		Ref:    "refs/heads/feature",
		Old:    strings.Repeat("0", 40),
		New:    headSHA,
		Target: "does-not-exist",
	})
	if err == nil {
		t.Fatal("invalid target started a run")
	}
	if got := review.execCnt.Load(); got != 0 {
		t.Fatalf("review executed %d times, want 0", got)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("invalid target created %d durable runs, want 0", len(runs))
	}
}

func TestRerunInheritsSelectedNonDefaultTarget(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, mainSHA := setupTestGitRepo(t, p, d, "target-rerun-repo")

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "test")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "target.txt"), []byte("target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "target.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "target")
	targetSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/test")

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "feature.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "feature")
	featureSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature")

	previous, err := d.InsertRunWithIntentAndTarget(repo.ID, "feature", featureSHA, mainSHA, nil, "test", targetSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(previous.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:        repo.ID,
		Branch:        "feature",
		PreviousRunID: previous.ID,
	}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.TargetBranch != "test" || run.TargetSHA != targetSHA {
		t.Fatalf("rerun target = %s@%s, want test@%s", run.TargetBranch, run.TargetSHA, targetSHA)
	}
}
