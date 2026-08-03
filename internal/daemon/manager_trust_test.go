package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEnsureConfiguredPRBaseBranch_DefaultDoesNotAddValidation(t *testing.T) {
	workDir := t.TempDir()
	gitCmd(t, workDir, "init")

	err := ensureConfiguredPRBaseBranch(context.Background(), workDir, &db.Repo{DefaultBranch: "main"}, &config.Config{})
	if err != nil {
		t.Fatalf("unset pr.base_branch changed legacy startup behavior: %v", err)
	}
}

func TestEnsureConfiguredPRBaseBranch_FetchesUpstreamNotFork(t *testing.T) {
	ctx := context.Background()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitCmd(t, "", "init", "--bare", upstream)
	gitCmd(t, "", "init", "--bare", fork)

	seed := t.TempDir()
	gitCmd(t, seed, "init", "--initial-branch=main")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "main")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, seed, "checkout", "-b", "quality-assurance")
	if err := os.WriteFile(filepath.Join(seed, "qa.txt"), []byte("upstream qa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "upstream qa")
	want := gitOutput(t, seed, "rev-parse", "HEAD")
	gitCmd(t, seed, "push", "origin", "quality-assurance")
	// The fork deliberately has no quality-assurance branch. A fetch routed to
	// the push target instead of the parent would fail.
	gitCmd(t, seed, "push", fork, "main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "update-ref", "-d", "refs/remotes/origin/quality-assurance")
	repo := &db.Repo{UpstreamURL: upstream, ForkURL: fork, DefaultBranch: "main"}
	cfg := &config.Config{PR: config.PR{BaseBranch: "quality-assurance"}}
	if err := ensureConfiguredPRBaseBranch(ctx, workDir, repo, cfg); err != nil {
		t.Fatal(err)
	}
	got := gitOutput(t, workDir, "rev-parse", "refs/remotes/origin/quality-assurance")
	if got != want {
		t.Fatalf("configured PR base resolved to %s, want upstream tip %s", got, want)
	}
}

func TestRunStartRejectsConfiguredBaseBranches(t *testing.T) {
	for _, branch := range []string{"quality-assurance", "main"} {
		t.Run(branch, func(t *testing.T) {
			t.Setenv("NM_DEMO", "1")
			p, database := newRefreshRunFixture(t)
			repo, _ := setupTestGitRepo(t, p, database, "configured-base-"+branch)

			configPath := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
			contents := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\npr:\n  base_branch: quality-assurance\n"
			if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "add", configPath)
			gitCmd(t, repo.WorkingPath, "commit", "-m", "configure PR base")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
			gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/quality-assurance")

			manager := NewRunManager(database, p, nil)
			t.Cleanup(manager.Shutdown)
			_, err := manager.startRun(context.Background(), repo, branch, head, strings.Repeat("0", 40), "test", nil, "reject protected branch")
			if err == nil {
				t.Fatalf("protected branch %q was accepted as a run branch", branch)
			}
			if !strings.Contains(err.Error(), "feature branch") {
				t.Fatalf("error is not actionable: %v", err)
			}
			if branch == "main" && !strings.Contains(err.Error(), "repository default branch") {
				t.Fatalf("default-branch error does not explain the protected role: %v", err)
			}
		})
	}
}

type capturedRerunPRConfig struct {
	baseBranch string
	noCI       bool
}

type captureRerunPRStep struct {
	pipeline.Step
	seen chan<- capturedRerunPRConfig
}

func (s *captureRerunPRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.seen <- capturedRerunPRConfig{baseBranch: sctx.Config.PR.BaseBranch, noCI: sctx.Config.NoCI}
	return s.Step.Execute(sctx)
}

func TestRerunCrashBeforePRPersistencePreservesBaseAcrossTrustedConfigChange(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "rerun-pr-base-continuity")

	configPath := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
	initialConfig := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\npr:\n  base_branch: quality-assurance\n"
	if err := os.WriteFile(configPath, []byte(initialConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", configPath)
	gitCmd(t, repo.WorkingPath, "commit", "-m", "target QA")
	qaSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "branch", "quality-assurance")
	gitCmd(t, repo.WorkingPath, "push", "gate", "quality-assurance")

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "feature.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "add feature")
	featureSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "feature")

	selectedRun, err := database.InsertRun(repo.ID, "feature", featureSHA, qaSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRBaseBranch(selectedRun.ID, "quality-assurance", true); err != nil {
		t.Fatal(err)
	}
	prResult, err := database.InsertStepResult(selectedRun.ID, types.StepPR)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(prResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(selectedRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecoverStaleRuns("daemon crashed during execution"); err != nil {
		t.Fatal(err)
	}

	gitCmd(t, repo.WorkingPath, "checkout", "-B", "main", qaSHA)
	currentConfig := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\nno_ci: true\npr:\n  base_branch: staging\n"
	if err := os.WriteFile(configPath, []byte(currentConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", configPath)
	gitCmd(t, repo.WorkingPath, "commit", "-m", "target staging")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "branch", "staging")
	gitCmd(t, repo.WorkingPath, "push", "gate", "staging")

	binDir, ghLog := writeRerunPRBaseMockGH(t, "quality-assurance", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	seen := make(chan capturedRerunPRConfig, 1)
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&captureRerunPRStep{Step: &steps.PRStep{}, seen: seen}}
	})
	t.Cleanup(manager.Shutdown)

	runID, err := manager.HandleRerun(context.Background(), repo.ID, "feature", selectedRun.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	identifiedRun, err := database.GetRun(selectedRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if identifiedRun.PRURL == nil || *identifiedRun.PRURL != "https://github.com/test/repo/pull/42" || identifiedRun.PRState == nil || *identifiedRun.PRState != "open" {
		t.Fatalf("crash-window PR was not authoritatively persisted: url=%v state=%v", identifiedRun.PRURL, identifiedRun.PRState)
	}
	rerun := waitForRunTerminalState(t, database, runID)
	if rerun.Status != types.RunCompleted {
		t.Fatalf("rerun status = %s, want completed: %v", rerun.Status, rerun.Error)
	}
	if rerun.PRBaseBranch == nil || *rerun.PRBaseBranch != "quality-assurance" || !rerun.PRBaseBranchExplicit {
		t.Fatalf("persisted rerun PR base = %v (explicit %t), want quality-assurance (explicit)", rerun.PRBaseBranch, rerun.PRBaseBranchExplicit)
	}
	captured := <-seen
	if captured.baseBranch != "quality-assurance" {
		t.Fatalf("pipeline PR base = %q, want inherited quality-assurance", captured.baseBranch)
	}
	if !captured.noCI {
		t.Fatal("rerun did not reload current trusted no_ci setting")
	}
	logBytes, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "pr list --head feature --base quality-assurance") {
		t.Fatalf("rerun did not discover the existing PR against its original base:\n%s", logText)
	}
	if strings.Contains(logText, "--base staging") || strings.Contains(logText, "pr create ") {
		t.Fatalf("rerun retargeted discovery or attempted a duplicate PR:\n%s", logText)
	}
}

func TestRerunContinuityChecksEveryPersistedBaseCandidate(t *testing.T) {
	qualityAssurance := "quality-assurance"
	for _, tc := range []struct {
		name          string
		persistedBase *string
		explicit      bool
		openBase      string
	}{
		{name: "legacy null", openBase: "main"},
		{name: "interrupted rerun", persistedBase: &qualityAssurance, explicit: true, openBase: "quality-assurance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, database := newRefreshRunFixture(t)
			repo, head := setupTestGitRepo(t, p, database, "rerun-candidate-"+strings.ReplaceAll(tc.name, " ", "-"))
			run, err := database.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if tc.persistedBase != nil {
				if err := database.UpdateRunPRBaseBranch(run.ID, *tc.persistedBase, tc.explicit); err != nil {
					t.Fatal(err)
				}
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			binDir, ghLog := writeRerunPRBaseMockGH(t, tc.openBase, "")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			manager := NewRunManager(database, p, nil)
			candidate := runPRBaseContinuityForRun(run, repo.DefaultBranch)
			continuity, err := manager.verifyRerunPRBaseContinuity(context.Background(), repo, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if continuity == nil || continuity.branch != tc.openBase || continuity.explicit != tc.explicit {
				t.Fatalf("verified continuity = %+v, want %s (explicit %t)", continuity, tc.openBase, tc.explicit)
			}
			verified, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if verified.PRURL == nil || *verified.PRURL != "https://github.com/test/repo/pull/42" || verified.PRState == nil || *verified.PRState != "open" {
				t.Fatalf("open PR was not persisted: url=%v state=%v", verified.PRURL, verified.PRState)
			}
			logBytes, err := os.ReadFile(ghLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(logBytes), "pr list --head feature --base "+tc.openBase) {
				t.Fatalf("saved head/base tuple was not checked:\n%s", logBytes)
			}
		})
	}
}

func TestRerunContinuityRejectsStaleCachedOpenPR(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "rerun-stale-open-pr")
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRBaseBranch(run.ID, "quality-assurance", true); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	binDir, ghLog := writeRerunPRBaseMockGH(t, "", "CLOSED")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager := NewRunManager(database, p, nil)
	candidate := &runPRBaseContinuity{sourceRun: run, branch: "quality-assurance", explicit: true}
	continuity, err := manager.verifyRerunPRBaseContinuity(context.Background(), repo, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if continuity != nil {
		t.Fatalf("stale cached open PR retained continuity: %+v", continuity)
	}
	verified, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.PRState == nil || *verified.PRState != "closed" || verified.PRStateObservedAt == nil {
		t.Fatalf("authoritative closed state was not persisted: state=%v observed_at=%v", verified.PRState, verified.PRStateObservedAt)
	}
	logBytes, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "pr list --head feature --base quality-assurance") || !strings.Contains(logText, "pr view 42") {
		t.Fatalf("saved head/base tuple and persisted PR were not authoritatively checked:\n%s", logText)
	}
}

func TestRerunOpenPRRejectsCurrentConfiguredBaseBranch(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "rerun-current-pr-base-guard")

	configPath := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
	initialConfig := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\npr:\n  base_branch: quality-assurance\n"
	if err := os.WriteFile(configPath, []byte(initialConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", configPath)
	gitCmd(t, repo.WorkingPath, "commit", "-m", "target QA")
	qaSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "branch", "quality-assurance")
	gitCmd(t, repo.WorkingPath, "push", "gate", "quality-assurance")

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "staging")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "staging.txt"), []byte("staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "staging.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "add staging change")
	stagingSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "staging")

	selectedRun, err := database.InsertRun(repo.ID, "staging", stagingSHA, qaSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRBaseBranch(selectedRun.ID, "quality-assurance", true); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(selectedRun.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(selectedRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	gitCmd(t, repo.WorkingPath, "checkout", "-B", "main", qaSHA)
	currentConfig := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\npr:\n  base_branch: staging\n"
	if err := os.WriteFile(configPath, []byte(currentConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", configPath)
	gitCmd(t, repo.WorkingPath, "commit", "-m", "target staging")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")

	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)
	_, err = manager.HandleRerun(context.Background(), repo.ID, "staging", selectedRun.ID, nil, "")
	if err == nil {
		t.Fatal("rerun on the current configured PR base branch was accepted")
	}
	if !strings.Contains(err.Error(), "configured PR base branch") || !strings.Contains(err.Error(), "feature branch") {
		t.Fatalf("rerun rejection is not actionable: %v", err)
	}
}

func writeRerunPRBaseMockGH(t *testing.T, openBase, persistedState string) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gh.log")
	if runtime.GOOS == "windows" {
		path := filepath.Join(binDir, "gh.bat")
		openRule := ""
		if openBase != "" {
			openRule = "echo %* | findstr /C:\"pr list --head feature --base " + openBase + "\" >nul && (echo [{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\"}]& exit /b 0)\r\n"
		}
		stateRule := ""
		if persistedState != "" {
			stateRule = "echo %* | findstr /C:\"pr view 42 \" >nul && (echo " + persistedState + "& exit /b 0)\r\n"
		}
		script := "@echo off\r\necho %*>>\"" + logPath + "\"\r\necho %* | findstr /C:\"auth status --hostname github.com\" >nul && exit /b 0\r\n" + openRule + "echo %* | findstr /C:\"pr list \" >nul && (echo []& exit /b 0)\r\n" + stateRule + "echo %* | findstr /C:\"pr edit 42 \" >nul && exit /b 0\r\necho %* | findstr /C:\"pr create \" >nul && (echo https://github.com/test/repo/pull/99& exit /b 0)\r\nexit /b 1\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return binDir, logPath
	}
	path := filepath.Join(binDir, "gh")
	openRule := ""
	if openBase != "" {
		openRule = `  *"pr list --head feature --base ` + openBase + `"*) printf '%s\n' '[{"number":42,"url":"https://github.com/test/repo/pull/42"}]'; exit 0 ;;
`
	}
	stateRule := ""
	if persistedState != "" {
		stateRule = `  *"pr view 42 "*) printf '%s\n' '` + persistedState + `'; exit 0 ;;
`
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >>` + shellQuoteForTest(logPath) + `
case "$*" in
  "auth status --hostname github.com") exit 0 ;;
` + openRule + `  "pr list "*) printf '%s\n' '[]'; exit 0 ;;
` + stateRule + `  "pr edit 42 "*) exit 0 ;;
  "pr create "*) printf '%s\n' 'https://github.com/test/repo/pull/99'; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, logPath
}

