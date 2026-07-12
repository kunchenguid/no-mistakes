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
	"github.com/kunchenguid/no-mistakes/internal/git"
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

func TestCIStepRefusesToStagePreExistingUserChangesAsCIRepair(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("user staged content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user-staged.txt"), []byte("user staged file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt", "user-staged.txt")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("user unstaged content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user-untracked.txt"), []byte("user untracked file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeStatus := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all")
	beforeIndex := gitCmd(t, dir, "write-tree")

	fixerCalled := false
	fixer := &ciRepublishAgent{fix: func(string) error {
		fixerCalled = true
		return nil
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"build"}, false)
	if err == nil || !strings.Contains(err.Error(), "pre-existing") {
		t.Fatalf("autoFixCI = pushed %v, err %v; want fail-closed dirty-state error", pushed, err)
	}
	if pushed || fixerCalled {
		t.Fatalf("dirty candidate = pushed %v, fixer called %v; want no repair attempt", pushed, fixerCalled)
	}
	if got := gitCmd(t, dir, "write-tree"); got != beforeIndex {
		t.Fatalf("user index tree = %s, want %s", got, beforeIndex)
	}
	if got := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
		t.Fatalf("user status = %q, want %q", got, beforeStatus)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != headSHA {
		t.Fatalf("remote SHA = %s, want unchanged %s", got, headSHA)
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

	fixer := &ciRepublishAgent{fix: func(cwd string) error {
		cmd := exec.Command("git", "rebase", "--abort")
		cmd.Dir = cwd
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("abort pre-existing rebase: %s: %w", output, err)
		}
		return errors.New("repair aborted the pre-existing rebase")
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
func TestCIStepRefusesUnrelatedDirtyPathsDuringPreExistingRebase(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	cmd := exec.Command("git", "rebase", "--force-rebase", "--exec", "false", "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("pre-existing rebase unexpectedly completed: %s", output)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated-user-file.txt"), []byte("do not publish\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixerCalled := false
	fixer := &ciRepublishAgent{fix: func(string) error {
		fixerCalled = true
		return nil
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, originalHead, config.Commands{})
	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil || pushed {
		t.Fatalf("dirty pre-existing rebase = pushed %v, err %v; want refusal", pushed, err)
	}
	if fixerCalled {
		t.Fatal("fixer ran against unrelated user dirt during pre-existing rebase")
	}
	if !ciRebaseInProgress(t, dir) {
		t.Fatal("refusal changed the pre-existing rebase topology")
	}
}

func TestCIStepRestoresSharedRefsAfterFailedAndSuccessfulRepairs(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	gitCmd(t, dir, "update-ref", "refs/heads/protected", baseSHA)
	gitCmd(t, dir, "update-ref", "refs/tags/protected", headSHA)
	wantProtectedBranch := gitCmd(t, dir, "rev-parse", "refs/heads/protected")
	wantProtectedTag := gitCmd(t, dir, "rev-parse", "refs/tags/protected")

	mutateRefs := func(cwd string) {
		gitCmd(t, cwd, "update-ref", "refs/heads/protected", headSHA)
		gitCmd(t, cwd, "update-ref", "-d", "refs/tags/protected")
		gitCmd(t, cwd, "update-ref", "refs/tags/repair-created", headSHA)
		gitCmd(t, cwd, "update-ref", "refs/no-mistakes/ci-republish-pending/malicious", headSHA)
	}
	assertProtected := func() {
		t.Helper()
		if got := gitCmd(t, dir, "rev-parse", "refs/heads/protected"); got != wantProtectedBranch {
			t.Fatalf("protected branch = %s, want %s", got, wantProtectedBranch)
		}
		if got := gitCmd(t, dir, "rev-parse", "refs/tags/protected"); got != wantProtectedTag {
			t.Fatalf("protected tag = %s, want %s", got, wantProtectedTag)
		}
		if output, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "refs/tags/repair-created").CombinedOutput(); err == nil {
			t.Fatalf("repair-created ref remains at %s", strings.TrimSpace(string(output)))
		}
		if output, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "refs/no-mistakes/ci-republish-pending/malicious").CombinedOutput(); err == nil {
			t.Fatalf("repair-created pending ref remains at %s", strings.TrimSpace(string(output)))
		}
	}

	failing := &ciRepublishAgent{fix: func(cwd string) error {
		mutateRefs(cwd)
		return errors.New("repair failed after mutating shared refs")
	}}
	step, sctx, host, pr := republishContext(t, failing, upstream, dir, baseSHA, headSHA, config.Commands{})
	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil || pushed {
		t.Fatalf("failed shared-ref repair = pushed %v, err %v", pushed, err)
	}
	assertProtected()

	successful := &ciRepublishAgent{fix: func(cwd string) error {
		mutateRefs(cwd)
		hookPath := gitCmd(t, cwd, "rev-parse", "--git-path", "hooks/post-commit")
		if !filepath.IsAbs(hookPath) {
			hookPath = filepath.Join(cwd, hookPath)
		}
		if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ngit update-ref refs/heads/protected HEAD\n"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(cwd, "successful-ci-fix.txt"), []byte("fixed\n"), 0o644)
	}}
	step, sctx, host, pr = republishContext(t, successful, upstream, dir, baseSHA, headSHA, config.Commands{})
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("successful shared-ref repair did not publish")
	}
	assertProtected()
	hookPath := gitCmd(t, dir, "rev-parse", "--git-path", "hooks/post-commit")
	if !filepath.IsAbs(hookPath) {
		hookPath = filepath.Join(dir, hookPath)
	}
	if _, err := os.Lstat(hookPath); !os.IsNotExist(err) {
		t.Fatalf("repair-created post-commit hook remains: %v", err)
	}
}

func TestCIStepSharedRefRestorePreservesConcurrentRefChangesWithCAS(t *testing.T) {
	_, dir, baseSHA, headSHA := setupCIRepublish(t)
	const (
		updatedRef = "refs/heads/cas-updated"
		deletedRef = "refs/heads/cas-deleted"
		createdRef = "refs/heads/cas-created"
	)
	gitCmd(t, dir, "update-ref", updatedRef, baseSHA)
	gitCmd(t, dir, "update-ref", deletedRef, baseSHA)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	snapshot, err := captureCICandidate(sctx)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.cleanup(sctx)

	gitCmd(t, dir, "update-ref", updatedRef, headSHA)
	gitCmd(t, dir, "update-ref", "-d", deletedRef)
	gitCmd(t, dir, "update-ref", createdRef, headSHA)

	concurrentByRef := map[string]string{
		updatedRef: gitCmd(t, dir, "commit-tree", headSHA+"^{tree}", "-p", headSHA, "-m", "concurrent update"),
		deletedRef: headSHA,
		createdRef: baseSHA,
	}
	hooked := make(map[string]bool, len(concurrentByRef))
	sctx.Ctx = context.WithValue(sctx.Ctx, ciRefRestoreHookContextKey{}, func(ref string) {
		concurrentSHA, ok := concurrentByRef[ref]
		if !ok || hooked[ref] {
			return
		}
		hooked[ref] = true
		gitCmd(t, dir, "update-ref", ref, concurrentSHA)
	})

	if err := snapshot.restoreSharedRefs(sctx, nil); err == nil {
		t.Fatal("shared-ref restore unexpectedly ignored concurrent CAS conflicts")
	}
	for ref, want := range concurrentByRef {
		if !hooked[ref] {
			t.Fatalf("restore hook did not run for %s", ref)
		}
		if got := gitCmd(t, dir, "rev-parse", ref); got != want {
			t.Fatalf("concurrent ref %s = %s, want preserved %s", ref, got, want)
		}
	}
}

func TestCIStepSharedRefRestorePreservesConcurrentSymbolicTopologyWithCAS(t *testing.T) {
	_, dir, baseSHA, headSHA := setupCIRepublish(t)
	const (
		symbolicRef      = "refs/heads/cas-symbolic"
		beforeTarget     = "refs/heads/cas-symbolic-before"
		attemptTarget    = "refs/heads/cas-symbolic-attempt"
		concurrentTarget = "refs/heads/cas-symbolic-concurrent"
	)
	gitCmd(t, dir, "update-ref", beforeTarget, baseSHA)
	gitCmd(t, dir, "update-ref", attemptTarget, headSHA)
	gitCmd(t, dir, "update-ref", concurrentTarget, baseSHA)
	gitCmd(t, dir, "symbolic-ref", symbolicRef, beforeTarget)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	snapshot, err := captureCICandidate(sctx)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.cleanup(sctx)

	gitCmd(t, dir, "symbolic-ref", symbolicRef, attemptTarget)
	hookRan := false
	sctx.Ctx = context.WithValue(sctx.Ctx, ciRefRestoreHookContextKey{}, func(ref string) {
		if ref != symbolicRef || hookRan {
			return
		}
		hookRan = true
		gitCmd(t, dir, "symbolic-ref", symbolicRef, concurrentTarget)
	})
	if err := snapshot.restoreSharedRefs(sctx, nil); err == nil {
		t.Fatal("symbolic shared-ref restore unexpectedly ignored concurrent topology change")
	}
	if !hookRan {
		t.Fatal("symbolic shared-ref restore did not reach deterministic CAS hook")
	}
	if got := gitCmd(t, dir, "symbolic-ref", symbolicRef); got != concurrentTarget {
		t.Fatalf("concurrent symbolic target = %s, want preserved %s", got, concurrentTarget)
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
	step.transportPublication = func(ctx context.Context, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA string, force bool) error {
		if err := git.Push(ctx, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA, force); err != nil {
			return err
		}
		return errors.New("connection lost after ignored-state publication was accepted")
	}
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatalf("clean retry: %v", err)
	}
	if pushed {
		t.Fatal("ambiguous accepted transport unexpectedly reported a confirmed push")
	}
	if got := gitCmd(t, upstream, "show", "refs/heads/feature:successful-ci-fix.txt"); got != "successful" {
		t.Fatalf("published retry content = %q, want successful", got)
	}
	if err := assertOriginal(dir); err != nil {
		t.Fatalf("successful repair changed pre-existing ignored state: %v", err)
	}
}

func TestCIStepNoOpRepairFailsClosedAndRestoresIgnoredMutation(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	excludePath := gitCmd(t, dir, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}
	if err := os.WriteFile(excludePath, []byte("ignored-state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(dir, "ignored-state")
	if err := os.WriteFile(ignoredPath, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixer := &ciRepublishAgent{fix: func(string) error {
		return os.WriteFile(ignoredPath, []byte("mutated without a candidate diff\n"), 0o644)
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})

	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err == nil || !strings.Contains(err.Error(), "candidate filesystem differs") {
		t.Fatalf("no-op ignored mutation = pushed %v, err %v; want closed integrity failure", pushed, err)
	}
	if pushed {
		t.Fatal("no-op ignored mutation was published")
	}
	if got, readErr := os.ReadFile(ignoredPath); readErr != nil || string(got) != "original\n" {
		t.Fatalf("ignored path after rejection = %q, %v; want exact original", got, readErr)
	}
	if info, statErr := os.Stat(ignoredPath); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ignored path mode after rejection = %v, %v; want 0600", info, statErr)
	}
}

func TestCIStepNoOpRepairFailsClosedAndRestoresRebaseTopologyMutation(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	fixer := &ciRepublishAgent{fix: func(cwd string) error {
		rebaseDir := gitCmd(t, cwd, "rev-parse", "--git-path", "rebase-merge")
		if !filepath.IsAbs(rebaseDir) {
			rebaseDir = filepath.Join(cwd, rebaseDir)
		}
		if err := os.MkdirAll(rebaseDir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(rebaseDir, "head-name"), []byte("refs/heads/feature\n"), 0o600)
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})

	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err == nil || !strings.Contains(err.Error(), "rebase topology differs") {
		t.Fatalf("no-op rebase mutation = pushed %v, err %v; want closed integrity failure", pushed, err)
	}
	if pushed {
		t.Fatal("no-op rebase mutation was published")
	}
	if ciRebaseInProgress(t, dir) {
		t.Fatal("rejected no-op repair left forged rebase topology")
	}
}

func TestCIStepDoesNotResurrectDeletedTrackedPathThatRepairNowIgnores(t *testing.T) {
	upstream, dir, baseSHA, _ := setupCIRepublish(t)
	if err := os.WriteFile(filepath.Join(dir, "obsolete.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "obsolete.txt")
	gitCmd(t, dir, "commit", "-m", "add obsolete tracked path")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	fixer := &ciRepublishAgent{fix: func(cwd string) error {
		if err := os.Remove(filepath.Join(cwd, "obsolete.txt")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte("obsolete.txt\n"), 0o644)
	}}
	step, sctx, host, pr := republishContext(t, fixer, upstream, dir, baseSHA, headSHA, config.Commands{})
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("verified deletion was not published")
	}
	if _, err := os.Lstat(filepath.Join(dir, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted path was resurrected locally: %v", err)
	}
	if output, err := exec.Command("git", "-C", upstream, "cat-file", "-e", "refs/heads/feature:obsolete.txt").CombinedOutput(); err == nil {
		t.Fatalf("deleted path remains on remote: %s", output)
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
