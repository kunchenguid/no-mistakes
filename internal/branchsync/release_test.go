package branchsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newPublishedReleaseFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	f.service.GateDir = filepath.Join(filepath.Dir(f.local), "gate.git")
	mustRun(t, filepath.Dir(f.local), "clone", "--bare", f.remote, f.service.GateDir)
	mustRun(t, f.local, "fetch", f.remote, "refs/heads/feature/sync")
	mustRun(t, f.local, "merge", "--ff-only", "FETCH_HEAD")
	// The gate contains a stranded unpublished descendant while the operator
	// and open PR retain the last published head.
	mustRun(t, f.local, "commit", "--allow-empty", "-m", "unpublished pipeline work")
	stranded := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.service.GateDir, "fetch", f.local, "HEAD")
	mustRun(t, f.service.GateDir, "update-ref", "refs/heads/feature/sync", stranded)
	mustRun(t, f.local, "reset", "--hard", f.pushed)
	if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "daemon shutting down", types.RunFailed, stranded); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	return f
}

func TestReleasePublishedArchivesAndRestoresMissingOrStaleGate(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale", true: "missing"}[missing], func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			if missing {
				mustRun(t, f.service.GateDir, "update-ref", "-d", "refs/heads/feature/sync")
			}
			before := f.run.HeadSHA
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, true, releasePRProof)
			if !state.Recovered || state.State != StateCustodyReturned {
				t.Fatalf("release: %+v", state)
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, before)); got != before {
				t.Fatalf("archive = %s", got)
			}
			if f.run.SubmittedHeadSHA != nil {
				if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, *f.run.SubmittedHeadSHA)); got != *f.run.SubmittedHeadSHA {
					t.Fatal("submitted head was not preserved")
				}
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
				t.Fatalf("gate = %s", got)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
				t.Fatalf("caller changed: %s", got)
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
				t.Fatalf("remote changed: %s", got)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt == nil || run.HeadSHA != before || ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) {
				t.Fatalf("lost historical provenance: %+v", run)
			}
			again := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, true, releasePRProof)
			if !again.Recovered || again.Changed {
				t.Fatalf("retry not idempotent: %+v", again)
			}
		})
	}
}

func TestReleasePublishedClassificationKeepsHistoricalPushProvenance(t *testing.T) {
	t.Parallel()
	for _, replacement := range []string{"equal", "ancestor", "descendant", "diverged"} {
		for _, restartOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reconcile=%v", replacement, restartOnly), func(t *testing.T) {
				f := newPublishedReleaseFixture(t)
				switch replacement {
				case "ancestor":
					mustRun(t, f.local, "reset", "--hard", f.base)
				case "descendant":
					mustRun(t, f.local, "commit", "--allow-empty", "-m", "published descendant")
				case "diverged":
					mustRun(t, f.local, "reset", "--hard", f.base)
					mustRun(t, f.local, "commit", "--allow-empty", "-m", "published replacement")
				}
				published := mustRun(t, f.local, "rev-parse", "HEAD")
				mustRun(t, f.local, "push", f.remote, published+":refs/heads/feature/sync", "--force-with-lease=refs/heads/feature/sync:"+f.pushed)
				state := f.service.ReleasePublished(f.ctx, f.run.ID, published, restartOnly, releasePRProof)
				if !state.Recovered {
					t.Fatalf("release = %+v", state)
				}
				for _, head := range []string{published, f.run.HeadSHA, f.pushed} {
					if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, head)); got != head {
						t.Fatalf("lost preserved head %s: %s", head, got)
					}
				}
				if state.State != StateCustodyReturned || state.Safety != "gate_ready" || state.Relation != RelationEqual || state.Error != "" || state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
					t.Fatalf("completed release = %+v", state)
				}
				if state.Pipeline.PushedHead != f.pushed || state.Pipeline.CurrentHead != f.run.HeadSHA || state.Remote.ObservedHead != published || state.Recovery == nil || state.Recovery.RequiredHead != published || state.Recovery.ArchiveRef != releaseArchiveRef(f.run.ID, published) {
					t.Fatalf("release lost publication or historical evidence: %+v", state)
				}
				cached := f.service.InspectCached(f.ctx)
				if cached.Recovery != nil && cached.Recovery.Source == "published_release" || cached.Remote.Freshness == "published_release" {
					t.Fatalf("cached inspection inferred release completion: %+v", cached)
				}
				for _, dir := range []string{f.local, f.remote, f.service.GateDir} {
					if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/sync"); got != published {
						t.Fatalf("follow-up changed %s head: %s", dir, got)
					}
				}
				run, err := f.db.GetRun(f.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.CustodyReturnedAt == nil || run.HeadSHA != f.run.HeadSHA || ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) || ptr(run.Error) != ptr(f.run.Error) {
					t.Fatalf("lost historical provenance: %+v", run)
				}
			})
		}
	}
}

