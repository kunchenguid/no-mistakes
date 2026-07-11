package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func ciRepairPersistenceContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(stepResult.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.CurrentRound = round
	return sctx
}

func persistCIPlan(t *testing.T, step *CIStep, sctx *pipeline.StepContext, plan ciRepairPlan) {
	t.Helper()
	ids, err := step.beginCIRepairs(sctx, plan, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishCIRepairs(sctx, ids, db.RepairVerdictUnresolved, "hosted check still failing", db.RepairStatusUnresolved); err != nil {
		t.Fatal(err)
	}
}

func TestCIStep_RestartRetainsHostedFailureLineageTier(t *testing.T) {
	sctx := ciRepairPersistenceContext(t)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	firstStep := &CIStep{}
	first, err := firstStep.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if first.Tier != 0 || len(first.Issues) != 1 {
		t.Fatalf("first plan = %+v, want one tier-0 hosted failure", first)
	}
	persistCIPlan(t, firstStep, sctx, first)
	repairs, err := sctx.DB.GetFindingRepairsByLineage(first.Issues[0].LineageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Tier != 0 || repairs[0].RemainingBudget != ciRepairBudget-1 {
		t.Fatalf("durable first repair = %+v, want tier 0 with %d remaining", repairs, ciRepairBudget-1)
	}

	restarted := &CIStep{}
	second, err := restarted.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tier != 1 || len(second.Issues) != 1 || second.Issues[0].LineageID != first.Issues[0].LineageID {
		t.Fatalf("restart plan = %+v, want same lineage at tier 1 after %+v", second, first)
	}
	if unresolved, err := sctx.DB.HasUnresolvedBlockingRepair(sctx.Run.ID); err != nil || !unresolved {
		t.Fatalf("unattended unresolved = %v, %v; want true for hosted CI lineage", unresolved, err)
	}
}

func TestCIStep_FailedHostedRepairPersistsBeforeInvocation(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	fixer := &mockAgent{
		name: "failing",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("fixer unavailable")
		},
	}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(stepResult.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.CurrentRound = round
	plan, err := step.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.runPlannedCIRepair(sctx, host, pr, plan, ciRepairBudget); err == nil {
		t.Fatal("expected failed CI repair invocation")
	}
	repairs, err := sctx.DB.GetFindingRepairsByLineage(plan.Issues[0].LineageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 {
		t.Fatalf("repair rows = %d, want 1", len(repairs))
	}
	if repairs[0].Status != db.RepairStatusFailed || repairs[0].Verdict != db.RepairVerdictInconclusive {
		t.Fatalf("repair = %+v, want failed inconclusive repair", repairs[0])
	}
}

func TestCIStep_FailedHostedRepairRestoresExactCandidateBeforeRetry(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("legitimate staged feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legitimate-staged.txt"), []byte("legitimate staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt", "legitimate-staged.txt")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("legitimate unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legitimate-untracked.txt"), []byte("legitimate untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeStatus := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all")
	beforeIndex := gitCmd(t, dir, "write-tree")

	fixerCalls := 0
	verified := false
	fixer := &ciRepublishAgent{
		fix: func(cwd string) error {
			fixerCalls++
			if fixerCalls == 1 {
				if err := os.WriteFile(filepath.Join(cwd, "failed-committed.txt"), []byte("failed committed\n"), 0o644); err != nil {
					return err
				}
				gitCmd(t, cwd, "add", "failed-committed.txt")
				gitCmd(t, cwd, "commit", "-m", "failed repair commit")
				if err := os.WriteFile(filepath.Join(cwd, "failed-staged.txt"), []byte("failed staged\n"), 0o644); err != nil {
					return err
				}
				gitCmd(t, cwd, "add", "failed-staged.txt")
				if err := os.WriteFile(filepath.Join(cwd, "feature.txt"), []byte("failed tracked\n"), 0o644); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(cwd, "failed-untracked.txt"), []byte("failed untracked\n"), 0o644); err != nil {
					return err
				}
				return errors.New("repair process exited after partial mutation")
			}
			if got := gitCmd(t, cwd, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("retry started at HEAD %s, want %s", got, headSHA)
			}
			if got := gitCmd(t, cwd, "write-tree"); got != beforeIndex {
				t.Fatalf("retry index tree = %s, want %s", got, beforeIndex)
			}
			if got := gitCmd(t, cwd, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
				t.Fatalf("retry status = %q, want %q", got, beforeStatus)
			}
			if got, err := os.ReadFile(filepath.Join(cwd, "feature.txt")); err != nil || string(got) != "legitimate unstaged\n" {
				t.Fatalf("retry tracked content = %q, %v; want legitimate state", got, err)
			}
			for path, want := range map[string]string{
				"legitimate-staged.txt":    "legitimate staged\n",
				"legitimate-untracked.txt": "legitimate untracked\n",
			} {
				got, err := os.ReadFile(filepath.Join(cwd, path))
				if err != nil || string(got) != want {
					t.Fatalf("retry content for %s = %q, %v; want %q", path, got, err, want)
				}
			}
			if err := os.WriteFile(filepath.Join(cwd, "successful-ci-fix.txt"), []byte("successful\n"), 0o644); err != nil {
				return err
			}
			return nil
		},
		verify: func(cwd string) error {
			for _, path := range []string{"failed-committed.txt", "failed-staged.txt", "failed-untracked.txt"} {
				if _, err := os.Lstat(filepath.Join(cwd, path)); !os.IsNotExist(err) {
					return errors.New("verifier observed failed repair content: " + path)
				}
			}
			verified = true
			return nil
		},
	}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(stepResult.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.CurrentRound = round

	firstPlan, err := step.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if pushed, err := step.runPlannedCIRepair(sctx, host, pr, firstPlan, ciRepairBudget); err == nil {
		t.Fatalf("first repair = pushed %v, nil error; want failed invocation", pushed)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("failed repair left HEAD %s, want %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "write-tree"); got != beforeIndex {
		t.Fatalf("failed repair left index tree %s, want %s", got, beforeIndex)
	}
	if got := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
		t.Fatalf("failed repair left status %q, want %q", got, beforeStatus)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "legitimate-untracked.txt")); err != nil || string(got) != "legitimate untracked\n" {
		t.Fatalf("failed repair restored untracked content = %q, %v", got, err)
	}
	firstRepairs, err := sctx.DB.GetFindingRepairsByLineage(firstPlan.Issues[0].LineageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRepairs) != 1 || firstRepairs[0].Status != db.RepairStatusFailed {
		t.Fatalf("failed repair journal = %+v, want one durable failed row", firstRepairs)
	}
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"build"}, false)
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if !pushed || !verified {
		t.Fatalf("second repair = pushed %v, verified %v; want clean verified publication", pushed, verified)
	}
	remoteFiles := gitCmd(t, upstream, "ls-tree", "-r", "--name-only", "refs/heads/feature")
	if strings.Contains(remoteFiles, "failed-") {
		t.Fatalf("published tree contains failed repair content: %q", remoteFiles)
	}
	for _, path := range []string{"legitimate-staged.txt", "legitimate-untracked.txt", "successful-ci-fix.txt"} {
		if !strings.Contains(remoteFiles, path) {
			t.Fatalf("published tree %q lost legitimate/successful path %q", remoteFiles, path)
		}
	}
	if got := gitCmd(t, upstream, "show", "refs/heads/feature:feature.txt"); got != "legitimate unstaged" {
		t.Fatalf("published tracked content = %q, want legitimate pre-repair state", got)
	}
	repairs, err := sctx.DB.GetFindingRepairsByLineage(firstPlan.Issues[0].LineageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Status != db.RepairStatusFailed {
		t.Fatalf("repair journal = %+v, want durable failed row after later publication", repairs)
	}
}

func TestCIStep_FailedRepairAbortsAttemptStartedRebaseBeforeRestore(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("base version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt")
	gitCmd(t, dir, "commit", "-m", "conflicting base change")
	baseTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	fixer := &ciRepublishAgent{fix: func(cwd string) error {
		cmd := exec.Command("git", "rebase", baseTip)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
		if output, err := cmd.CombinedOutput(); err == nil {
			return errors.New("rebase unexpectedly completed")
		} else {
			return fmt.Errorf("repair stopped during rebase: %s: %w", output, err)
		}
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, originalHead, config.Commands{})
	sctx.Repo.DefaultBranch = "main"

	if pushed, err := step.autoFixCI(sctx, host, pr, nil, true); err == nil {
		t.Fatalf("autoFixCI = pushed %v, nil error; want failed rebase attempt", pushed)
	}
	if ciRebaseInProgress(t, dir) {
		t.Fatal("failed repair left its rebase in progress")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != originalHead {
		t.Fatalf("failed rebase repair restored HEAD %s, want %s", got, originalHead)
	}
	if got := gitCmd(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("failed rebase repair left worktree changes: %q", got)
	}
}

func TestCIStep_FailedRepairDoesNotAbortPreExistingRebase(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	cmd := exec.Command("git", "rebase", "--force-rebase", "--exec", "false", "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("pre-existing rebase unexpectedly completed: %s", output)
	}
	if !ciRebaseInProgress(t, dir) {
		t.Fatal("fixture did not leave a pre-existing rebase")
	}
	preAttemptHead := gitCmd(t, dir, "rev-parse", "HEAD")
	preAttemptStatus := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all")

	fixer := &ciRepublishAgent{fix: func(string) error {
		return errors.New("repair failed without touching the pre-existing rebase")
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, originalHead, config.Commands{})

	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil {
		t.Fatalf("autoFixCI = pushed %v, nil error; want failed repair", pushed)
	}
	if !ciRebaseInProgress(t, dir) {
		t.Fatal("rollback aborted a rebase that predated the repair attempt")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != preAttemptHead {
		t.Fatalf("pre-existing rebase HEAD = %s after rollback, want %s", got, preAttemptHead)
	}
	if got := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); got != preAttemptStatus {
		t.Fatalf("pre-existing rebase status = %q after rollback, want %q", got, preAttemptStatus)
	}
}

func TestCIStep_FailedRepairRestoresIgnoredFilesystemExactlyBeforeRetry(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	excludePath := gitCmd(t, dir, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}
	if err := os.WriteFile(excludePath, []byte("ignored-*\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("ignored-modified.txt", "original modified file\n", 0o644)
	write("ignored-deleted.txt", "original deleted file\n", 0o644)
	write("ignored-mode.sh", "#!/bin/sh\n", 0o755)
	write("ignored-target-a.txt", "target a\n", 0o644)
	write("ignored-target-b.txt", "target b\n", 0o644)
	if err := os.Symlink("ignored-target-a.txt", filepath.Join(dir, "ignored-link")); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "ignored-dir"), 0o711); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("ignored-dir", "content.txt"), "original directory content\n", 0o640)
	if err := os.Mkdir(filepath.Join(dir, "ignored-dir", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if status := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("ignored fixture unexpectedly visible to git: %q", status)
	}

	assertOriginal := func(cwd string) error {
		for path, want := range map[string]string{
			"ignored-modified.txt":                      "original modified file\n",
			"ignored-deleted.txt":                       "original deleted file\n",
			filepath.Join("ignored-dir", "content.txt"): "original directory content\n",
		} {
			got, err := os.ReadFile(filepath.Join(cwd, path))
			if err != nil || string(got) != want {
				return fmt.Errorf("%s = %q, %v; want %q", path, got, err, want)
			}
		}
		for _, path := range []string{"ignored-created.txt", "ignored-created-dir", "ignored-created-link"} {
			if _, err := os.Lstat(filepath.Join(cwd, path)); !os.IsNotExist(err) {
				return fmt.Errorf("failed-attempt ignored path %s remains: %v", path, err)
			}
		}
		linkTarget, err := os.Readlink(filepath.Join(cwd, "ignored-link"))
		if err != nil || linkTarget != "ignored-target-a.txt" {
			return fmt.Errorf("ignored symlink target = %q, %v; want ignored-target-a.txt", linkTarget, err)
		}
		for path, want := range map[string]os.FileMode{
			"ignored-mode.sh":                     0o755,
			"ignored-dir":                         0o711,
			filepath.Join("ignored-dir", "empty"): 0o700,
		} {
			info, err := os.Lstat(filepath.Join(cwd, path))
			if err != nil || info.Mode().Perm() != want {
				return fmt.Errorf("%s mode = %v, %v; want %v", path, info, err, want)
			}
		}
		return nil
	}

	fixerCalls := 0
	fixer := &ciRepublishAgent{
		fix: func(cwd string) error {
			fixerCalls++
			if fixerCalls == 1 {
				if err := os.WriteFile(filepath.Join(cwd, "ignored-modified.txt"), []byte("failed mutation\n"), 0o600); err != nil {
					return err
				}
				if err := os.Remove(filepath.Join(cwd, "ignored-deleted.txt")); err != nil {
					return err
				}
				if err := os.Chmod(filepath.Join(cwd, "ignored-mode.sh"), 0o600); err != nil {
					return err
				}
				if err := os.Remove(filepath.Join(cwd, "ignored-link")); err != nil {
					return err
				}
				if err := os.Symlink("ignored-target-b.txt", filepath.Join(cwd, "ignored-link")); err != nil {
					return err
				}
				if err := os.RemoveAll(filepath.Join(cwd, "ignored-dir")); err != nil {
					return err
				}
				if err := os.Mkdir(filepath.Join(cwd, "ignored-dir"), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(cwd, "ignored-created.txt"), []byte("failed create\n"), 0o644); err != nil {
					return err
				}
				if err := os.Mkdir(filepath.Join(cwd, "ignored-created-dir"), 0o755); err != nil {
					return err
				}
				if err := os.Symlink("ignored-target-b.txt", filepath.Join(cwd, "ignored-created-link")); err != nil {
					return err
				}
				return errors.New("repair failed after ignored filesystem mutations")
			}
			if err := assertOriginal(cwd); err != nil {
				return fmt.Errorf("retry candidate was not clean: %w", err)
			}
			return os.WriteFile(filepath.Join(cwd, "successful-ci-fix.txt"), []byte("successful\n"), 0o644)
		},
		verify: assertOriginal,
	}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})

	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil {
		t.Fatalf("first repair = pushed %v, nil error; want failed attempt", pushed)
	}
	if err := assertOriginal(dir); err != nil {
		t.Fatalf("failed repair did not restore ignored candidate: %v", err)
	}
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatalf("clean retry: %v", err)
	}
	if !pushed {
		t.Fatal("clean retry did not publish its verified fix")
	}
	if got := gitCmd(t, upstream, "show", "refs/heads/feature:successful-ci-fix.txt"); got != "successful" {
		t.Fatalf("published retry content = %q, want successful", got)
	}
}

func ciRebaseInProgress(t *testing.T, dir string) bool {
	t.Helper()
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		path := gitCmd(t, dir, "rev-parse", "--git-path", state)
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if _, err := os.Stat(path); err == nil {
			return true
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return false
}

func TestCIStep_DistinctHostedFailuresHaveDistinctBudgets(t *testing.T) {
	sctx := ciRepairPersistenceContext(t)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	step := &CIStep{}
	var buildLineage string
	for tier := range ciRepairBudget {
		plan, err := step.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Tier != tier || len(plan.Issues) != 1 {
			t.Fatalf("build plan %d = %+v", tier, plan)
		}
		if buildLineage == "" {
			buildLineage = plan.Issues[0].LineageID
		}
		persistCIPlan(t, step, sctx, plan)
	}
	exhausted, err := (&CIStep{}).planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted.Exhausted || len(exhausted.Issues) != 0 {
		t.Fatalf("exhausted build plan = %+v, want no further quality tier", exhausted)
	}
	if unresolved, err := sctx.DB.HasUnresolvedBlockingRepair(sctx.Run.ID); err != nil || !unresolved {
		t.Fatalf("unattended exhausted lineage = %v, %v; want fail closed", unresolved, err)
	}

	plan, err := (&CIStep{}).planCIRepair(sctx, pr, []string{"build", "test"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Exhausted != true || plan.Tier != 0 || len(plan.Issues) != 1 || plan.Issues[0].Name != "test" {
		t.Fatalf("mixed exhausted/new plan = %+v, want fresh test failure at tier 0", plan)
	}
	if plan.Issues[0].LineageID == buildLineage {
		t.Fatal("distinct hosted failures shared a lineage and budget")
	}
}

func TestCIStep_HostedFailureJournalReadFailureIsFatal(t *testing.T) {
	sctx := ciRepairPersistenceContext(t)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	if err := sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := (&CIStep{}).planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err == nil {
		t.Fatal("expected closed repair journal to fail")
	}
	if !isCIJournalFailure(err) {
		t.Fatalf("error %v is not a fatal CI journal failure", err)
	}
}
