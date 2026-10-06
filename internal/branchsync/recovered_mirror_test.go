package branchsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newReturnedRebasedMirrorFixture(t *testing.T, dropContent bool) *recoverFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "base.txt"), "base\n")
	mustRun(t, local, "add", "base.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "bin", "fm-codex-appserver.py"), "submitted adapter\n")
	mustWrite(t, filepath.Join(local, "tests", "fm-codex-appserver-check.py"), "submitted checks\n")
	mustRun(t, local, "add", "bin/fm-codex-appserver.py", "tests/fm-codex-appserver-check.py")
	mustRun(t, local, "commit", "-m", "submitted feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "main.txt"), "advanced base\n")
	mustRun(t, local, "add", "main.txt")
	mustRun(t, local, "commit", "-m", "advance main")
	mustRun(t, local, "checkout", "feature/recover")
	mustRun(t, local, "rebase", "main")
	if dropContent {
		mustRun(t, local, "rm", "bin/fm-codex-appserver.py", "tests/fm-codex-appserver-check.py")
		mustRun(t, local, "commit", "-m", "invalid recovered head drops submitted files")
	}
	recovered := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "reset", "--hard", submitted)

	gateDir := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gateDir)
	mustRun(t, local, "push", gateDir, submitted+":refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, recovered); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, recovered); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, local, "push", gateDir, recovered+":"+custody.RecoveryRef(run.ID))
	service := &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gateDir}
	f := &recoverFixture{
		t: t, ctx: context.Background(), db: database, repo: repo, run: run,
		service: service, local: local, gate: gateDir, remote: remote,
		base: base, submitted: submitted, preserved: recovered,
	}
	if dropContent {
		mustRun(t, local, "reset", "--hard", recovered)
		mustRun(t, local, "update-ref", custody.RecoveryRef(run.ID), recovered)
		mustRun(t, local, "update-ref", custody.RecoveryLocalRef(run.ID), submitted)
		if err := database.SetRunCustodyReturned(run.ID); err != nil {
			t.Fatal(err)
		}
	} else {
		state := service.Recover(f.ctx, false)
		if !state.Recovered || !state.Changed || state.State != StateCustodyReturned {
			t.Fatalf("initial recovered-lineage custody return = %#v", state)
		}
	}
	return f
}

func addRecoveredFollowup(t *testing.T, f *recoverFixture) string {
	t.Helper()
	mustWrite(t, filepath.Join(f.local, "bin", "fm-codex-appserver.py"), "follow-up adapter\n")
	mustWrite(t, filepath.Join(f.local, "tests", "fm-codex-appserver-check.py"), "follow-up checks\n")
	mustRun(t, f.local, "add", "bin/fm-codex-appserver.py", "tests/fm-codex-appserver-check.py")
	mustRun(t, f.local, "commit", "-m", "legitimate post-recovery fixes")
	return mustRun(t, f.local, "rev-parse", "HEAD")
}

func TestRecoverKeepLocalReturnedAncestorMirrorIsNoop(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"never-pushed", "already-pushed", "last-pushed-mirror"} {
		t.Run(name, func(t *testing.T) {
			f := newRecoverFixture(t, types.RunCancelled)
			pushed := name != "never-pushed"
			mirror := f.submitted
			if name == "last-pushed-mirror" {
				mirror = mustRun(t, f.gate, "rev-parse", f.preserved+"^")
				mustRun(t, f.local, "fetch", f.gate, mirror)
			}
			if pushed {
				mustRun(t, f.local, "push", f.remote, mirror+":refs/heads/feature/recover")
				if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
					HeadSHA: mirror, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
				}); err != nil {
					t.Fatal(err)
				}
			}
			first := f.service.Recover(f.ctx, false)
			if !first.Recovered || !first.Changed {
				t.Fatalf("initial recovery = %#v", first)
			}
			mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", mirror)
			if pushed {
				// Prime the private observation ref used by the mandatory live
				// remote check before comparing recovery's durable refs.
				if state := f.service.Refresh(f.ctx); state.Error != "" {
					t.Fatalf("refresh returned push binding = %#v", state)
				}
			}
			localRefs := mustRun(t, f.local, "show-ref")
			gateRefs := mustRun(t, f.gate, "show-ref")
			before, err := f.db.GetRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				state := f.service.Recover(f.ctx, true)
				if !state.Recovered || state.Changed || state.Error != "" {
					t.Fatalf("repeat recovery = %#v", state)
				}
			}
			if got := mustRun(t, f.local, "show-ref"); got != localRefs {
				t.Fatalf("no-op changed local refs: %s", got)
			}
			if got := mustRun(t, f.gate, "show-ref"); got != gateRefs {
				t.Fatalf("no-op changed mirror refs: %s", got)
			}
			if got := mustRun(t, f.local, "status", "--porcelain"); got != "" {
				t.Fatalf("no-op changed worktree: %s", got)
			}
			after, err := f.db.GetRun(f.run.ID)
			if err != nil || after.UpdatedAt != before.UpdatedAt || *after.CustodyReturnedAt != *before.CustodyReturnedAt {
				t.Fatalf("no-op changed custody record: %#v, %v", after, err)
			}
			if pushed {
				mustRun(t, f.remote, "update-ref", "refs/heads/feature/recover", f.base)
				state := f.service.Recover(f.ctx, true)
				if state.Recovered || state.Changed || state.Safety != "blocked_recover_keep_local_not_applicable" {
					t.Fatalf("rewritten remote must prevent no-op = %#v", state)
				}
			}
		})
	}
}