func TestReleasePublishedRefusalDoesNotCreditOrdinaryRecoveryAsPublished(t *testing.T) {
	t.Parallel()
	for _, restartOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconcile=%v", restartOnly), func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			unpublished := f.run.HeadSHA
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, restartOnly, func(context.Context) error {
				return errors.New("PR changed")
			})
			if state.Recovered || !strings.Contains(state.Error, "PR changed") {
				t.Fatalf("release refusal = %+v", state)
			}
			for _, head := range []string{f.pushed, unpublished} {
				if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, head)); got != head {
					t.Fatalf("refused release did not preserve %s: %s", head, got)
				}
			}
			run, err := f.db.GetRun(f.run.ID)
			if err != nil || run == nil || run.CustodyReturnedAt != nil {
				t.Fatalf("refusal returned custody: run=%+v err=%v", run, err)
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != unpublished {
				t.Fatalf("refusal moved gate: %s", got)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
				t.Fatalf("refusal moved caller: %s", got)
			}
			recovered := f.service.Recover(f.ctx, false)
			if !recovered.Recovered || !recovered.Changed {
				t.Fatalf("ordinary recovery = %+v", recovered)
			}
			for _, inspect := range []func(context.Context) State{f.service.InspectCached, f.service.Refresh} {
				got := inspect(f.ctx)
				if got.State != StateLocalAhead || got.Relation != RelationAhead || got.Local.Head != unpublished || got.Safety == "gate_ready" || got.Recovery != nil && got.Recovery.Source == "published_release" {
					t.Fatalf("unpublished recovery credited as publication: %+v", got)
				}
				if got.Pipeline.PushedHead != f.pushed || got.Remote.ObservedHead != f.pushed {
					t.Fatalf("inspection changed publication evidence: %+v", got)
				}
			}
			again := f.service.Recover(f.ctx, false)
			if !again.Recovered || again.Changed || again.Error != "" {
				t.Fatalf("ordinary recovery is not idempotent: %+v", again)
			}
			for _, dir := range []string{f.local, f.service.GateDir} {
				if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/sync"); got != unpublished {
					t.Fatalf("recovery lost unpublished work in %s: %s", dir, got)
				}
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
				t.Fatalf("recovery changed remote: %s", got)
			}
			run, err = f.db.GetRun(f.run.ID)
			if err != nil || run == nil || run.CustodyReturnedAt == nil || run.HeadSHA != unpublished || ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) || ptr(run.Error) != ptr(f.run.Error) {
				t.Fatalf("recovery lost historical provenance: run=%+v err=%v", run, err)
			}
		})
	}
}

func TestReleasePublishedRefusesUnsafePaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*testing.T, *syncFixture)
		reason string
	}{
		{"unpublished-local", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.local, "commit", "--allow-empty", "-m", "unpublished local")
		}, "exactly match"},
		{"missing-publication", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.remote, "update-ref", "-d", "refs/heads/feature/sync")
		}, "exactly match"},
		{"dirty-caller", func(t *testing.T, f *syncFixture) { mustWrite(t, filepath.Join(f.local, "dirty"), "uncommitted") }, "clean"},
		{"active-run", func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
		}, "live run"},
		{"missing-unpublished-head", func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunHeadSHA(f.run.ID, strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
		}, "unavailable"},
		{"conflicting-archive", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.service.GateDir, "update-ref", releaseArchiveRef(f.run.ID, f.run.HeadSHA), f.pushed)
		}, "archive conflicts"},
		{"symbolic-archive", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.service.GateDir, "symbolic-ref", releaseArchiveRef(f.run.ID, f.run.HeadSHA), "refs/heads/feature/sync")
		}, "archive conflicts"},
		{"symbolic-gate", func(t *testing.T, f *syncFixture) {
			mustRun(t, f.service.GateDir, "update-ref", "refs/heads/alias", f.run.HeadSHA)
			mustRun(t, f.service.GateDir, "symbolic-ref", "refs/heads/feature/sync", "refs/heads/alias")
		}, "symbolic"},
		{"nonrestart-failure", func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "lint failed", types.RunFailed, f.run.HeadSHA); err != nil {
				t.Fatal(err)
			}
		}, "historical daemon"},
		{"newer-generation", func(t *testing.T, f *syncFixture) {
			r, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatus(r.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
		}, "current branch generation"},
		{"dirty-managed", func(t *testing.T, f *syncFixture) {
			if err := f.db.SetRunWorktreeDir(f.run.ID, f.local); err != nil {
				t.Fatal(err)
			}
			f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
				mustWrite(t, filepath.Join(f.local, "dirty"), "uncommitted")
				return git.LsRemote(ctx, dir, remote, ref)
			}
		}, "uncommitted work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			tc.change(t, f)
			gateBefore, _, _ := git.ExactRefTarget(f.ctx, f.service.GateDir, "refs/heads/feature/sync")
			head := mustRun(t, f.local, "rev-parse", "HEAD")
			state := f.service.ReleasePublished(f.ctx, f.run.ID, head, true, releasePRProof)
			if state.Recovered || !strings.Contains(state.Error, tc.reason) {
				t.Fatalf("refusal: %+v; want %q", state, tc.reason)
			}
			gateAfter, _, _ := git.ExactRefTarget(f.ctx, f.service.GateDir, "refs/heads/feature/sync")
			if gateBefore != gateAfter {
				t.Fatal("refused operation replaced gate ref")
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt != nil {
				t.Fatal("refused operation released custody")
			}
		})
	}
}

func TestReleasePublishedDoesNotOverwriteGateRace(t *testing.T) {
	t.Parallel()
	f := newPublishedReleaseFixture(t)
	n := 0
	f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
		n++
		if n == 2 {
			mustRun(t, f.service.GateDir, "update-ref", "refs/heads/feature/sync", f.base)
		}
		return git.LsRemote(ctx, dir, remote, ref)
	}
	state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof)
	if state.Recovered || !strings.Contains(state.Error, "gate lane changed") {
		t.Fatalf("race: %+v", state)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != f.base {
		t.Fatalf("other owner clobbered: %s", got)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, f.run.HeadSHA)); got != f.run.HeadSHA {
		t.Fatalf("old head not archived: %s", got)
	}
}

// Keep the recovery namespace independent: release never replaces a prior
// recovery anchor just to make a diverged published branch usable.
func TestReleasePublishedPreservesPriorRecoveryEvidence(t *testing.T) {
	t.Parallel()
	f := newPublishedReleaseFixture(t)
	mustRun(t, f.service.GateDir, "update-ref", custody.RecoveryRef(f.run.ID), f.base)
	if state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof); !state.Recovered {
		t.Fatalf("%+v", state)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", custody.RecoveryRef(f.run.ID)); got != f.base {
		t.Fatal("recovery evidence overwritten")
	}
}

