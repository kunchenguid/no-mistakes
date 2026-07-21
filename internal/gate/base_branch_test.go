package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func baseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func setupBaseBranchRepo(t *testing.T, mainConfig string) (work, upstream string) {
	t.Helper()
	work = setupTestRepo(t)
	upstream = baseGit(t, work, "remote", "get-url", "origin")
	baseGit(t, work, "checkout", "-B", "main")
	if mainConfig != "" {
		if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte(mainConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		baseGit(t, work, "add", ".no-mistakes.yaml")
		baseGit(t, work, "commit", "-m", "configure base")
	}
	baseGit(t, work, "push", "origin", "main")
	baseGit(t, upstream, "symbolic-ref", "HEAD", "refs/heads/main")

	baseGit(t, work, "checkout", "-b", "staging")
	baseGit(t, work, "commit", "--allow-empty", "-m", "staging")
	baseGit(t, work, "push", "origin", "staging")
	baseGit(t, work, "checkout", "main")
	baseGit(t, work, "checkout", "-b", "release/v2")
	baseGit(t, work, "commit", "--allow-empty", "-m", "release")
	baseGit(t, work, "push", "origin", "release/v2")
	baseGit(t, work, "checkout", "main")
	return work, upstream
}

func TestInitWithOptionsExplicitBaseBranchWinsTrustedConfig(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "base_branch: release/v2\n")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	base := "staging"

	repo, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &base})
	if err != nil {
		t.Fatalf("InitWithOptions: %v", err)
	}
	if repo.DefaultBranch != "main" || repo.BaseBranch != "staging" {
		t.Fatalf("branches = default %q base %q, want main/staging", repo.DefaultBranch, repo.BaseBranch)
	}
}

func TestInitWithOptionsReadsBaseOnlyFromFreshTrustedBranch(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "base_branch: staging\n")
	baseGit(t, work, "checkout", "-b", "feature/untrusted")
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("base_branch: release/v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	repo, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{})
	if err != nil {
		t.Fatalf("InitWithOptions: %v", err)
	}
	if repo.DefaultBranch != "main" || repo.BaseBranch != "staging" {
		t.Fatalf("branches = default %q base %q, want trusted main/staging", repo.DefaultBranch, repo.BaseBranch)
	}
}

func TestInitWithOptionsPreservesAndClearsBaseBranch(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	base := "staging"
	if _, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &base}); err != nil {
		t.Fatalf("initial init: %v", err)
	}

	preserved, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if preserved.BaseBranch != "staging" {
		t.Fatalf("plain refresh base = %q, want staging", preserved.BaseBranch)
	}

	cleared, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{ClearBaseBranch: true})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.BaseBranch != "" || cleared.EffectiveBaseBranch() != "main" {
		t.Fatalf("cleared base = %q effective %q, want empty/main", cleared.BaseBranch, cleared.EffectiveBaseBranch())
	}
}

func TestInitWithOptionsAppliesOneTrustedDelegationOnRefresh(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	base := "staging"
	if _, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &base}); err != nil {
		t.Fatalf("initial init: %v", err)
	}

	baseGit(t, work, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("base_branch: release/v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseGit(t, work, "add", ".no-mistakes.yaml")
	baseGit(t, work, "commit", "-m", "delegate next base")
	baseGit(t, work, "push", "origin", "staging")
	baseGit(t, work, "checkout", "main")

	refreshed, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.DefaultBranch != "main" || refreshed.BaseBranch != "release/v2" {
		t.Fatalf("branches = default %q base %q, want main/release/v2", refreshed.DefaultBranch, refreshed.BaseBranch)
	}
}

func TestInitWithOptionsFailedRefreshPreservesRegistration(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	base := "staging"
	repo, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &base})
	if err != nil {
		t.Fatalf("initial init: %v", err)
	}
	missing := "missing"
	if _, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &missing}); err == nil {
		t.Fatal("expected missing replacement to fail")
	}
	persisted, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.DefaultBranch != "main" || persisted.BaseBranch != "staging" {
		t.Fatalf("failed refresh changed registration: %#v", persisted)
	}
}

func TestInitWithOptionsRejectsBranchThatExistsOnlyOutsideParentOrigin(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "")
	fork := filepath.Join(gateTestTempDir(t), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", fork).CombinedOutput(); err != nil {
		t.Fatalf("init fork: %v: %s", err, out)
	}
	baseGit(t, work, "checkout", "-b", "fork-only")
	baseGit(t, work, "remote", "add", "fork", fork)
	baseGit(t, work, "push", "fork", "fork-only")

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	candidate := "fork-only"
	if _, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &candidate}); err == nil {
		t.Fatal("expected fork-only base branch to be rejected")
	}
	if repo, err := d.GetRepoByPath(work); err != nil {
		t.Fatal(err)
	} else if repo != nil {
		t.Fatalf("fork-only base persisted repo: %#v", repo)
	}
}

func TestInitWithOptionsRejectsMalformedConfigOnCandidateBase(t *testing.T) {
	work, _ := setupBaseBranchRepo(t, "commands: [\n")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	candidate := "staging"
	if _, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &candidate}); err == nil {
		t.Fatal("expected malformed candidate config to be rejected")
	}
	if repo, err := d.GetRepoByPath(work); err != nil {
		t.Fatal(err)
	} else if repo != nil {
		t.Fatalf("malformed candidate persisted repo: %#v", repo)
	}
}

func TestInitWithOptionsRejectsMissingOrUnsafeBaseWithoutPersisting(t *testing.T) {
	for _, candidate := range []string{"missing", "HEAD", "refs/heads/staging", "../staging"} {
		t.Run(strings.ReplaceAll(candidate, "/", "_"), func(t *testing.T) {
			work, _ := setupBaseBranchRepo(t, "")
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d := openTestDB(t, p)
			_, _, err := InitWithOptions(context.Background(), d, p, work, InitOptions{BaseBranch: &candidate})
			if err == nil {
				t.Fatalf("expected %q to be rejected", candidate)
			}
			got, getErr := d.GetRepoByPath(work)
			if getErr != nil {
				t.Fatalf("GetRepoByPath: %v", getErr)
			}
			if got != nil {
				t.Fatalf("repo was persisted after invalid base %q", candidate)
			}
		})
	}
}