func TestRecoverKeepLocalReconcilesReturnedRebasedMirrorAfterFollowup(t *testing.T) {
	t.Parallel()
	f := newReturnedRebasedMirrorFixture(t, false)
	live := addRecoveredFollowup(t, f)

	if _, err := gate.PlanStaleBranchReconciliation(f.ctx, f.gate, f.local, f.run.Branch, live, ""); err == nil {
		t.Fatal("the old stale-mirror check accepted add/add conflicts against the original submitted mirror")
	}
	if got := mustRun(t, f.local, "show", live+":bin/fm-codex-appserver.py"); got != "follow-up adapter" {
		t.Fatalf("live follow-up content = %q", got)
	}

	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || !state.Changed || state.State != StateCustodyReturned || state.Safety != "mirror_reconciled" {
		t.Fatalf("guarded mirror retry = %#v", state)
	}
	archive := "refs/tags/no-mistakes-abandoned/feature/recover/" + f.submitted
	if got := mustRun(t, f.gate, "rev-parse", archive+"^{commit}"); got != f.submitted {
		t.Fatalf("submitted lineage archive = %s, want %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "for-each-ref", "--format=%(refname)", "refs/heads/feature/recover"); got != "" {
		t.Fatalf("stale private mirror remains: %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", custody.RecoveryLocalRef(f.run.ID)); got != f.submitted {
		t.Fatalf("pre-recovery provenance = %s, want submitted %s", got, f.submitted)
	}
	if got := mustRun(t, f.local, "rev-parse", custody.RecoveryRef(f.run.ID)); got != f.preserved {
		t.Fatalf("recovered provenance = %s, want recovered %s", got, f.preserved)
	}

	// The next pipeline publication uses an ordinary non-force push into the
	// now-empty private lane and retains the exact current recovered tree.
	mustRun(t, f.local, "push", f.gate, live+":refs/heads/feature/recover")
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != live {
		t.Fatalf("ordinary pipeline push reached %s, want %s", got, live)
	}
	if got := mustRun(t, f.gate, "show", "refs/heads/feature/recover:tests/fm-codex-appserver-check.py"); got != "follow-up checks" {
		t.Fatalf("published test follow-up = %q", got)
	}

	again := f.service.Recover(f.ctx, true)
	if !again.Recovered || again.Changed {
		t.Fatalf("repeat mirror reconciliation = %#v", again)
	}
}

func TestRecoverKeepLocalReconcilesPureRebaseAndResumesAfterArchiveOnlyCrash(t *testing.T) {
	t.Parallel()
	f := newReturnedRebasedMirrorFixture(t, false)
	live := f.preserved
	plan, err := gate.PlanStaleBranchReconciliation(f.ctx, f.gate, f.local, f.run.Branch, live, f.submitted)
	if err != nil || !plan.Reconcile {
		t.Fatalf("pure-rebase archive plan = %+v, err=%v", plan, err)
	}
	// This is the recoverable state after archive creation but before the
	// expected-old-value ref deletion.
	mustRun(t, f.gate, "update-ref", "--no-deref", plan.ArchiveTag, f.submitted, strings.Repeat("0", len(f.submitted)))

	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || !state.Changed || state.Safety != "mirror_reconciled" {
		t.Fatalf("retry after archive-only partial handoff = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", plan.ArchiveTag+"^{commit}"); got != f.submitted {
		t.Fatalf("retry replaced archive head with %s", got)
	}
}

func TestRecoverKeepLocalReconcileRefusesDroppedContent(t *testing.T) {
	t.Parallel()
	f := newReturnedRebasedMirrorFixture(t, true)
	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_mirror_lineage" {
		t.Fatalf("dropped-content retry = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("dropped-content refusal moved private mirror to %s", got)
	}
	if got := mustRun(t, f.gate, "for-each-ref", "--format=%(refname)", "refs/tags/no-mistakes-abandoned"); got != "" {
		t.Fatalf("dropped-content refusal archived mirror: %s", got)
	}
}

func TestRecoverKeepLocalReconcileRefusesUnexpectedMirrorAndWrongRecoveryHead(t *testing.T) {
	t.Parallel()
	t.Run("unrelated private commit", func(t *testing.T) {
		f := newReturnedRebasedMirrorFixture(t, false)
		writer := filepath.Join(filepath.Dir(f.local), "mirror-writer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "outside.txt"), "unrelated\\n")
		mustRun(t, writer, "add", "outside.txt")
		mustRun(t, writer, "commit", "-m", "unrelated mirror commit")
		unrelated := mustRun(t, writer, "rev-parse", "HEAD")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_mirror_unexpected_head" {
			t.Fatalf("unrelated mirror retry = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != unrelated {
			t.Fatalf("refusal overwrote unrelated mirror %s", got)
		}
	})
	t.Run("wrong recovery ref", func(t *testing.T) {
		f := newReturnedRebasedMirrorFixture(t, false)
		wrong := mustRun(t, f.local, "rev-parse", f.submitted)
		mustRun(t, f.local, "update-ref", custody.RecoveryRef(f.run.ID), wrong)
		mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), wrong)
		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_mirror_lineage" {
			t.Fatalf("wrong recovered head retry = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("wrong-head refusal moved private mirror to %s", got)
		}
	})
}

func TestRecoverKeepLocalReconcileRefusesPartialMetadataAndUnrelatedBranch(t *testing.T) {
	t.Parallel()
	t.Run("missing pre-recovery anchor", func(t *testing.T) {
		f := newReturnedRebasedMirrorFixture(t, false)
		mustRun(t, f.local, "update-ref", "-d", custody.RecoveryLocalRef(f.run.ID))
		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_mirror_lineage" {
			t.Fatalf("partial metadata retry = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("partial-metadata refusal moved private mirror to %s", got)
		}
	})
	t.Run("branch not descended", func(t *testing.T) {
		f := newReturnedRebasedMirrorFixture(t, false)
		mustRun(t, f.local, "checkout", "-b", "sibling", f.base)
		mustWrite(t, filepath.Join(f.local, "unrelated.txt"), "sibling history\\n")
		mustRun(t, f.local, "add", "unrelated.txt")
		mustRun(t, f.local, "commit", "-m", "unrelated branch")
		sibling := mustRun(t, f.local, "rev-parse", "HEAD")
		mustRun(t, f.local, "checkout", "feature/recover")
		mustRun(t, f.local, "reset", "--hard", sibling)
		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_mirror_lineage" {
			t.Fatalf("unrelated recovered-lineage retry = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("unrelated-branch refusal moved private mirror to %s", got)
		}
	})
}

func TestRecoverKeepLocalReconcileUsesCASAfterPlanning(t *testing.T) {
	t.Parallel()
	f := newReturnedRebasedMirrorFixture(t, false)
	addRecoveredFollowup(t, f)
	writer := filepath.Join(filepath.Dir(f.local), "cas-racer")
	moved := false
	f.service.beforeRecoveredMirrorCAS = func() {
		if moved {
			return
		}
		moved = true
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "racing.txt"), "concurrent private mirror work\n")
		mustRun(t, writer, "add", "racing.txt")
		mustRun(t, writer, "commit", "-m", "concurrent private mirror movement")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	state := f.service.Recover(f.ctx, true)
	if !moved || state.Recovered || state.Changed || state.Safety != "blocked_recover_mirror_race" {
		t.Fatalf("concurrent-ref retry = %#v, moved=%v", state, moved)
	}
	got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")
	want := mustRun(t, writer, "rev-parse", "HEAD")
	if got != want {
		t.Fatalf("concurrent ref was overwritten: got %s, want %s", got, want)
	}
	if got := mustRun(t, f.gate, "for-each-ref", "--format=%(refname)", "refs/tags/no-mistakes-abandoned"); got != "" {
		t.Fatalf("CAS refusal archived the moved ref: %s", got)
	}
}
