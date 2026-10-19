package branchsync

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func rewordFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	f := newRecoverFixtureAtSubmittedHead(t)
	pipeline := filepath.Join(filepath.Dir(f.local), "rewrite-pipeline")
	mustRun(t, f.gate, "clone", f.gate, pipeline)
	mustRun(t, pipeline, "checkout", "--detach", f.submitted)
	old, rewritten := f.submitted, f.submitted
	for i := range 4 {
		mustWrite(t, filepath.Join(pipeline, fmt.Sprintf("fix-%d", i)), fmt.Sprintf("fix %d\n", i))
		mustRun(t, pipeline, "add", ".")
		tree := mustRun(t, pipeline, "write-tree")
		old = mustRun(t, pipeline, "commit-tree", tree, "-p", old, "-m", fmt.Sprintf("original fix %d", i))
		rewritten = mustRun(t, pipeline, "commit-tree", tree, "-p", rewritten, "-m", fmt.Sprintf("reworded fix %d", i))
	}
	// The caller never received either history. The gate holds both as dangling
	// objects, as it does after deleting a failed run's detached worktree.
	for i, head := range []string{old, rewritten} {
		ref := fmt.Sprintf("refs/seed/%d", i)
		mustRun(t, pipeline, "update-ref", ref, head)
		mustRun(t, f.gate, "fetch", pipeline, ref)
	}
	if err := f.db.UpdateRunHeadSHA(f.run.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunErrorStatus(f.run.ID, "review fix rewrote recorded history", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	f.preserved = old
	f.run, _ = f.db.GetRun(f.run.ID)
	return f, rewritten
}

func TestRecoverRewordedPreservesFourFixesAndBothHistories(t *testing.T) {
	t.Parallel()
	f, rewritten := rewordFixture(t)
	state := f.service.RecoverReworded(f.ctx, f.preserved, rewritten)
	if !state.Recovered || !state.Changed {
		t.Fatalf("recovery: %+v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != rewritten {
		t.Fatalf("HEAD = %s, want %s", got, rewritten)
	}
	for i := range 4 {
		if got := mustRun(t, f.local, "show", fmt.Sprintf("HEAD:fix-%d", i)); got != fmt.Sprintf("fix %d", i) {
			t.Fatalf("lost fix %d: %s", i, got)
		}
	}
	for _, head := range []string{f.preserved, rewritten} {
		if got := mustRun(t, f.gate, "rev-parse", custody.RewordRef(f.run.ID, head)); got != head {
			t.Fatalf("lost history %s", head)
		}
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run.CustodyReturnedAt == nil || run.ReviewApprovedHeadSHA != nil || run.HeadSHA != rewritten {
		t.Fatalf("recovery must not approve review: %+v %v", run, err)
	}
}

func TestRecoverRewordedDoesNotInventCustodyOnAnotherBranch(t *testing.T) {
	t.Parallel()
	f, rewritten := rewordFixture(t)
	mustRun(t, f.local, "checkout", "-b", "unrelated", f.submitted)
	state := f.service.RecoverReworded(f.ctx, f.preserved, rewritten)
	if state.Recovered || state.State == StatePipelineOwned || state.Error == "" {
		t.Fatalf("recovery invented ownership: %+v", state)
	}
}

func TestRecoverRewordedRefusesUnprovenOrMovedEvidence(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"wrong-observation", "missing-object", "short-sha", "squash", "changed-intermediate-tree", "dirty", "moved-local", "moved-gate", "symbolic-gate", "conflicting-anchor", "symbolic-anchor", "approved", "active", "local-race", "gate-race", "database-race"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			f, rewritten := rewordFixture(t)
			recorded := f.preserved
			switch change {
			case "wrong-observation":
				recorded = f.submitted
			case "missing-object":
				rewritten = strings.Repeat("a", 40)
			case "short-sha":
				rewritten = rewritten[:8]
			case "squash":
				tree := mustRun(t, f.gate, "rev-parse", rewritten+"^{tree}")
				rewritten = mustRun(t, f.gate, "commit-tree", tree, "-p", f.submitted, "-m", "squashed")
			case "changed-intermediate-tree":
				commits := strings.Fields(mustRun(t, f.gate, "rev-list", "--reverse", f.submitted+".."+rewritten))
				parent := f.submitted
				for i, commit := range commits {
					tree := mustRun(t, f.gate, "rev-parse", commit+"^{tree}")
					if i == 0 {
						tree = mustRun(t, f.gate, "rev-parse", f.submitted+"^{tree}")
					}
					parent = mustRun(t, f.gate, "commit-tree", tree, "-p", parent, "-m", "changed intermediate")
				}
				rewritten = parent
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "dirty"), "local work")
			case "moved-local":
				mustRun(t, f.local, "commit", "--allow-empty", "-m", "moved")
			case "moved-gate":
				mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.preserved)
			case "symbolic-gate":
				mustRun(t, f.gate, "symbolic-ref", "refs/heads/"+f.run.Branch, "refs/heads/main")
			case "conflicting-anchor":
				mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.preserved)
			case "symbolic-anchor":
				mustRun(t, f.gate, "symbolic-ref", custody.RewordRef(f.run.ID, recorded), "refs/heads/main")
			case "approved":
				if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, recorded); err != nil {
					t.Fatal(err)
				}
			case "active":
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			case "local-race":
				f.service.beforeRecoverTerminalHeadPreserve = func() { mustWrite(t, filepath.Join(f.local, "raced"), "work") }
			case "gate-race":
				f.service.beforeRecoverTerminalHeadPreserve = func() { mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.preserved) }
			case "database-race":
				f.service.beforeRecoverTerminalHeadPreserve = func() {
					if err := f.db.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
						t.Fatal(err)
					}
				}
			}
			localBefore := mustRun(t, f.local, "rev-parse", "HEAD")
			refsBefore := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname)", "refs/no-mistakes/")
			state := f.service.RecoverReworded(f.ctx, recorded, rewritten)
			if state.Recovered || state.Changed || state.Error == "" {
				t.Fatalf("accepted %s: %+v", change, state)
			}
			if mustRun(t, f.local, "rev-parse", "HEAD") != localBefore || mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname)", "refs/no-mistakes/") != refsBefore {
				t.Fatal("refusal changed a branch or anchor")
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt != nil || run.TerminalHeadVerifiedAt != nil {
				t.Fatal("refusal stamped custody or verification")
			}
		})
	}
}