func TestReleasePublishedRechecksArchivesAndManagedWork(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"deleted-archive", "symbolic-archive", "changed-managed", "changed-generation", "changed-error", "changed-default", "changed-registration"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			if scenario == "changed-managed" {
				if err := f.db.SetRunWorktreeDir(f.run.ID, f.local); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
				n++
				if n == 2 {
					switch scenario {
					case "symbolic-archive":
						mustRun(t, f.service.GateDir, "symbolic-ref", releaseArchiveRef(f.run.ID, f.run.HeadSHA), "refs/heads/feature/sync")
					case "deleted-archive":
						mustRun(t, f.service.GateDir, "update-ref", "-d", releaseArchiveRef(f.run.ID, f.run.HeadSHA))
					case "changed-managed":
						mustWrite(t, filepath.Join(f.local, "dirty"), "uncommitted work")
					case "changed-default":
						if _, err := f.db.UpdateRepoMetadata(f.repo.ID, f.repo.UpstreamURL, f.run.Branch); err != nil {
							t.Fatal(err)
						}
					case "changed-registration":
						if _, err := f.db.UpdateRepoWorkingPath(f.repo.ID, filepath.Join(f.local, "different")); err != nil {
							t.Fatal(err)
						}
					case "changed-error":
						if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "lint failed", types.RunFailed, f.run.HeadSHA); err != nil {
							t.Fatal(err)
						}
					case "changed-generation":
						if err := f.db.UpdateRunHeadSHA(f.run.ID, f.base); err != nil {
							t.Fatal(err)
						}
					}
				}
				return git.LsRemote(ctx, dir, remote, ref)
			}
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof)
			if state.Recovered {
				t.Fatalf("unsafe recovery: %+v", state)
			}
			if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != f.run.HeadSHA {
				t.Fatal("unarchived or stale generation gate head dropped")
			}
			r, _ := f.db.GetRun(f.run.ID)
			if r.CustodyReturnedAt != nil {
				t.Fatal("unsafe generation stamped")
			}
		})
	}
}

func releasePRProof(context.Context) error { return nil }

func TestReleasePublishedUsesRecordedDestinationAndKeepsCustodyLane(t *testing.T) {
	for _, scenario := range []string{"source-missing", "source-different", "destination-missing", "destination-different", "destination-changed", "destination-owner", "rebound-owner", "target-changed", "default-destination"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			liveRun, _ := f.db.GetRun(f.run.ID)
			destination := "existing"
			if scenario == "default-destination" {
				destination = "main"
			}
			if err := f.db.RebindPublication(f.repo, liveRun, destination, "https://github.com/test/repo/pull/1", TargetFingerprint(f.remote)); err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunErrorStatusWithVerifiedHead(f.run.ID, "daemon shutting down", types.RunFailed, f.run.HeadSHA); err != nil {
				t.Fatal(err)
			}
			publicationRef := "refs/heads/" + destination
			mustRun(t, f.local, "push", f.remote, f.pushed+":"+publicationRef)
			reason := ""
			switch scenario {
			case "source-missing":
				mustRun(t, f.remote, "update-ref", "-d", "refs/heads/feature/sync")
			case "source-different":
				mustRun(t, f.remote, "update-ref", "refs/heads/feature/sync", f.base)
			case "destination-missing":
				mustRun(t, f.remote, "update-ref", "-d", publicationRef)
				reason = "exactly match"
			case "destination-different":
				mustRun(t, f.remote, "update-ref", publicationRef, f.base)
				reason = "exactly match"
			case "destination-changed":
				calls := 0
				f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
					calls++
					if calls == 2 {
						mustRun(t, f.remote, "update-ref", publicationRef, f.base)
					}
					return git.LsRemote(ctx, dir, remote, ref)
				}
				reason = "branch changed"
			case "destination-owner", "rebound-owner":
				branch := destination
				if scenario == "rebound-owner" {
					branch = "other-source"
				}
				owner, err := f.db.InsertRun(f.repo.ID, branch, f.pushed, f.base)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "rebound-owner" {
					if err := f.db.RebindPublication(f.repo, owner, destination, "https://github.com/test/repo/pull/1", TargetFingerprint(f.remote)); err != nil {
						t.Fatal(err)
					}
				}
				reason = "generation changed"
			case "target-changed":
				if _, err := f.db.UpdateRepoMetadata(f.repo.ID, f.remote+"-different", "main"); err != nil {
					t.Fatal(err)
				}
				reason = "publication target changed"
			case "default-destination":
				reason = "default branch"
			}
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, true, releasePRProof)
			run, _ := f.db.GetRun(f.run.ID)
			gateHead := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync")
			if reason != "" {
				if state.Recovered || !strings.Contains(state.Error, reason) || run.CustodyReturnedAt != nil || gateHead != f.run.HeadSHA {
					t.Fatalf("unsafe release: state=%+v run=%+v gate=%s; want %q", state, run, gateHead, reason)
				}
				return
			}
			if !state.Recovered || state.Target.Ref != publicationRef || run.CustodyReturnedAt == nil || gateHead != f.pushed {
				t.Fatalf("rebound release: state=%+v run=%+v gate=%s", state, run, gateHead)
			}
			for _, head := range []string{f.run.HeadSHA, f.pushed} {
				if got := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, head)); got != head {
					t.Fatal("owned head not archived")
				}
			}
			if _, exists, err := git.ExactRefTarget(f.ctx, f.service.GateDir, publicationRef); err != nil || exists {
				t.Fatalf("release created a destination gate lane: exists=%v err=%v", exists, err)
			}
			if run.PublishBranch() != destination || ptr(run.PushRef) != ptr(f.run.PushRef) || ptr(run.LastPushedSHA) != f.pushed || run.HeadSHA != f.run.HeadSHA {
				t.Fatalf("lost historical provenance: %+v", run)
			}
		})
	}
}

