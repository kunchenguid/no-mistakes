package branchsync

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newReboundSupersessionFixture(t *testing.T, historicalRef string, published bool, olderFingerprints ...string) (*recoverFixture, *db.Run) {
	t.Helper()
	f := newRecoverFixture(t, types.RunRunning)
	mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/recover", "HEAD:refs/heads/existing")
	if historicalRef != "" {
		if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
			HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: historicalRef,
		}); err != nil {
			t.Fatal(err)
		}
	}
	older, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	olderFingerprint := TargetFingerprint(f.remote)
	if len(olderFingerprints) > 0 {
		olderFingerprint = olderFingerprints[0]
	}
	if err := f.db.RebindPublication(f.repo, older, "existing", "https://github.com/test/repo/pull/1", olderFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunErrorStatusWithVerifiedHead(older.ID, "failed with unpublished work", types.RunFailed, f.preserved); err != nil {
		t.Fatal(err)
	}
	f.run, err = f.db.GetRun(older.ID)
	if err != nil {
		t.Fatal(err)
	}
	tree := mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}")
	head := mustRun(t, f.gate, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-p", f.preserved, "-m", "replacement validation")
	newer, err := f.db.InsertRun(f.repo.ID, "feature/recover", head, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RebindPublication(f.repo, newer, "existing", ptr(f.run.PRURL), TargetFingerprint(f.remote)); err != nil {
		t.Fatal(err)
	}
	if published {
		mustRun(t, f.gate, "push", f.remote, head+":refs/heads/existing")
		mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", head)
		if err := f.db.UpdateRunPublication(newer.ID, db.PushBinding{
			HeadSHA: head, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/existing",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunCompleted, head); err != nil {
		t.Fatal(err)
	}
	newer, err = f.db.GetRun(newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, newer
}

func TestReboundSupersessionStatusSyncAndRecovery(t *testing.T) {
	t.Parallel()
	for _, historicalRef := range []string{"", "refs/heads/feature/recover", "refs/heads/existing"} {
		t.Run("historical_ref="+historicalRef, func(t *testing.T) {
			t.Parallel()
			f, newer := newReboundSupersessionFixture(t, historicalRef, true)
			beforeRefs := mustRun(t, f.gate, "show-ref")
			cached := f.service.InspectCached(f.ctx)
			if cached.Pipeline.RunID != newer.ID || cached.Pipeline.PushedHead != newer.HeadSHA || cached.State != StateAmbiguousContext || cached.Safety != "blocked_relation_unknown" || cached.Target.Ref != "refs/heads/existing" {
				t.Fatalf("cached replacement selection = %+v", cached)
			}
			if got := mustRun(t, f.gate, "show-ref"); got != beforeRefs {
				t.Fatal("cached inspection changed gate refs")
			}
			checked := f.service.Refresh(f.ctx)
			if checked.Pipeline.RunID != newer.ID || checked.Remote.ObservedHead != newer.HeadSHA || !CanApply(checked) {
				t.Fatalf("replacement sync check = %+v", checked)
			}
			recovered := f.service.Recover(f.ctx, true)
			if recovered.Pipeline.RunID != newer.ID || recovered.Recovered || recovered.Changed {
				t.Fatalf("superseded predecessor offered recovery = %+v", recovered)
			}
			applied := f.service.Apply(f.ctx)
			if applied.Pipeline.RunID != newer.ID || applied.State != StateSynchronized || !applied.Changed || applied.Local.Head != newer.HeadSHA {
				t.Fatalf("replacement synchronization = %+v", applied)
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
				t.Fatal("synchronization republished the custody source")
			}
			assertSupersessionPredecessorUnchanged(t, f)
		})
	}
}

func assertSupersessionPredecessorUnchanged(t *testing.T, f *recoverFixture) {
	t.Helper()
	older, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(older, f.run) {
		t.Fatalf("supersession rewrote predecessor: before=%+v after=%+v", f.run, older)
	}
}

func TestReboundSupersessionRefusesUnrelatedOrUnpublishedEvidence(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"unrelated destination", "old target mismatch", "new target mismatch", "unpublished replacement", "moved gate", "noncontaining push"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			var olderFingerprints []string
			if scenario == "old target mismatch" {
				olderFingerprints = []string{TargetFingerprint("https://example.com/other/repo.git")}
			}
			f, newer := newReboundSupersessionFixture(t, "refs/heads/feature/recover", scenario != "unpublished replacement", olderFingerprints...)
			switch scenario {
			case "unrelated destination", "new target mismatch":
				if err := f.db.UpdateRunStatus(newer.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				var err error
				newer, err = f.db.GetRun(newer.ID)
				if err != nil {
					t.Fatal(err)
				}
				branch, fingerprint := "existing", TargetFingerprint(f.remote)
				if scenario == "unrelated destination" {
					branch = "other"
				} else {
					fingerprint = TargetFingerprint(filepath.Join(filepath.Dir(f.remote), "other.git"))
				}
				if err := f.db.RebindPublication(f.repo, newer, branch, ptr(newer.PRURL), fingerprint); err != nil {
					t.Fatal(err)
				}
				if scenario == "unrelated destination" {
					mustRun(t, f.gate, "push", f.remote, newer.HeadSHA+":refs/heads/other")
					if err := f.db.UpdateRunPublication(newer.ID, db.PushBinding{
						HeadSHA: newer.HeadSHA, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/other",
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.db.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunCompleted, newer.HeadSHA); err != nil {
					t.Fatal(err)
				}
			case "moved gate":
				mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.preserved)
			case "noncontaining push":
				mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
				mustRun(t, f.remote, "update-ref", "refs/heads/existing", f.submitted)
				if err := f.db.UpdateRunPublication(newer.ID, db.PushBinding{
					HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/existing",
				}); err != nil {
					t.Fatal(err)
				}
			}
			beforeGate := mustRun(t, f.gate, "show-ref")
			beforeRemote := mustRun(t, f.remote, "show-ref")
			for _, state := range []State{f.service.InspectCached(f.ctx), f.service.Refresh(f.ctx), f.service.Apply(f.ctx)} {
				if state.Pipeline.RunID != f.run.ID || state.State != StatePipelineOwned || state.Changed {
					t.Fatalf("invalid replacement superseded predecessor = %+v", state)
				}
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
				t.Fatal("invalid replacement moved caller head")
			}
			if got := mustRun(t, f.gate, "show-ref"); got != beforeGate {
				t.Fatal("invalid replacement changed gate refs")
			}
			if got := mustRun(t, f.remote, "show-ref"); got != beforeRemote {
				t.Fatal("invalid replacement changed remote refs")
			}
			assertSupersessionPredecessorUnchanged(t, f)
			recovered := f.service.Recover(f.ctx, false)
			if !recovered.Recovered || recovered.Local.Head != f.preserved {
				t.Fatalf("recovery did not retain the unsuperseded head = %+v", recovered)
			}
			if got := mustRun(t, f.local, "rev-parse", recoverAnchorRef(f.run.ID)); got != f.preserved {
				t.Fatal("recovery did not preserve the unsuperseded predecessor")
			}
			older, err := f.db.GetRun(f.run.ID)
			if err != nil || older.CustodyReturnedAt == nil {
				t.Fatalf("recovery lost predecessor custody: %+v, %v", older, err)
			}
			older.CustodyReturnedAt, older.UpdatedAt = f.run.CustodyReturnedAt, f.run.UpdatedAt
			if !reflect.DeepEqual(older, f.run) {
				t.Fatal("recovery changed historical predecessor provenance")
			}
		})
	}
}

func TestReboundSupersessionKeepLocalRecoveryFiltersOnlyPublishedPredecessors(t *testing.T) {
	t.Parallel()
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpublished", true: "published"}[published], func(t *testing.T) {
			t.Parallel()
			f, _ := newReboundSupersessionFixture(t, "refs/heads/feature/recover", published)
			missing, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatusWithVerifiedHead(missing.ID, types.RunFailed, strings.Repeat("f", 40)); err != nil {
				t.Fatal(err)
			}
			cached := f.service.InspectCached(f.ctx)
			if cached.Pipeline.RunID != missing.ID || cached.NextAction == nil || cached.NextAction.Command != "no-mistakes axi sync --recover --keep-local" {
				t.Fatalf("missing head with replacement = %+v", cached)
			}
			state := f.service.Recover(f.ctx, true)
			if !state.Recovered || state.Local.Head != f.submitted {
				t.Fatalf("keep-local recovery with replacement = %+v", state)
			}
			latest, err := f.db.GetRun(missing.ID)
			if err != nil || latest.CustodyReturnedAt == nil {
				t.Fatalf("missing run custody not returned: %+v, %v", latest, err)
			}
			if published {
				assertSupersessionPredecessorUnchanged(t, f)
			} else {
				older, err := f.db.GetRun(f.run.ID)
				if err != nil || older.CustodyReturnedAt == nil {
					t.Fatalf("unpublished predecessor was filtered: %+v, %v", older, err)
				}
				if got := mustRun(t, f.gate, "rev-parse", recoverAnchorRef(f.run.ID)); got != f.preserved {
					t.Fatal("unpublished predecessor was not preserved")
				}
				older.CustodyReturnedAt, older.UpdatedAt = f.run.CustodyReturnedAt, f.run.UpdatedAt
				if !reflect.DeepEqual(older, f.run) {
					t.Fatal("keep-local recovery changed historical predecessor provenance")
				}
			}
		})
	}
}
