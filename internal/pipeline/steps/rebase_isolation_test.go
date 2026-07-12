package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestRebaseStep_FailedTierRestoresExactConflictCandidate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		firstError error
	}{
		{name: "schema-valid incomplete success"},
		{name: "operational error", firstError: errors.New("provider failed after writing")},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
			gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(dir, gitDir)
			}
			if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(".candidate-cache/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cacheDir := filepath.Join(dir, ".candidate-cache")
			emptyDir := filepath.Join(cacheDir, "empty")
			if err := os.MkdirAll(emptyDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(cacheDir, 0o750); err != nil {
				t.Fatal(err)
			}
			ignoredPath := filepath.Join(cacheDir, "ignored.txt")
			if err := os.WriteFile(ignoredPath, []byte("original ignored bytes\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			readOnlyDir := filepath.Join(cacheDir, "read-only")
			if err := os.Mkdir(readOnlyDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(readOnlyDir, "payload.txt"), []byte("read-only baseline\n"), 0o444); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(readOnlyDir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chmod(readOnlyDir, 0o755)
				_ = os.Chmod(filepath.Join(readOnlyDir, "payload.txt"), 0o644)
			})
			gitCmd(t, dir, "update-ref", "refs/heads/protected", baseSHA)

			fixerCalls := 0
			var originalOnto string
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if strings.Contains(opts.Prompt, "independently verifying") {
						return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
					}
					fixerCalls++
					rebaseDir := gitCmd(t, dir, "rev-parse", "--git-path", "rebase-merge")
					if !filepath.IsAbs(rebaseDir) {
						rebaseDir = filepath.Join(dir, rebaseDir)
					}
					ontoPath := filepath.Join(rebaseDir, "onto")
					if fixerCalls == 1 {
						if opts.AttemptIsolation == nil {
							t.Fatal("fixer invocation did not receive conflict-aware attempt isolation")
						}
						onto, err := os.ReadFile(ontoPath)
						if err != nil {
							return nil, err
						}
						originalOnto = string(onto)
						if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
							t.Fatalf("conflicted HEAD topology = %q, want detached HEAD", got)
						}

						if err := os.WriteFile(ignoredPath, []byte("malicious ignored mutation\n"), 0o600); err != nil {
							return nil, err
						}
						if err := os.Remove(emptyDir); err != nil {
							return nil, err
						}
						if err := os.Chmod(cacheDir, 0o777); err != nil {
							return nil, err
						}
						if err := os.Chmod(readOnlyDir, 0o755); err != nil {
							return nil, err
						}
						if err := os.RemoveAll(readOnlyDir); err != nil {
							return nil, err
						}
						gitCmd(t, dir, "update-ref", "refs/heads/protected", headSHA)
						gitCmd(t, dir, "symbolic-ref", "HEAD", "refs/heads/feature")
						if err := os.WriteFile(ontoPath, []byte("malicious-rebase-target\n"), 0o600); err != nil {
							return nil, err
						}
						if err := agent.ValidateSuccessfulAttempt(opts); err == nil {
							t.Fatal("successful-attempt boundary accepted an incomplete, topology-mutated rebase")
						}
						if tc.firstError != nil {
							return nil, tc.firstError
						}
						return &agent.Result{Output: json.RawMessage(`{"summary":"claimed success without continuing"}`)}, nil
					}

					if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
						t.Fatalf("restored HEAD topology = %q, want detached HEAD", got)
					}
					if got := gitCmd(t, dir, "rev-parse", "refs/heads/protected"); got != baseSHA {
						t.Fatalf("restored protected ref = %s, want %s", got, baseSHA)
					}
					ignored, err := os.ReadFile(ignoredPath)
					if err != nil {
						return nil, err
					}
					if got := string(ignored); got != "original ignored bytes\n" {
						t.Fatalf("restored ignored bytes = %q", got)
					}
					ignoredInfo, err := os.Lstat(ignoredPath)
					if err != nil {
						return nil, err
					}
					if got := ignoredInfo.Mode().Perm(); got != 0o640 {
						t.Fatalf("restored ignored mode = %o, want 640", got)
					}
					cacheInfo, err := os.Lstat(cacheDir)
					if err != nil {
						return nil, err
					}
					if got := cacheInfo.Mode().Perm(); got != 0o750 {
						t.Fatalf("restored directory mode = %o, want 750", got)
					}
					emptyInfo, err := os.Lstat(emptyDir)
					if err != nil {
						return nil, err
					}
					if !emptyInfo.IsDir() || emptyInfo.Mode().Perm() != 0o700 {
						t.Fatalf("restored empty directory = %s mode %o, want directory mode 700", emptyInfo.Mode().Type(), emptyInfo.Mode().Perm())
					}
					readOnlyInfo, err := os.Lstat(readOnlyDir)
					if err != nil {
						return nil, err
					}
					if readOnlyInfo.Mode().Perm() != 0o555 {
						t.Fatalf("restored read-only directory mode = %o, want 555", readOnlyInfo.Mode().Perm())
					}
					readOnlyPayload, err := os.ReadFile(filepath.Join(readOnlyDir, "payload.txt"))
					if err != nil {
						return nil, err
					}
					if string(readOnlyPayload) != "read-only baseline\n" {
						t.Fatalf("restored read-only payload = %q", readOnlyPayload)
					}
					onto, err := os.ReadFile(ontoPath)
					if err != nil {
						return nil, err
					}
					if got := string(onto); got != originalOnto {
						t.Fatalf("restored rebase metadata onto = %q, want %q", got, originalOnto)
					}
					if err := resolveConflictContinue(dir); err != nil {
						return nil, err
					}
					if err := agent.ValidateSuccessfulAttempt(opts); err != nil {
						t.Fatalf("successful-attempt boundary rejected clean completed rebase: %v", err)
					}
					return &agent.Result{Output: json.RawMessage(`{"summary":"resolved after exact failover"}`)}, nil
				},
			}

			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Run.Branch = "refs/heads/feature"
			sctx.Repo.UpstreamURL = upstream
			sctx.Config.Routing = config.DefaultRoutingConfig()
			sctx.Fixing = true

			if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
				t.Fatalf("clean tier failover failed: %v", err)
			}
			if fixerCalls != 2 {
				t.Fatalf("fixer calls = %d, want 2", fixerCalls)
			}
		})
	}
}