func TestReleasePublishedRollsBackAfterDatabaseRefusal(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"existing", "missing", "intervening-publisher"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishedReleaseFixture(t)
			ref := "refs/heads/feature/sync"
			if scenario != "existing" {
				mustRun(t, f.service.GateDir, "update-ref", "-d", ref)
			}
			connection, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.local), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := connection.Exec(`CREATE TRIGGER refuse_custody_stamp BEFORE UPDATE OF custody_returned_at ON runs BEGIN SELECT RAISE(ABORT, 'registration fence refused'); END`); err != nil {
				t.Fatal(err)
			}
			if scenario == "intervening-publisher" {
				hook := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = committed ] || exit 0\nwhile read old new ref; do\nif [ \"$ref\" = '%s' ] && [ \"$new\" = '%s' ]; then\ngit --git-dir='%s' update-ref --no-deref '%s' '%s' '%s' || exit 1\nfi\ndone\n", ref, f.pushed, f.service.GateDir, ref, f.base, f.pushed)
				if err := os.WriteFile(filepath.Join(f.service.GateDir, "hooks", "reference-transaction"), []byte(hook), 0755); err != nil {
					t.Fatal(err)
				}
			}
			state := f.service.ReleasePublished(f.ctx, f.run.ID, f.pushed, false, releasePRProof)
			if state.Recovered || !strings.Contains(state.Error, "generation changed") {
				t.Fatalf("unexpected release: %+v", state)
			}
			got, exists, err := git.ExactRefTarget(f.ctx, f.service.GateDir, ref)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "existing":
				if !exists || got != f.run.HeadSHA {
					t.Fatalf("old gate head not restored: %s", got)
				}
			case "missing":
				if exists {
					t.Fatalf("created gate ref retained after refusal: %s", got)
				}
			case "intervening-publisher":
				if got != f.base || !strings.Contains(state.Error, "rollback failed") {
					t.Fatalf("other publisher or rollback failure lost: %s %+v", got, state)
				}
			}
			if archived := mustRun(t, f.service.GateDir, "rev-parse", releaseArchiveRef(f.run.ID, f.pushed)); archived != f.pushed {
				t.Fatal("published head not archived")
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt != nil {
				t.Fatal("refused stamp released custody")
			}
		})
	}
}