func TestEnsureConfiguredPRBaseBranch_RejectsMissingAndUnsafeTargets(t *testing.T) {
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, "", "init", "--bare", upstream)
	workDir := t.TempDir()
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "remote", "add", "origin", upstream)
	repo := &db.Repo{UpstreamURL: upstream, DefaultBranch: "main"}

	for _, branch := range []string{"missing-branch", "refs/heads/main", "-unsafe"} {
		t.Run(strings.ReplaceAll(branch, "/", "_"), func(t *testing.T) {
			cfg := &config.Config{PR: config.PR{BaseBranch: branch}}
			err := ensureConfiguredPRBaseBranch(context.Background(), workDir, repo, cfg)
			if err == nil {
				t.Fatalf("expected configured PR base %q to fail", branch)
			}
			if !strings.Contains(err.Error(), "pr.base_branch") || !strings.Contains(err.Error(), branch) {
				t.Fatalf("error %q is not actionable for target %q", err, branch)
			}
		})
	}
}

func TestLoadRecoveredConfig_PRBaseComesFromTrustedDefaultBranch(t *testing.T) {
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, "", "init", "--bare", upstream)
	seed := t.TempDir()
	gitCmd(t, seed, "init", "--initial-branch=main")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, ".no-mistakes.yaml"), []byte("pr:\n  base_branch: quality-assurance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "trusted config")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, seed, "branch", "quality-assurance")
	gitCmd(t, seed, "push", "origin", "quality-assurance")

	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, ".no-mistakes.yaml"), []byte("pr:\n  base_branch: attacker-target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".no-mistakes.yaml")
	gitCmd(t, seed, "commit", "-m", "attempt redirect")
	gitCmd(t, seed, "push", "origin", "feature")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "checkout", "feature")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(nil, p, nil)
	initialBase := "quality-assurance"
	cfg, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "run", PRBaseBranch: &initialBase, PRBaseBranchExplicit: true}, &db.Repo{UpstreamURL: upstream, DefaultBranch: "main"}, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PR.BaseBranch != "quality-assurance" {
		t.Fatalf("effective PR base = %q, want trusted quality-assurance", cfg.PR.BaseBranch)
	}
	if _, err := git.ResolveRef(context.Background(), workDir, "refs/remotes/origin/quality-assurance"); err != nil {
		t.Fatalf("trusted configured PR base was not resolved: %v", err)
	}
	if _, err := git.ResolveRef(context.Background(), workDir, "refs/remotes/origin/attacker-target"); err == nil {
		t.Fatal("pushed branch redirected PR base fetch")
	}

	gitCmd(t, seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, ".no-mistakes.yaml"), []byte("pr:\n  base_branch: staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".no-mistakes.yaml")
	gitCmd(t, seed, "commit", "-m", "change future PR target")
	gitCmd(t, seed, "branch", "staging")
	gitCmd(t, seed, "push", "origin", "main", "staging")

	originalBase := "quality-assurance"
	_, err = mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "protected-parked-run", Branch: "staging", PRBaseBranch: &originalBase, PRBaseBranchExplicit: true}, &db.Repo{UpstreamURL: upstream, DefaultBranch: "main"}, workDir)
	if err == nil || !strings.Contains(err.Error(), "configured PR base branch") {
		t.Fatalf("recovery did not reject the current configured PR base branch: %v", err)
	}
	cfg, err = mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "parked-run", Branch: "feature", PRBaseBranch: &originalBase, PRBaseBranchExplicit: true}, &db.Repo{UpstreamURL: upstream, DefaultBranch: "main"}, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PR.BaseBranch != originalBase {
		t.Fatalf("recovered PR base = %q, want persisted %q", cfg.PR.BaseBranch, originalBase)
	}
	if !cfg.PR.HasExplicitBaseBranch() {
		t.Fatal("recovered configured PR base lost explicit-target semantics")
	}

	legacyBase := "main"
	for _, run := range []*db.Run{
		{ID: "persisted-unset-parked-run", PRBaseBranch: &legacyBase},
		{ID: "legacy-null-parked-run"},
	} {
		cfg, err = mgr.loadRecoveredConfig(context.Background(), run, &db.Repo{UpstreamURL: upstream, DefaultBranch: "main"}, workDir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PR.BaseBranch != legacyBase {
			t.Fatalf("recovered legacy PR base = %q, want persisted %q", cfg.PR.BaseBranch, legacyBase)
		}
		if cfg.PR.HasExplicitBaseBranch() {
			t.Fatal("recovered unset PR base acquired explicit-target semantics")
		}
	}
}