func TestRebaseStep_WriteCapableVerifierRestoresSealedCandidate(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(".verifier-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, ".verifier-cache")
	emptyDir := filepath.Join(cacheDir, "empty")
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(cacheDir, "ignored.txt")
	if err := os.WriteFile(ignoredPath, []byte("sealed ignored bytes\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "update-ref", "refs/heads/protected", baseSHA)

	var candidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if !strings.Contains(opts.Prompt, "independently verifying") {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
			}
			if opts.AttemptIsolation == nil {
				t.Fatal("verifier invocation did not receive sealed-candidate isolation")
			}
			candidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/feature" {
				t.Fatalf("completed candidate HEAD topology = %q, want feature branch", got)
			}
			if err := os.WriteFile(ignoredPath, []byte("verifier ignored mutation\n"), 0o600); err != nil {
				return nil, err
			}
			if err := os.Remove(emptyDir); err != nil {
				return nil, err
			}
			if err := os.Chmod(cacheDir, 0o777); err != nil {
				return nil, err
			}
			gitCmd(t, dir, "update-ref", "refs/heads/protected", headSHA)
			gitCmd(t, dir, "checkout", "--detach", candidateSHA)
			fakeRebaseDir := filepath.Join(gitDir, "rebase-merge")
			if err := os.Mkdir(fakeRebaseDir, 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(fakeRebaseDir, "onto"), []byte("forged\n"), 0o600); err != nil {
				return nil, err
			}
			if err := agent.ValidateSuccessfulAttempt(opts); err == nil {
				t.Fatal("successful-attempt boundary accepted verifier candidate mutation")
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected verifier purity violation to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != candidateSHA {
		t.Fatalf("restored candidate HEAD = %s, want %s", got, candidateSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("restored candidate HEAD topology = %q, want feature branch", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/protected"); got != baseSHA {
		t.Fatalf("restored protected ref = %s, want %s", got, baseSHA)
	}
	ignored, err := os.ReadFile(ignoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(ignored); got != "sealed ignored bytes\n" {
		t.Fatalf("restored ignored bytes = %q", got)
	}
	ignoredInfo, err := os.Lstat(ignoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := ignoredInfo.Mode().Perm(); got != 0o640 {
		t.Fatalf("restored ignored mode = %o, want 640", got)
	}
	cacheInfo, err := os.Lstat(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cacheInfo.Mode().Perm(); got != 0o750 {
		t.Fatalf("restored directory mode = %o, want 750", got)
	}
	emptyInfo, err := os.Lstat(emptyDir)
	if err != nil {
		t.Fatal(err)
	}
	if !emptyInfo.IsDir() || emptyInfo.Mode().Perm() != 0o700 {
		t.Fatalf("restored empty directory = %s mode %o, want directory mode 700", emptyInfo.Mode().Type(), emptyInfo.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); !os.IsNotExist(err) {
		t.Fatalf("forged rebase metadata survived restore: %v", err)
	}
}
func TestRebaseStep_RestoresVerifierHeadTopologyOnlyMutation(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	var candidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if !strings.Contains(opts.Prompt, "independently verifying") {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
			}
			candidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "--detach", candidateSHA)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected HEAD-topology-only verifier mutation to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != candidateSHA {
		t.Fatalf("restored candidate HEAD = %s, want %s", got, candidateSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("restored candidate HEAD topology = %q, want feature branch", got)
	}
}

func TestRebaseStep_OperationalVerifierErrorRestoresCandidate(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	var candidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if !strings.Contains(opts.Prompt, "independently verifying") {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
			}
			candidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(dir, "verifier-operational-mutation.txt"), []byte("must be discarded\n"), 0o600); err != nil {
				return nil, err
			}
			gitCmd(t, dir, "checkout", "--detach", candidateSHA)
			if err := agent.ValidateSuccessfulAttempt(opts); err == nil {
				t.Fatal("routed verifier success validation accepted mutation")
			}
			if err := opts.AttemptIsolation.RestoreFailedAttempt(); err != nil {
				return nil, err
			}
			return nil, errors.New("validate successful verifier attempt: candidate mutated")
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected operational verifier error to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != candidateSHA {
		t.Fatalf("restored candidate HEAD = %s, want %s", got, candidateSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("restored candidate HEAD topology = %q, want feature branch", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "verifier-operational-mutation.txt")); !os.IsNotExist(err) {
		t.Fatalf("operational verifier mutation survived restore: %v", err)
	}
}

func TestRebaseStep_FailedTierRestoresDetachedConflictTopology(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	fixerCalls := 0
	var conflictHead, conflictStatus string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				conflictHead = gitCmd(t, dir, "rev-parse", "HEAD")
				conflictStatus = gitCmd(t, dir, "status", "--porcelain")
				if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
					t.Fatalf("conflicted detached topology = %q, want HEAD", got)
				}
				gitCmd(t, dir, "symbolic-ref", "HEAD", "refs/heads/feature")
				return &agent.Result{Output: json.RawMessage(`{"summary":"incomplete detached attempt"}`)}, nil
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != conflictHead {
				t.Fatalf("restored detached conflict HEAD = %s, want %s", got, conflictHead)
			}
			if got := gitCmd(t, dir, "status", "--porcelain"); got != conflictStatus {
				t.Fatalf("restored detached conflict status = %q, want %q", got, conflictStatus)
			}
			if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
				t.Fatalf("restored detached topology = %q, want HEAD", got)
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved detached candidate"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("detached failover failed: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
		t.Fatalf("accepted detached topology = %q, want HEAD", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got == headSHA {
		t.Fatal("detached candidate did not complete the rebase")
	}
}

func TestRebaseStep_CompletedTierCannotMutateProtectedTopology(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(".completed-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(dir, ".completed-cache", "ignored.txt")
	if err := os.MkdirAll(filepath.Dir(ignoredPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignoredPath, []byte("completed baseline\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				gitCmd(t, dir, "update-ref", "refs/heads/forged-by-fixer", "HEAD")
				gitCmd(t, dir, "checkout", "--detach", "HEAD")
				if err := os.WriteFile(ignoredPath, []byte("completed fixer mutation\n"), 0o600); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved while mutating topology"}`)}, nil
			}
			if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
				t.Fatalf("protected-topology rollback did not restore detached conflict HEAD: %q", got)
			}
			if rebaseTestRefExists(dir, "refs/heads/forged-by-fixer") {
				t.Fatal("fixer-created shared ref survived tier rollback")
			}
			ignored, err := os.ReadFile(ignoredPath)
			if err != nil {
				return nil, err
			}
			if string(ignored) != "completed baseline\n" {
				t.Fatalf("completed-tier rollback retained ignored mutation: %q", ignored)
			}
			if !rebaseInProgress(context.Background(), dir) {
				t.Fatal("protected-topology rollback did not restore live conflict metadata")
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved without topology mutation"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("protected-topology failover failed: %v", err)
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want protected mutation followed by clean tier", fixerCalls)
	}
	if rebaseTestRefExists(dir, "refs/heads/forged-by-fixer") {
		t.Fatal("accepted rebase retained fixer-created shared ref")
	}
	if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("accepted HEAD topology = %q, want feature branch", got)
	}
}

func TestRebaseStep_LinkedDetachedWorktreeRestoresCommonAndLocalState(t *testing.T) {
	t.Parallel()

	mainDir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	dir := filepath.Join(t.TempDir(), "linked")
	gitCmd(t, mainDir, "worktree", "add", "--detach", dir, headSHA)
	gitLinkPath := filepath.Join(dir, ".git")
	gitLinkBytes, err := os.ReadFile(gitLinkPath)
	if err != nil {
		t.Fatal(err)
	}
	gitDir := gitCmd(t, dir, "rev-parse", "--absolute-git-dir")
	commonLinkPath := filepath.Join(gitDir, "commondir")
	backLinkPath := filepath.Join(gitDir, "gitdir")
	commonLinkBytes, err := os.ReadFile(commonLinkPath)
	if err != nil {
		t.Fatal(err)
	}
	backLinkBytes, err := os.ReadFile(backLinkPath)
	if err != nil {
		t.Fatal(err)
	}
	excludePath := gitCmd(t, dir, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}
	const excludeBytes = ".linked-cache/\n"
	if err := os.WriteFile(excludePath, []byte(excludeBytes), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(excludePath, 0o640); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, ".linked-cache")
	if err := os.MkdirAll(filepath.Join(cacheDir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(cacheDir, "ignored.txt")
	if err := os.WriteFile(ignoredPath, []byte("linked ignored baseline\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
					t.Fatalf("linked conflict HEAD topology = %q, want detached", got)
				}
				if err := os.WriteFile(excludePath, []byte("malicious common exclude\n"), 0o600); err != nil {
					return nil, err
				}
				if err := os.WriteFile(ignoredPath, []byte("mutated linked ignored\n"), 0o600); err != nil {
					return nil, err
				}
				if err := os.WriteFile(commonLinkPath, []byte("/forged-common\n"), 0o600); err != nil {
					return nil, err
				}
				if err := os.WriteFile(backLinkPath, []byte("/forged-back-link\n"), 0o600); err != nil {
					return nil, err
				}
				if err := os.WriteFile(gitLinkPath, []byte("gitdir: /forged\n"), 0o600); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"incomplete linked attempt"}`)}, nil
			}
			exclude, err := os.ReadFile(excludePath)
			if err != nil {
				return nil, err
			}
			if string(exclude) != excludeBytes {
				t.Fatalf("restored common exclude = %q, want %q", exclude, excludeBytes)
			}
			excludeInfo, err := os.Lstat(excludePath)
			if err != nil {
				return nil, err
			}
			if excludeInfo.Mode().Perm() != 0o640 {
				t.Fatalf("restored common exclude mode = %o, want 640", excludeInfo.Mode().Perm())
			}
			ignored, err := os.ReadFile(ignoredPath)
			if err != nil {
				return nil, err
			}
			if string(ignored) != "linked ignored baseline\n" {
				t.Fatalf("restored linked ignored bytes = %q", ignored)
			}
			restoredGitLink, err := os.ReadFile(gitLinkPath)
			if err != nil {
				return nil, err
			}
			if string(restoredGitLink) != string(gitLinkBytes) {
				t.Fatalf("restored worktree git link = %q, want %q", restoredGitLink, gitLinkBytes)
			}
			restoredCommonLink, err := os.ReadFile(commonLinkPath)
			if err != nil {
				return nil, err
			}
			if string(restoredCommonLink) != string(commonLinkBytes) {
				t.Fatalf("restored commondir link = %q, want %q", restoredCommonLink, commonLinkBytes)
			}
			restoredBackLink, err := os.ReadFile(backLinkPath)
			if err != nil {
				return nil, err
			}
			if string(restoredBackLink) != string(backLinkBytes) {
				t.Fatalf("restored gitdir back-link = %q, want %q", restoredBackLink, backLinkBytes)
			}
			if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
				t.Fatalf("restored linked HEAD topology = %q, want detached", got)
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved linked worktree"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("linked detached failover failed: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
		t.Fatalf("accepted linked HEAD topology = %q, want detached", got)
	}
}

func TestRebaseStep_CompletedTierModeMutationRestoresBeforeFailover(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitDir := gitCmd(t, dir, "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(".mode-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, ".mode-cache")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(cacheDir, "ignored.txt")
	if err := os.WriteFile(ignoredPath, []byte("sealed ignored bytes\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				if err := os.WriteFile(ignoredPath, []byte("malicious completed mutation\n"), 0o600); err != nil {
					return nil, err
				}
				if err := os.Chmod(cacheDir, 0o777); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved with hidden mode mutation"}`)}, nil
			}

			if !rebaseInProgress(context.Background(), dir) {
				t.Fatal("completed-candidate rollback did not restore live rebase metadata")
			}
			ignored, err := os.ReadFile(ignoredPath)
			if err != nil {
				return nil, err
			}
			if string(ignored) != "sealed ignored bytes\n" {
				t.Fatalf("restored ignored bytes = %q", ignored)
			}
			ignoredInfo, err := os.Lstat(ignoredPath)
			if err != nil {
				return nil, err
			}
			if ignoredInfo.Mode().Perm() != 0o640 {
				t.Fatalf("restored ignored mode = %o, want 640", ignoredInfo.Mode().Perm())
			}
			cacheInfo, err := os.Lstat(cacheDir)
			if err != nil {
				return nil, err
			}
			if cacheInfo.Mode().Perm() != 0o750 {
				t.Fatalf("restored directory mode = %o, want 750", cacheInfo.Mode().Perm())
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved cleanly after failover"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("mode-mutation failover failed: %v", err)
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want mutated tier followed by clean tier", fixerCalls)
	}
}

func TestRebaseStep_AcceptsLaterCommitAddingTrackedDirectory(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, _ := setupRebaseConflictRepo(t)
	addedDir := filepath.Join(dir, "added-later")
	if err := os.Mkdir(addedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	addedPath := filepath.Join(addedDir, "tracked.txt")
	if err := os.WriteFile(addedPath, []byte("added by later commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "added-later/tracked.txt")
	gitCmd(t, dir, "commit", "-m", "add later tracked path")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved multi-commit rebase"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("multi-commit rebase adding tracked path failed: %v", err)
	}
	content, err := os.ReadFile(addedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "added by later commit\n" {
		t.Fatalf("later tracked path content = %q", content)
	}
}

func TestRebaseStep_RestoresPackedDirectRefToSymbolicBaseline(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	const symbolicRef = "refs/remotes/origin/HEAD"
	const symbolicTarget = "refs/remotes/origin/main"
	gitCmd(t, dir, "symbolic-ref", symbolicRef, symbolicTarget)

	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				gitCmd(t, dir, "update-ref", "--no-deref", symbolicRef, headSHA)
				gitCmd(t, dir, "pack-refs", "--all", "--prune")
				return nil, errors.New("provider failed after packing mutated ref")
			}
			if got := gitCmd(t, dir, "symbolic-ref", symbolicRef); got != symbolicTarget {
				t.Fatalf("restored symbolic ref target = %q, want %q", got, symbolicTarget)
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved after symbolic-ref restore"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	_, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		if !strings.Contains(err.Error(), "does not support atomic symbolic-ref transactions") {
			t.Fatalf("symbolic-ref failover failed: %v", err)
		}
		if fixerCalls != 1 {
			t.Fatalf("unsupported Git fixer calls = %d, want fail closed before escalation", fixerCalls)
		}
		if state := captureRebaseRefForTest(t, dir, symbolicRef); state.symref != "" || state.oid != headSHA {
			t.Fatalf("unsupported Git changed packed ref state to %+v", state)
		}
		return
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want packed-ref failure followed by clean tier", fixerCalls)
	}
}

func TestRebaseStep_CompletedTierCannotPoisonOrigHead(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	origHeadPath := gitCmd(t, dir, "rev-parse", "--git-path", "ORIG_HEAD")
	if !filepath.IsAbs(origHeadPath) {
		origHeadPath = filepath.Join(dir, origHeadPath)
	}

	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				if err := os.WriteFile(origHeadPath, []byte(baseSHA+"\n"), 0o600); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved after poisoning ORIG_HEAD"}`)}, nil
			}
			if got := gitCmd(t, dir, "rev-parse", "ORIG_HEAD"); got != headSHA {
				t.Fatalf("restored ORIG_HEAD = %s, want %s", got, headSHA)
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved without metadata mutation"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("ORIG_HEAD failover failed: %v", err)
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want poisoned tier followed by clean tier", fixerCalls)
	}
}

func TestRebaseStep_CompletedTierCannotDropReplayedCommits(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	ontoSHA := gitCmd(t, dir, "rev-parse", "origin/main")
	origHeadPath := gitCmd(t, dir, "rev-parse", "--git-path", "ORIG_HEAD")
	if !filepath.IsAbs(origHeadPath) {
		origHeadPath = filepath.Join(dir, origHeadPath)
	}
	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				gitCmd(t, dir, "reset", "--hard", ontoSHA)
				if err := os.WriteFile(origHeadPath, []byte(headSHA+"\n"), 0o600); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved after dropping replayed commits"}`)}, nil
			}
			if !rebaseInProgress(context.Background(), dir) {
				t.Fatal("dropped-commit rollback did not restore live rebase")
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved without dropping commits"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("dropped-commit failover failed: %v", err)
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want malicious tier followed by clean tier", fixerCalls)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got == ontoSHA {
		t.Fatal("accepted rebase dropped every replayed commit")
	}
}

func TestRebaseStep_DoesNotClobberConcurrentLinkedWorktreeRef(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitCmd(t, dir, "branch", "concurrent", baseSHA)
	linkedDir := filepath.Join(t.TempDir(), "concurrent")
	gitCmd(t, dir, "worktree", "add", linkedDir, "concurrent")
	var concurrentSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := os.WriteFile(filepath.Join(linkedDir, "concurrent.txt"), []byte("concurrent commit\n"), 0o644); err != nil {
				return nil, err
			}
			gitCmd(t, linkedDir, "add", "concurrent.txt")
			gitCmd(t, linkedDir, "commit", "-m", "concurrent linked-worktree update")
			concurrentSHA = gitCmd(t, linkedDir, "rev-parse", "HEAD")
			return nil, errors.New("repair provider failed after concurrent commit")
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	_, err := (&RebaseStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "concurrent shared-ref change prevented exact rollback") {
		t.Fatalf("error = %v, want fatal concurrent shared-ref rollback error", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/concurrent"); got != concurrentSHA {
		t.Fatalf("concurrent linked-worktree ref = %s, want preserved %s", got, concurrentSHA)
	}
	if !rebaseInProgress(context.Background(), dir) {
		t.Fatal("failed rollback did not restore the pipeline worktree conflict state")
	}
}

func TestUnwindRejectedRebase_SurfacesResetFailure(t *testing.T) {
	t.Parallel()

	dir, _, _, headSHA := setupRebaseConflictRepo(t)
	gitCmd(t, dir, "update-ref", "ORIG_HEAD", headSHA)
	gitDir := gitCmd(t, dir, "rev-parse", "--absolute-git-dir")
	indexLock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(indexLock, []byte("block reset\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(indexLock)
	})

	err := unwindRejectedRebase(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "unwind rejected rebase candidate") {
		t.Fatalf("error = %v, want surfaced reset failure", err)
	}
}

func TestRestoreRebaseRefCAS_PreservesChangeAfterObservation(t *testing.T) {
	t.Parallel()

	dir, _, baseSHA, headSHA := setupRebaseConflictRepo(t)
	concurrentSHA := gitCmd(t, dir, "rev-parse", "origin/main")
	const ref = "refs/heads/cas-protected"
	gitCmd(t, dir, "update-ref", ref, baseSHA)
	gitCmd(t, dir, "update-ref", ref, headSHA)
	observed := rebaseRefState{oid: headSHA}
	hookRan := false
	ctx := context.WithValue(context.Background(), rebaseRefRestoreHookContextKey{}, func(name string) {
		if name == ref {
			hookRan = true
			gitCmd(t, dir, "update-ref", ref, concurrentSHA)
		}
	})
	err := restoreRebaseRefCAS(
		ctx,
		dir,
		ref,
		rebaseRefState{oid: baseSHA},
		true,
		observed,
		true,
	)
	if !hookRan {
		t.Fatal("direct ref restore did not reach the deterministic post-observation race hook")
	}
	if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("error = %v, want concurrent ref-change error", err)
	}
	if got := gitCmd(t, dir, "rev-parse", ref); got != concurrentSHA {
		t.Fatalf("ref after failed CAS = %s, want preserved concurrent %s", got, concurrentSHA)
	}
}

func TestRestoreRebaseRefCAS_PreservesSymbolicChangeAfterObservation(t *testing.T) {
	t.Parallel()

	dir, _, baseSHA, headSHA := setupRebaseConflictRepo(t)
	const ref = "refs/heads/cas-symbolic"
	gitCmd(t, dir, "update-ref", "refs/heads/symbolic-before", baseSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/symbolic-after", headSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/symbolic-concurrent", gitCmd(t, dir, "rev-parse", "origin/main"))
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/symbolic-before")
	before := captureRebaseRefForTest(t, dir, ref)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/symbolic-after")
	after := captureRebaseRefForTest(t, dir, ref)

	hookRan := false
	ctx := context.WithValue(context.Background(), rebaseRefRestoreHookContextKey{}, func(name string) {
		if name != ref {
			return
		}
		hookRan = true
		gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/symbolic-concurrent")
	})
	err := restoreRebaseRefCAS(ctx, dir, ref, before, true, after, true)
	if err == nil {
		t.Fatal("symbolic restore unexpectedly overwrote a concurrent ref change")
	}
	if !hookRan {
		t.Fatal("symbolic restore did not reach the deterministic post-observation race hook")
	}
	if got := gitCmd(t, dir, "symbolic-ref", ref); got != "refs/heads/symbolic-concurrent" {
		t.Fatalf("symbolic ref after failed CAS = %q, want preserved concurrent target", got)
	}
}

func TestRestoreRebaseRefCAS_RestoresSymbolicRefTransactionOrFailsClosed(t *testing.T) {
	t.Parallel()

	dir, _, baseSHA, headSHA := setupRebaseConflictRepo(t)
	const ref = "refs/heads/cas-symbolic-support"
	gitCmd(t, dir, "update-ref", "refs/heads/symbolic-support-before", baseSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/symbolic-support-after", headSHA)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/symbolic-support-before")
	before := captureRebaseRefForTest(t, dir, ref)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/symbolic-support-after")
	after := captureRebaseRefForTest(t, dir, ref)

	err := restoreRebaseRefCAS(context.Background(), dir, ref, before, true, after, true)
	if err != nil {
		if !strings.Contains(err.Error(), "does not support atomic symbolic-ref transactions") {
			t.Fatalf("symbolic restore error = %v, want explicit fail-closed compatibility error", err)
		}
		if got := gitCmd(t, dir, "symbolic-ref", ref); got != "refs/heads/symbolic-support-after" {
			t.Fatalf("unsupported Git changed symbolic ref to %q", got)
		}
		return
	}
	if got := gitCmd(t, dir, "symbolic-ref", ref); got != "refs/heads/symbolic-support-before" {
		t.Fatalf("transactional symbolic restore target = %q, want sealed target", got)
	}
}

func TestRestoreRebaseRefCAS_ReftableSymbolicTransaction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initCmd := exec.Command("git", "init", "--ref-format=reftable", dir)
	if output, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("installed Git does not support reftable repositories: %s", strings.TrimSpace(string(output)))
	}
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "tracked.txt")
	gitCmd(t, dir, "commit", "-m", "before")
	beforeOID := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "commit", "-am", "after")
	afterOID := gitCmd(t, dir, "rev-parse", "HEAD")

	const ref = "refs/heads/reftable-symbolic"
	gitCmd(t, dir, "update-ref", "refs/heads/reftable-before", beforeOID)
	gitCmd(t, dir, "update-ref", "refs/heads/reftable-after", afterOID)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/reftable-before")
	before := captureRebaseRefForTest(t, dir, ref)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/reftable-after")
	after := captureRebaseRefForTest(t, dir, ref)

	if err := restoreRebaseRefCAS(context.Background(), dir, ref, before, true, after, true); err != nil {
		t.Fatalf("restore reftable symbolic ref transaction: %v", err)
	}
	if got := gitCmd(t, dir, "symbolic-ref", ref); got != "refs/heads/reftable-before" {
		t.Fatalf("reftable symbolic restore target = %q, want sealed target", got)
	}
}

func TestRestoreRebaseRefCAS_PreservesAgentCreatedSymbolicRefRace(t *testing.T) {
	t.Parallel()

	dir, _, baseSHA, _ := setupRebaseConflictRepo(t)
	const ref = "refs/heads/cas-created-symbolic"
	gitCmd(t, dir, "update-ref", "refs/heads/created-symbolic-after", baseSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/created-symbolic-concurrent", baseSHA)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/created-symbolic-after")
	after := captureRebaseRefForTest(t, dir, ref)

	ctx := context.WithValue(context.Background(), rebaseRefRestoreHookContextKey{}, func(name string) {
		if name == ref {
			gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/created-symbolic-concurrent")
		}
	})
	err := restoreRebaseRefCAS(ctx, dir, ref, rebaseRefState{}, false, after, true)
	if err == nil {
		t.Fatal("agent-created symbolic ref restore overwrote a concurrent topology change")
	}
	if got := gitCmd(t, dir, "symbolic-ref", ref); got != "refs/heads/created-symbolic-concurrent" {
		t.Fatalf("agent-created symbolic ref after failed CAS = %q, want concurrent target", got)
	}
}

func TestRestoreRebaseRefCAS_FailsClosedRestoringDirectRefFromSymbolicValue(t *testing.T) {
	t.Parallel()

	dir, _, baseSHA, _ := setupRebaseConflictRepo(t)
	const ref = "refs/heads/cas-direct-from-symbolic"
	gitCmd(t, dir, "update-ref", ref, baseSHA)
	before := captureRebaseRefForTest(t, dir, ref)
	gitCmd(t, dir, "update-ref", "refs/heads/direct-symbolic-after", baseSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/direct-symbolic-concurrent", baseSHA)
	gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/direct-symbolic-after")
	after := captureRebaseRefForTest(t, dir, ref)

	ctx := context.WithValue(context.Background(), rebaseRefRestoreHookContextKey{}, func(name string) {
		if name == ref {
			gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/direct-symbolic-concurrent")
		}
	})
	err := restoreRebaseRefCAS(ctx, dir, ref, before, true, after, true)
	if err == nil {
		t.Fatal("direct restore overwrote a concurrent symbolic topology change")
	}
	if got := gitCmd(t, dir, "symbolic-ref", ref); got != "refs/heads/direct-symbolic-concurrent" {
		t.Fatalf("symbolic ref after unsupported direct restore = %q, want concurrent target", got)
	}
}

// TestRestoreRebaseRefCAS_DocumentsSameOIDDirectSymrefLimitation pins the one
// race no current Git backend can defend: a direct ref observed at object id X
// is concurrently replaced (after our final observation, before the write) with
// a symbolic ref that resolves to the same X. Git's no-deref old-oid
// verification resolves the symref before comparing, so the restore succeeds and
// silently discards that symbolic topology. This is verified against files,
// packed, and reftable backends through Git 2.46; closing it requires
// attempt-private ref isolation. The test documents the current behavior so a
// future isolation change (or a Git that grows a direct-only CAS) is caught.
func TestRestoreRebaseRefCAS_DocumentsSameOIDDirectSymrefLimitation(t *testing.T) {
	t.Parallel()

	dir, _, baseSHA, headSHA := setupRebaseConflictRepo(t)
	const ref = "refs/heads/cas-direct-same-oid-symref"
	gitCmd(t, dir, "update-ref", ref, headSHA)
	gitCmd(t, dir, "update-ref", "refs/heads/same-oid-target", headSHA)
	after := rebaseRefState{oid: headSHA}

	ctx := context.WithValue(context.Background(), rebaseRefRestoreHookContextKey{}, func(name string) {
		if name == ref {
			gitCmd(t, dir, "symbolic-ref", ref, "refs/heads/same-oid-target")
		}
	})
	// Documented limitation: the OID-CAS restore proceeds and clobbers the
	// same-OID symbolic topology back to the sealed direct baseline. It never
	// silently retargets to a DIFFERENT object id (that is caught by the OID
	// CAS, see TestRestoreRebaseRefCAS_PreservesChangeAfterObservation).
	if err := restoreRebaseRefCAS(ctx, dir, ref, rebaseRefState{oid: baseSHA}, true, after, true); err != nil {
		t.Fatalf("direct OID-CAS restore errored unexpectedly: %v", err)
	}
	restored := captureRebaseRefForTest(t, dir, ref)
	if restored.symref != "" || restored.oid != baseSHA {
		t.Fatalf("restored ref = %+v, want direct sealed baseline %s (known same-OID symref limitation)", restored, baseSHA)
	}
}

func captureRebaseRefForTest(t *testing.T, dir, name string) rebaseRefState {
	t.Helper()
	refs, err := captureRebaseRefs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := refs[name]
	if !ok {
		t.Fatalf("ref %s is missing", name)
	}
	return state
}

func TestRebaseStep_AcceptsLaterCommitDeletingTrackedOnlyDirectory(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, _ := setupRebaseConflictRepo(t)
	trackedDir := filepath.Join(dir, "deleted-later")
	if err := os.Mkdir(trackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	trackedPath := filepath.Join(trackedDir, "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("removed by later commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "deleted-later/tracked.txt")
	gitCmd(t, dir, "commit", "--amend", "--no-edit")
	gitCmd(t, dir, "rm", "deleted-later/tracked.txt")
	gitCmd(t, dir, "commit", "-m", "delete later tracked directory")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved multi-commit deletion"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("multi-commit rebase deleting tracked directory failed: %v", err)
	}
	if _, err := os.Stat(trackedDir); !os.IsNotExist(err) {
		t.Fatalf("later replayed deletion did not remove tracked-only directory: %v", err)
	}
}

func TestRebaseStep_DetachedTierCannotAbortAndDropReplayedCommits(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ontoSHA := gitCmd(t, dir, "rev-parse", "origin/main")
	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				gitCmd(t, dir, "rebase", "--abort")
				gitCmd(t, dir, "checkout", "--detach", ontoSHA)
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "forge completed rebase")
				return &agent.Result{Output: json.RawMessage(`{"summary":"aborted and dropped replayed commits"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved detached rebase"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("detached dropped-commit failover failed: %v", err)
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want malicious tier followed by clean tier", fixerCalls)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got == ontoSHA {
		t.Fatal("accepted detached rebase dropped every replayed commit")
	}
}

func TestRebaseStep_PreservesOriginalSymbolicHeadTopology(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitCmd(t, dir, "symbolic-ref", "refs/heads/feature-alias", "refs/heads/feature")
	gitCmd(t, dir, "symbolic-ref", "HEAD", "refs/heads/feature-alias")
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved symbolic branch"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("symbolic-HEAD rebase failed: %v", err)
	}
	headRef, err := captureRebaseHeadRef(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if headRef != "refs/heads/feature-alias" {
		t.Fatalf("accepted HEAD topology = %q, want original symbolic alias", headRef)
	}
}

func TestRebaseStep_AcceptsCompletionWithoutBranchReflog(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	gitCmd(t, dir, "config", "core.logAllRefUpdates", "false")
	gitDir := gitCmd(t, dir, "rev-parse", "--absolute-git-dir")
	if err := os.Remove(filepath.Join(gitDir, "logs", "refs", "heads", "feature")); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved without reflog"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("reflog-disabled rebase failed: %v", err)
	}
}

func TestRestoreRebaseRefCAS_PreservesNewRefAfterObservation(t *testing.T) {
	t.Parallel()

	for _, ref := range []string{
		"refs/heads/concurrent-new",
		"refs/remotes/origin/concurrent-new",
		"refs/tags/concurrent-new",
	} {
		ref := ref
		t.Run(ref, func(t *testing.T) {
			dir, _, baseSHA, headSHA := setupRebaseConflictRepo(t)
			gitCmd(t, dir, "update-ref", ref, headSHA)
			observed := rebaseRefState{oid: headSHA}
			hookRan := false
			ctx := context.WithValue(context.Background(), rebaseRefRestoreHookContextKey{}, func(name string) {
				if name == ref {
					hookRan = true
					gitCmd(t, dir, "update-ref", ref, baseSHA)
				}
			})
			err := restoreRebaseRefCAS(
				ctx,
				dir,
				ref,
				rebaseRefState{},
				false,
				observed,
				true,
			)
			if !hookRan {
				t.Fatal("new-ref restore did not reach the deterministic post-observation race hook")
			}
			if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
				t.Fatalf("error = %v, want concurrent new-ref error", err)
			}
			if got := gitCmd(t, dir, "rev-parse", ref); got != baseSHA {
				t.Fatalf("new concurrent ref = %s, want preserved %s", got, baseSHA)
			}
		})
	}
}

func TestRebaseStep_CompletedTierCannotMutateTrackedDirectoryMode(t *testing.T) {
	t.Parallel()

	dir, upstream, baseSHA, _ := setupRebaseConflictRepo(t)
	trackedDir := filepath.Join(dir, "tracked-directory")
	if err := os.Mkdir(trackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trackedDir, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "tracked-directory/tracked.txt")
	gitCmd(t, dir, "commit", "--amend", "--no-edit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	fixerCalls := 0
	var baselineMode os.FileMode
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				info, err := os.Lstat(trackedDir)
				if err != nil {
					return nil, err
				}
				baselineMode = info.Mode().Perm()
				if err := resolveConflictContinue(dir); err != nil {
					return nil, err
				}
				if err := os.Chmod(trackedDir, 0o777); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"resolved with directory mode mutation"}`)}, nil
			}
			info, err := os.Lstat(trackedDir)
			if err != nil {
				return nil, err
			}
			if info.Mode().Perm() != baselineMode {
				t.Fatalf("restored tracked directory mode = %o, want %o", info.Mode().Perm(), baselineMode)
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved without mode mutation"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("tracked-directory mode failover failed: %v", err)
	}
	if fixerCalls != 2 {
		t.Fatalf("fixer calls = %d, want mode-mutated tier followed by clean tier", fixerCalls)
	}
}

func rebaseTestRefExists(dir, ref string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}
