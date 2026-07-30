package steps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newLiveAdoptionContext(t *testing.T) (*pipeline.StepContext, string, string) {
	t.Helper()
	dir, base, old := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", old)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, old, config.Commands{})
	return sctx, dir, old
}

func TestCommitAgentFixesAdoptsCleanStrictForwardSelfCommit(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "agent.txt")
	gitCmd(t, dir, "commit", "-m", "agent self commit")
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")

	if err := commitAgentFixes(sctx, types.StepDocument, "", "docs"); err != nil {
		t.Fatal(err)
	}
	assertLiveHeadAdopted(t, sctx, dir, old, candidate)
}

func TestCommitAgentFixesNoChangeIsTrueNoOp(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	if err := commitAgentFixes(sctx, types.StepDocument, "", "docs"); err != nil {
		t.Fatal(err)
	}
	if sctx.Run.HeadSHA != old || gitCmd(t, dir, "rev-parse", "refs/heads/feature") != old {
		t.Fatal("no-change path moved authority")
	}
	journal, err := sctx.DB.GetActiveRunHeadAdvance(sctx.Run.ID, old)
	if err != nil || journal != nil {
		t.Fatalf("no-change journal = %#v, %v", journal, err)
	}
}

func TestCommitAgentFixesAdoptsSelfCommitPlusDirtyLeftovers(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "agent.txt")
	gitCmd(t, dir, "commit", "-m", "agent self commit")
	selfCommit := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := commitAgentFixes(sctx, types.StepLint, "finish leftovers", "lint"); err != nil {
		t.Fatal(err)
	}
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")
	if candidate == selfCommit {
		t.Fatal("dirty leftovers did not produce the final pipeline commit")
	}
	if _, err := git.Run(sctx.Ctx, dir, "merge-base", "--is-ancestor", selfCommit, candidate); err != nil {
		t.Fatalf("self commit is not preserved beneath final candidate: %v", err)
	}
	assertLiveHeadAdopted(t, sctx, dir, old, candidate)
}

func TestCommitAgentFixesStatusReadErrorIsFatal(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index"), []byte("not a git index"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := commitAgentFixes(sctx, types.StepReview, "", "review")
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("status error = %v, want fatal status diagnostic", err)
	}
	if sctx.Run.HeadSHA != old {
		t.Fatal("status error advanced in-memory head")
	}
}