func TestLoadRecoveredConfig_BoundsFetchAndFailsClosed(t *testing.T) {
	oldTimeout := recoveredConfigFetchTimeout
	recoveredConfigFetchTimeout = 20 * time.Millisecond
	t.Cleanup(func() { recoveredConfigFetchTimeout = oldTimeout })

	fetchResult := make(chan error, 1)
	oldFetch := fetchRecoveredRemoteBranch
	fetchRecoveredRemoteBranch = func(ctx context.Context, _, _, _ string) error {
		select {
		case <-ctx.Done():
			fetchResult <- ctx.Err()
			return ctx.Err()
		case <-time.After(time.Second):
			err := errors.New("fetch context was not bounded")
			fetchResult <- err
			return err
		}
	}
	t.Cleanup(func() { fetchRecoveredRemoteBranch = oldFetch })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("commands:\n  lint: echo pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewRunManager(nil, p, nil)
	started := time.Now()
	// The bounded fetch times out, leaving trustedSHA empty. Under the
	// disable_project_settings security boundary a trusted-config fetch failure
	// must ABORT (not silently proceed as "not opted out"), so this now returns
	// an error rather than a config with empty commands.
	_, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "run"}, &db.Repo{DefaultBranch: "main"}, workDir)
	if err == nil {
		t.Fatal("expected loadRecoveredConfig to abort on trusted-config fetch failure")
	}
	if !strings.Contains(err.Error(), "disable_project_settings") {
		t.Fatalf("abort error should name the boundary, got: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("load recovered config took %s, want under 1s", elapsed)
	}
	if err := <-fetchResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch error = %v, want deadline exceeded", err)
	}
}

