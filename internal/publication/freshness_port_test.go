package publication

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func prepareFreshnessView(t *testing.T, suffix string) (*candidatePortFixture, *GitCandidatePort, CandidateStepView) {
	t.Helper()
	fixture := newCandidatePortFixture(t, suffix)
	port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
	if err != nil {
		t.Fatalf("new Git candidate port: %v", err)
	}
	view, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepRebase)
	if err != nil {
		t.Fatalf("prepare Rebase candidate: %v", err)
	}
	t.Cleanup(func() {
		_ = port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepRebase)
	})
	return fixture, port, view
}

func TestGitCandidatePortCheckUpToDateAcceptsExactBoundAncestorBaseWithoutEffects(t *testing.T) {
	fixture, port, view := prepareFreshnessView(t, "freshness-pass")
	ctx := context.Background()
	beforeView, err := port.Inspect(ctx, fixture.publication.PublicationID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	beforeRefs := candidateGit(t, fixture.source, "for-each-ref", "--format=%(refname) %(objectname)")
	beforeConfig := candidateGit(t, fixture.source, "config", "--local", "--null", "--list")
	beforeStatus := candidateGit(t, fixture.source, "status", "--porcelain=v1", "--untracked-files=all")

	if err := port.CheckUpToDate(ctx, fixture.publication.PublicationID, view); err != nil {
		t.Fatalf("exact bound ancestor base rejected: %v", err)
	}

	afterView, err := port.Inspect(ctx, fixture.publication.PublicationID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterView, beforeView) {
		t.Fatalf("freshness check changed candidate view:\nbefore %#v\nafter  %#v", beforeView, afterView)
	}
	if got := candidateGit(t, fixture.source, "for-each-ref", "--format=%(refname) %(objectname)"); got != beforeRefs {
		t.Fatalf("freshness check changed source refs:\n%s\nwant:\n%s", got, beforeRefs)
	}
	if got := candidateGit(t, fixture.source, "config", "--local", "--null", "--list"); got != beforeConfig {
		t.Fatal("freshness check changed source config")
	}
	if got := candidateGit(t, fixture.source, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
		t.Fatalf("freshness check changed source worktree status: %q, want %q", got, beforeStatus)
	}
}

func TestGitCandidatePortCheckUpToDateAcceptsMultipleCandidateCommitsAfterExactBase(t *testing.T) {
	fixture := newCandidatePortFixture(t, "freshness-ancestor-not-parent")
	if err := os.WriteFile(filepath.Join(fixture.source, "second.txt"), []byte("second candidate commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, fixture.source, "add", "second.txt")
	candidateGit(t, fixture.source, "commit", "-m", "second candidate commit")
	headSHA := candidateGit(t, fixture.source, "rev-parse", "HEAD")
	treeSHA := candidateGit(t, fixture.source, "rev-parse", "HEAD^{tree}")

	request := fixture.parsed.Request
	request.Candidate.CommitSHA = headSHA
	request.Candidate.TreeSHA = treeSHA
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpdateRunStatus(fixture.publication.RunID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	publication, _, _, err := fixture.database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID: parsed.PublicationID, CanonicalRequest: parsed.CanonicalBytes,
		RepoID: fixture.repo.ID, CandidateRef: request.Candidate.HeadRef,
		BaseRef: request.Candidate.BaseRef, BaseSHA: request.Candidate.BaseSHA,
		HeadSHA: headSHA, TreeSHA: treeSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	view, err := port.PrepareStep(context.Background(), publication.PublicationID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.DisposeStep(context.Background(), publication.PublicationID, types.StepRebase) })
	if err := port.CheckUpToDate(context.Background(), publication.PublicationID, view); err != nil {
		t.Fatalf("freshness rejected exact bound ancestor with multiple candidate commits: %v", err)
	}
}

func TestGitCandidatePortCheckUpToDateRejectsBaseThatRequiresRebase(t *testing.T) {
	tests := map[string]func(*testing.T, *candidatePortFixture){
		"ahead": func(t *testing.T, fixture *candidatePortFixture) {
			candidateGit(t, fixture.source, "branch", "-f", "main", fixture.headSHA)
			candidateGit(t, fixture.source, "checkout", "main")
			if err := os.WriteFile(filepath.Join(fixture.source, "base-ahead.txt"), []byte("ahead\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			candidateGit(t, fixture.source, "add", "base-ahead.txt")
			candidateGit(t, fixture.source, "commit", "-m", "base ahead")
		},
		"diverged": func(t *testing.T, fixture *candidatePortFixture) {
			candidateGit(t, fixture.source, "checkout", "main")
			if err := os.WriteFile(filepath.Join(fixture.source, "base-diverged.txt"), []byte("diverged\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			candidateGit(t, fixture.source, "add", "base-diverged.txt")
			candidateGit(t, fixture.source, "commit", "-m", "base diverged")
		},
		"missing": func(t *testing.T, fixture *candidatePortFixture) {
			candidateGit(t, fixture.source, "branch", "-D", "main")
		},
		"noncommit": func(t *testing.T, fixture *candidatePortFixture) {
			blobPath := filepath.Join(t.TempDir(), "blob")
			if err := os.WriteFile(blobPath, []byte("not a commit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			blob := candidateGit(t, fixture.source, "hash-object", "-w", blobPath)
			if err := os.WriteFile(filepath.Join(fixture.source, ".git", "refs", "heads", "main"), []byte(blob+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutateBase := range tests {
		t.Run(name, func(t *testing.T) {
			fixture, port, view := prepareFreshnessView(t, "freshness-base-"+name)
			mutateBase(t, fixture)
			if err := port.CheckUpToDate(context.Background(), fixture.publication.PublicationID, view); err == nil {
				t.Fatalf("freshness accepted %s base", name)
			}
			if got := candidateGit(t, view.WorktreeDir, "rev-parse", "HEAD"); got != fixture.headSHA {
				t.Fatalf("failed freshness check changed view HEAD to %s, want H %s", got, fixture.headSHA)
			}
		})
	}
}

func TestGitCandidatePortCheckUpToDateRejectsExactBoundBaseOutsideCandidateHistory(t *testing.T) {
	fixture := newCandidatePortFixture(t, "freshness-exact-diverged")
	candidateGit(t, fixture.source, "checkout", "main")
	if err := os.WriteFile(filepath.Join(fixture.source, "diverged.txt"), []byte("diverged base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, fixture.source, "add", "diverged.txt")
	candidateGit(t, fixture.source, "commit", "-m", "diverged exact base")
	divergedBase := candidateGit(t, fixture.source, "rev-parse", "HEAD")

	request := fixture.parsed.Request
	request.Candidate.BaseSHA = divergedBase
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpdateRunStatus(fixture.publication.RunID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	publication, _, _, err := fixture.database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID: parsed.PublicationID, CanonicalRequest: parsed.CanonicalBytes,
		RepoID: fixture.repo.ID, CandidateRef: request.Candidate.HeadRef,
		BaseRef: request.Candidate.BaseRef, BaseSHA: request.Candidate.BaseSHA,
		HeadSHA: request.Candidate.CommitSHA, TreeSHA: request.Candidate.TreeSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	view, err := port.PrepareStep(context.Background(), publication.PublicationID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.DisposeStep(context.Background(), publication.PublicationID, types.StepRebase) })
	if err := port.CheckUpToDate(context.Background(), publication.PublicationID, view); err == nil {
		t.Fatal("freshness accepted an exact live and bound base outside H's history")
	}
}

func TestGitCandidatePortCheckUpToDateRejectsCandidateRefDrift(t *testing.T) {
	fixture, port, view := prepareFreshnessView(t, "freshness-candidate-drift")
	candidateGit(t, fixture.source, "update-ref", fixture.parsed.Request.Candidate.HeadRef, "refs/heads/main")
	if err := port.CheckUpToDate(context.Background(), fixture.publication.PublicationID, view); err == nil {
		t.Fatal("freshness accepted registered CandidateRef drift away from H")
	}
	if got := candidateGit(t, view.WorktreeDir, "rev-parse", "HEAD"); got != fixture.headSHA {
		t.Fatalf("candidate drift check changed disposable view HEAD: %s", got)
	}
}

func TestGitCandidatePortCheckUpToDateRejectsForeignStaleOrWrongStepView(t *testing.T) {
	t.Run("substituted fields", func(t *testing.T) {
		fixture, port, view := prepareFreshnessView(t, "freshness-substitution")
		for name, mutate := range map[string]func(*CandidateStepView){
			"worktree": func(value *CandidateStepView) { value.WorktreeDir = t.TempDir() },
			"scratch":  func(value *CandidateStepView) { value.ScratchDir = t.TempDir() },
			"contract": func(value *CandidateStepView) { value.WorkContractRaw = []byte("foreign") },
		} {
			t.Run(name, func(t *testing.T) {
				foreign := view
				foreign.WorkContractRaw = append([]byte(nil), view.WorkContractRaw...)
				mutate(&foreign)
				if err := port.CheckUpToDate(context.Background(), fixture.publication.PublicationID, foreign); err == nil {
					t.Fatalf("freshness accepted substituted %s", name)
				}
			})
		}
	})

	t.Run("stale disposed", func(t *testing.T) {
		fixture, port, view := prepareFreshnessView(t, "freshness-stale")
		if err := port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepRebase); err != nil {
			t.Fatal(err)
		}
		if err := port.CheckUpToDate(context.Background(), fixture.publication.PublicationID, view); err == nil {
			t.Fatal("freshness accepted a disposed view")
		}
	})

	t.Run("wrong step context", func(t *testing.T) {
		fixture := newCandidatePortFixture(t, "freshness-wrong-step")
		port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
		if err != nil {
			t.Fatal(err)
		}
		view, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
		})
		if err := port.CheckUpToDate(context.Background(), fixture.publication.PublicationID, view); err == nil {
			t.Fatal("freshness accepted a Review view as the Rebase context")
		}
	})

	t.Run("foreign publication", func(t *testing.T) {
		fixtureA, portA, viewA := prepareFreshnessView(t, "freshness-foreign-a")
		fixtureB, _, _ := prepareFreshnessView(t, "freshness-foreign-b")
		if err := portA.CheckUpToDate(context.Background(), fixtureB.publication.PublicationID, viewA); err == nil {
			t.Fatal("freshness accepted a view under a foreign publication ID")
		}
		if !strings.HasPrefix(viewA.WorktreeDir, fixtureA.root) {
			t.Fatal("fixture A view unexpectedly escaped its root")
		}
	})
}