func TestCommitAgentFixesFinalStatusReadErrorIsFatal(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")
	gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	err := commitAgentFixesWithHooks(sctx, types.StepReview, "", "review", headAdoptionHooks{
		BeforeGateCAS: func() error {
			return os.WriteFile(filepath.Join(gitDir, "index"), []byte("not a git index"), 0o644)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("final status error = %v", err)
	}
	if sctx.Run.HeadSHA != old || gitCmd(t, dir, "rev-parse", "refs/heads/feature") != old {
		t.Fatal("final status error moved authority")
	}
	if got := gitCmd(t, dir, "rev-parse", liveHeadCandidateAnchorRef(sctx.Run.ID, candidate)); got != candidate {
		t.Fatalf("final status error lost anchor: %s", got)
	}
}

func TestCommitAgentFixesRefusesCommitHookResetToImmediateParent(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	selfCommit := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("must survive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	hook := filepath.Join(gitDir, "hooks", "post-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ngit reset --hard "+selfCommit+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := commitAgentFixes(sctx, types.StepDocument, "leftover", "docs")
	if err == nil || !strings.Contains(err.Error(), "exact staged tree") {
		t.Fatalf("commit-hook reset = %v", err)
	}
	if sctx.Run.HeadSHA != old {
		t.Fatal("commit-hook reset advanced in-memory authority")
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != old {
		t.Fatalf("commit-hook reset moved gate to %s", got)
	}
	assertLiveDBHead(t, sctx, old)
}

func TestCommitAgentFixesConflictingImmutableAnchorDenies(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "update-ref", liveHeadCandidateAnchorRef(sctx.Run.ID, candidate), old)
	err := commitAgentFixes(sctx, types.StepDocument, "", "docs")
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("anchor conflict = %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", liveHeadCandidateAnchorRef(sctx.Run.ID, candidate)); got != old {
		t.Fatalf("conflicting anchor overwritten with %s", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != old {
		t.Fatalf("anchor conflict moved gate to %s", got)
	}
	assertLiveDBHead(t, sctx, old)
}

func TestCommitAgentFixesGateCASRacePreservesConcurrentHead(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")
	tree := gitCmd(t, dir, "rev-parse", old+"^{tree}")
	racing := gitCmd(t, dir, "commit-tree", tree, "-p", old, "-m", "concurrent gate head")

	err := commitAgentFixesWithHooks(sctx, types.StepDocument, "", "docs", headAdoptionHooks{
		BeforeGateCAS: func() error {
			gitCmd(t, dir, "update-ref", "refs/heads/feature", racing, old)
			return nil
		},
	})
	if err == nil {
		t.Fatal("gate CAS race unexpectedly succeeded")
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != racing {
		t.Fatalf("concurrent gate head was overwritten: %s", got)
	}
	assertLiveDBHead(t, sctx, old)
	if got := gitCmd(t, dir, "rev-parse", liveHeadCandidateAnchorRef(sctx.Run.ID, candidate)); got != candidate {
		t.Fatalf("candidate anchor = %s, want %s", got, candidate)
	}
}

func TestCommitAgentFixesExactHeadRaceRefusesBeforeGateCAS(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	err := commitAgentFixesWithHooks(sctx, types.StepReview, "", "review", headAdoptionHooks{
		BeforeGateCAS: func() error {
			gitCmd(t, dir, "reset", "--hard", old)
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "HEAD changed") {
		t.Fatalf("HEAD race error = %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != old {
		t.Fatalf("HEAD race moved gate to %s", got)
	}
	assertLiveDBHead(t, sctx, old)
}

func TestCommitAgentFixesDBCASLossIsMonotonicAndRetryable(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")
	err := commitAgentFixesWithHooks(sctx, types.StepDocument, "", "docs", headAdoptionHooks{
		AfterGateCAS: func() error {
			if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			return nil
		},
	})
	if err == nil || !errors.Is(err, db.ErrRunHeadCAS) {
		t.Fatalf("DB CAS loss = %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != candidate {
		t.Fatalf("gate CAS was rolled back to %s", got)
	}
	assertLiveDBHead(t, sctx, old)
	if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepDocument, "", "docs"); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	assertLiveHeadAdopted(t, sctx, dir, old, candidate)
}

func TestCommitAgentFixesCrashBoundaryRetries(t *testing.T) {
	for _, boundary := range []string{"after_anchor", "after_gate", "after_db"} {
		t.Run(boundary, func(t *testing.T) {
			sctx, dir, old := newLiveAdoptionContext(t)
			writeSelfCommit(t, dir)
			candidate := gitCmd(t, dir, "rev-parse", "HEAD")
			crash := errors.New("simulated crash")
			hooks := headAdoptionHooks{}
			switch boundary {
			case "after_anchor":
				hooks.AfterAnchor = func() error { return crash }
			case "after_gate":
				hooks.AfterGateCAS = func() error { return crash }
			case "after_db":
				hooks.AfterDBCAS = func() error { return crash }
			}
			if err := commitAgentFixesWithHooks(sctx, types.StepLint, "", "lint", hooks); !errors.Is(err, crash) {
				t.Fatalf("boundary error = %v", err)
			}
			if sctx.Run.HeadSHA != old {
				t.Fatal("in-memory head advanced before durable method success")
			}
			if err := commitAgentFixes(sctx, types.StepLint, "", "lint"); err != nil {
				t.Fatalf("retry after %s: %v", boundary, err)
			}
			assertLiveHeadAdopted(t, sctx, dir, old, candidate)
		})
	}
}

func TestCommitAgentFixesFinalRaceAfterDurableCASIsHonest(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*testing.T, *pipeline.StepContext, string, string)
	}{
		{"head", func(t *testing.T, sctx *pipeline.StepContext, dir, old string) {
			gitCmd(t, dir, "reset", "--hard", old)
		}},
		{"gate", func(t *testing.T, sctx *pipeline.StepContext, dir, old string) {
			candidate := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
			tree := gitCmd(t, dir, "rev-parse", old+"^{tree}")
			racing := gitCmd(t, dir, "commit-tree", tree, "-p", old, "-m", "late gate race")
			gitCmd(t, dir, "update-ref", "refs/heads/feature", racing, candidate)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sctx, dir, old := newLiveAdoptionContext(t)
			writeSelfCommit(t, dir)
			candidate := gitCmd(t, dir, "rev-parse", "HEAD")
			err := commitAgentFixesWithHooks(sctx, types.StepLint, "", "lint", headAdoptionHooks{
				AfterDBCAS: func() error {
					tt.mutate(t, sctx, dir, old)
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "durably adopted") {
				t.Fatalf("final race = %v", err)
			}
			if sctx.Run.HeadSHA != old {
				t.Fatal("final race advanced in-memory authority")
			}
			assertLiveDBHead(t, sctx, candidate)
			journal, journalErr := sctx.DB.GetActiveRunHeadAdvance(sctx.Run.ID, candidate)
			if journalErr != nil || journal == nil || journal.ExpectedHead != old || journal.AnchorRef != liveHeadCandidateAnchorRef(sctx.Run.ID, candidate) {
				t.Fatalf("final race journal = %#v, %v", journal, journalErr)
			}
		})
	}
}

func TestCommitAgentFixesUsesProvidedBranchSerialization(t *testing.T) {
	sctx, dir, old := newLiveAdoptionContext(t)
	writeSelfCommit(t, dir)
	locked := false
	entries := 0
	sctx.BranchLock = func() func() {
		if locked {
			t.Fatal("branch lock re-entered")
		}
		locked = true
		entries++
		return func() { locked = false }
	}
	if err := commitAgentFixesWithHooks(sctx, types.StepDocument, "", "docs", headAdoptionHooks{
		AfterAnchor: func() error {
			if !locked {
				t.Fatal("candidate anchor escaped branch serialization")
			}
			return nil
		},
		AfterDBCAS: func() error {
			if !locked {
				t.Fatal("database CAS escaped branch serialization")
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if locked || entries != 1 {
		t.Fatalf("branch lock final state locked=%v entries=%d", locked, entries)
	}
	candidate := gitCmd(t, dir, "rev-parse", "HEAD")
	assertLiveHeadAdopted(t, sctx, dir, old, candidate)
}

func writeSelfCommit(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "agent.txt")
	gitCmd(t, dir, "commit", "-m", "agent self commit")
}

func assertLiveHeadAdopted(t *testing.T, sctx *pipeline.StepContext, dir, old, candidate string) {
	t.Helper()
	if candidate == old || sctx.Run.HeadSHA != candidate {
		t.Fatalf("in-memory head = %s, candidate = %s, old = %s", sctx.Run.HeadSHA, candidate, old)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != candidate {
		t.Fatalf("gate branch = %s, want %s", got, candidate)
	}
	assertLiveDBHead(t, sctx, candidate)
	if got := gitCmd(t, dir, "rev-parse", liveHeadCandidateAnchorRef(sctx.Run.ID, candidate)); got != candidate {
		t.Fatalf("candidate anchor = %s, want %s", got, candidate)
	}
	journal, err := sctx.DB.GetActiveRunHeadAdvance(sctx.Run.ID, candidate)
	if err != nil || journal == nil || journal.RunID != sctx.Run.ID || journal.RepoID != sctx.Run.RepoID ||
		journal.Branch != sctx.Run.Branch || journal.ExpectedHead != old || journal.Candidate != candidate ||
		journal.AnchorRef != liveHeadCandidateAnchorRef(sctx.Run.ID, candidate) {
		t.Fatalf("durable head journal = %#v, %v", journal, err)
	}
}

func assertLiveDBHead(t *testing.T, sctx *pipeline.StepContext, want string) {
	t.Helper()
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run == nil {
		t.Fatalf("reload run: %v, %#v", err, run)
	}
	if run.HeadSHA != want {
		t.Fatalf("database head = %s, want %s", run.HeadSHA, want)
	}
}