// TestLoadTrustedRepoConfig_FailClosedOnFetchFailure is the regression test for
// the supply-chain RCE review item #1: when the default-branch fetch fails,
// startRun passes an empty trustedSHA, and loadTrustedRepoConfig MUST return
// nil even though a (potentially stale) origin/<default> ref is still present
// in the worktree's shared refs. Reading that stale ref would run a command
// the live default branch has already removed. EffectiveRepoConfig then forces
// empty commands, so the stale command does not run.
func TestLoadTrustedRepoConfig_FailClosedOnFetchFailure(t *testing.T) {
	ctx := context.Background()

	// Source repo whose default branch carries a "stale" lint command — the
	// kind of command a maintainer has since removed but a stale ref would
	// still serve.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo stale-command\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "stale command on default branch")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	// The gate bare repo is its own origin so the linked worktree can fetch
	// main exactly the way startRun does.
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// Linked worktree sharing the bare repo's refs and config.
	wt := filepath.Join(t.TempDir(), "wt")
	headSHA := gitOutput(t, src, "rev-parse", "HEAD")
	if err := git.WorktreeAdd(ctx, bare, wt, headSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// A previous successful fetch left origin/main present in the shared
	// refs — this is the stale ref the old code read after a fetch failure.
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("prime origin/main: %v", err)
	}
	ok, err := git.RefExists(ctx, wt, "origin/main")
	if err != nil {
		t.Fatalf("RefExists origin/main: %v", err)
	}
	if !ok {
		t.Fatal("precondition failed: origin/main should be present (the stale ref)")
	}

	// THE REGRESSION: fetch "failed" → startRun passes an empty trustedSHA.
	// Even with origin/main present and carrying the stale command, the
	// trusted config must be nil so the stale command cannot run.
	got := loadTrustedRepoConfig(ctx, wt, "", "test-run")
	if got != nil {
		t.Fatalf("expected nil trusted config on empty SHA (fetch failure); got commands.lint=%q", got.Commands.Lint)
	}

	// And the effective config drops the pushed-branch command too — the
	// secure default, not a fallback to a stale or hostile copy.
	pushed := &config.RepoConfig{Commands: config.Commands{Lint: "echo pushed-branch-command"}}
	eff := config.EffectiveRepoConfig(pushed, got, false)
	if eff.Commands.Lint != "" {
		t.Fatalf("SECURITY REGRESSION: command would run after fetch failure: %q", eff.Commands.Lint)
	}
}

// TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch proves the
// complementary side of review item #1: when the fetch succeeds, the trusted
// config is read at the exact resolved SHA (not the origin/<default> ref
// name), so it reflects the freshly fetched default-branch tip rather than a
// stale ref value. Advancing the default branch and re-fetching must yield the
// new command, not the old one.
func TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch(t *testing.T) {
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo stale-A\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "stale command A")
	staleSHA := gitOutput(t, src, "rev-parse", "HEAD")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// Advance the default branch to a fresh command and push.
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo fresh-B\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "fresh command B")
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")
	freshSHA := gitOutput(t, src, "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := git.WorktreeAdd(ctx, bare, wt, staleSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("fetch main: %v", err)
	}
	resolved, err := git.ResolveRef(ctx, wt, "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}
	if resolved != freshSHA {
		t.Fatalf("resolved SHA %s != fresh default-branch tip %s", resolved, freshSHA)
	}

	trusted := loadTrustedRepoConfig(ctx, wt, resolved, "test-run")
	if trusted == nil {
		t.Fatal("expected trusted config at the pinned fresh SHA")
	}
	if trusted.Commands.Lint != "echo fresh-B" {
		t.Fatalf("trusted lint = %q, want fresh-B (read at pinned SHA, not stale ref)", trusted.Commands.Lint)
	}
}
