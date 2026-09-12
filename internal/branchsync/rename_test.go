package branchsync

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func prepareRenameRerunFixture(t *testing.T) (*syncFixture, *db.Run, string, string) {
	t.Helper()
	f, previous, current := prepareRepositoryRenameFixture(t)
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.old, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(previous), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, f.pushed); err != nil {
		t.Fatal(err)
	}
	older, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := f.db.InsertRun(f.repo.ID, older.Branch, f.pushed, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPushBinding(newer.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(previous), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunErrorStatus(newer.ID, "canonical PR discovery failed", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	f.run, err = f.db.GetRun(newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.GateDir = filepath.Join(t.TempDir(), "gate.git")
	mustRun(t, f.local, "clone", "--bare", f.local, f.service.GateDir)
	f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
		if remote != current {
			return "", fmt.Errorf("unexpected target %s", remote)
		}
		return gitpkg.LsRemote(ctx, dir, f.remote, ref)
	}
	f.service.fetchRemote = func(ctx context.Context, dir, remote, branch, ref string) error {
		if remote != current {
			return fmt.Errorf("unexpected target %s", remote)
		}
		return gitpkg.FetchRemoteBranchToPrivateRef(ctx, dir, f.remote, branch, ref)
	}
	return f, older, previous, current
}

func TestAcceptRepositoryRenameRerunHistorySurvivesCachedLiveAndLaterPush(t *testing.T) {
	t.Parallel()
	f, older, previous, current := prepareRenameRerunFixture(t)
	before, err := f.db.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldService, oldRepo := *f.service, *f.repo
	oldRepo.UpstreamURL = previous
	oldService.Repo = &oldRepo
	if state := oldService.InspectCached(f.ctx); state.State != StateSynchronized || state.Pipeline.RunID != f.run.ID {
		t.Fatalf("original supported rerun history = %#v", state)
	}
	if state := f.service.InspectCached(f.ctx); state.Pipeline.RunID != older.ID || state.State == StateSynchronized {
		t.Fatalf("rename did not reproduce stranded history = %#v", state)
	}
	refs := make(map[string]string)
	for _, dir := range []string{f.local, f.remote, f.service.GateDir} {
		refs[dir] = mustRun(t, dir, "show-ref")
	}
	for _, wantChanged := range []bool{true, false} {
		state := f.service.AcceptRepositoryRename(f.ctx, previous)
		if state.State != StateSynchronized || state.Pipeline.RunID != f.run.ID || state.Changed != wantChanged || state.NextAction == nil || state.NextAction.Code != "rerun_pipeline" {
			t.Fatalf("rename continuation = %#v", state)
		}
	}
	after, err := f.db.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentFingerprint := TargetFingerprint(current)
	before[0].PushTargetFingerprint = &currentFingerprint
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed historical runs beyond the selected fingerprint: before=%+v after=%+v", before, after)
	}
	for dir, before := range refs {
		if after := mustRun(t, dir, "show-ref"); after != before {
			t.Fatalf("migration changed refs in %s: before=%s after=%s", dir, before, after)
		}
	}
	reopened, err := db.Open(filepath.Join(filepath.Dir(f.local), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service := *f.service
	service.DB = reopened
	service.VerifyRepositoryRename = nil
	for _, inspect := range []func(context.Context) State{service.InspectCached, service.Refresh} {
		state := inspect(f.ctx)
		if state.State != StateSynchronized || state.Pipeline.RunID != f.run.ID || state.Changed {
			t.Fatalf("reopened cached/live selection = %#v", state)
		}
	}
	mustWrite(t, filepath.Join(f.local, "later.txt"), "later validation fix\n")
	mustRun(t, f.local, "add", "later.txt")
	mustRun(t, f.local, "commit", "-m", "later validation fix")
	laterHead := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/sync")
	mustRun(t, f.service.GateDir, "fetch", f.local, "refs/heads/feature/sync:refs/heads/feature/sync")
	later, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(later.ID, laterHead); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPushBinding(later.ID, db.PushBinding{HeadSHA: laterHead, TargetKind: "upstream", TargetFingerprint: currentFingerprint, Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunErrorStatus(later.ID, "later validation still failed", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	for _, inspect := range []func(context.Context) State{service.InspectCached, service.Refresh} {
		if state := inspect(f.ctx); state.State != StateSynchronized || state.Pipeline.RunID != later.ID || state.Pipeline.Status != string(types.RunFailed) {
			t.Fatalf("later descendant selection = %#v", state)
		}
	}
	if got := mustRun(t, f.local, "show", "HEAD:fix.txt"); got != "pipeline fix" {
		t.Fatalf("lost original fix: %q", got)
	}
}

func TestAcceptRepositoryRenameRerunRefusalsPreserveHistory(t *testing.T) {
	t.Parallel()
	for _, condition := range []string{"identity", "unrelated", "ref", "kind", "uncontained", "missing_gate", "symbolic_gate", "remote_changed", "active", "active_during_read", "gate_changed_during_read", "binding_changed_during_read", "source_changed_during_read", "target_changed_during_read", "interrupted"} {
		t.Run(condition, func(t *testing.T) {
			t.Parallel()
			f, older, previous, _ := prepareRenameRerunFixture(t)
			mutate := func() {
				switch condition {
				case "identity":
					f.service.VerifyRepositoryRename = func(context.Context, string, string) error { return context.Canceled }
				case "interrupted":
					f.service.lsRemote = func(context.Context, string, string, string) (string, error) { return "", context.Canceled }
				case "unrelated", "ref", "kind":
					binding := db.PushBinding{HeadSHA: f.old, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(previous), Ref: "refs/heads/feature/sync"}
					switch condition {
					case "unrelated":
						binding.TargetFingerprint = TargetFingerprint("https://github.com/other/unrelated")
					case "ref":
						binding.Ref = "refs/heads/another"
					case "kind":
						binding.TargetKind = "fork"
					}
					if err := f.db.UpdateRunPushBinding(older.ID, binding); err != nil {
						t.Fatal(err)
					}
				case "uncontained":
					head := mustRun(t, f.service.GateDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", f.base+"^{tree}", "-p", f.base, "-m", "uncontained fix")
					if err := f.db.UpdateRunHeadSHA(older.ID, head); err != nil {
						t.Fatal(err)
					}
				case "missing_gate":
					f.service.GateDir = ""
				case "symbolic_gate":
					mustRun(t, f.service.GateDir, "update-ref", "refs/heads/alias", f.pushed)
					mustRun(t, f.service.GateDir, "symbolic-ref", "refs/heads/feature/sync", "refs/heads/alias")
				case "remote_changed":
					mustRun(t, f.remote, "update-ref", "refs/heads/feature/sync", f.old)
				case "active", "active_during_read":
					if _, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base); err != nil {
						t.Fatal(err)
					}
				case "gate_changed_during_read":
					mustRun(t, f.service.GateDir, "update-ref", "refs/heads/feature/sync", f.old)
				case "binding_changed_during_read":
					if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(previous), Ref: "refs/heads/feature/sync"}); err != nil {
						t.Fatal(err)
					}
				case "source_changed_during_read":
					mustWrite(t, filepath.Join(f.local, "dirty.txt"), "operator work\n")
				case "target_changed_during_read":
					if _, err := f.db.UpdateRepoMetadata(f.repo.ID, "https://github.com/org/changed-again", "main"); err != nil {
						t.Fatal(err)
					}
				}
			}
			var before []*db.Run
			capture := func() {
				var err error
				before, err = f.db.GetRunsByRepo(f.repo.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch condition {
			case "active_during_read", "gate_changed_during_read", "binding_changed_during_read", "source_changed_during_read", "target_changed_during_read":
				readRemote := f.service.lsRemote
				f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
					live, err := readRemote(ctx, dir, remote, ref)
					mutate()
					capture()
					return live, err
				}
			default:
				mutate()
			}
			capture()
			state := f.service.AcceptRepositoryRename(f.ctx, previous)
			if state.State == StateSynchronized || state.Changed || state.NextAction != nil {
				t.Fatalf("unsafe history accepted: %#v", state)
			}
			after, err := f.db.GetRunsByRepo(f.repo.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal modified history: before=%+v after=%+v, %v", before, after, err)
			}
			witnesses, err := f.db.GetPushTargetRenameWitnesses(f.repo.ID, f.run.Branch, TargetFingerprint(previous))
			if err != nil || len(witnesses) != 0 {
				t.Fatalf("refusal left rename authority: %+v, %v", witnesses, err)
			}
			if head := mustRun(t, f.local, "rev-parse", "HEAD"); head != f.pushed {
				t.Fatalf("refusal moved source: %s", head)
			}
		})
	}
}

func TestRepositoryRenameProvenanceCannotOverrideChangedOrUnrelatedHistory(t *testing.T) {
	t.Parallel()
	for _, condition := range []string{"witness_generation", "uncontained_head", "gate_head", "active_owner", "later_unpublished_old_target"} {
		t.Run(condition, func(t *testing.T) {
			t.Parallel()
			f, older, previous, current := prepareRenameRerunFixture(t)
			if state := f.service.AcceptRepositoryRename(f.ctx, previous); state.State != StateSynchronized {
				t.Fatalf("prepare migration: %#v", state)
			}
			switch condition {
			case "witness_generation":
				if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(current), Ref: "refs/heads/feature/sync"}); err != nil {
					t.Fatal(err)
				}
			case "uncontained_head":
				head := mustRun(t, f.service.GateDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", f.base+"^{tree}", "-p", f.base, "-m", "independent fix")
				if err := f.db.UpdateRunHeadSHA(older.ID, head); err != nil {
					t.Fatal(err)
				}
			case "gate_head":
				mustRun(t, f.service.GateDir, "update-ref", "refs/heads/feature/sync", f.old)
			case "active_owner":
				if _, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base); err != nil {
					t.Fatal(err)
				}
			case "later_unpublished_old_target":
				for _, fingerprint := range []string{TargetFingerprint(previous), TargetFingerprint(current)} {
					run, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base)
					if err != nil {
						t.Fatal(err)
					}
					pushed := f.pushed
					if fingerprint == TargetFingerprint(previous) {
						pushed = f.old
					}
					if err := f.db.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: fingerprint, Ref: "refs/heads/feature/sync"}); err != nil {
						t.Fatal(err)
					}
					if err := f.db.UpdateRunErrorStatus(run.ID, "later failure", types.RunFailed); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := f.db.GetRunsByRepo(f.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.service.VerifyRepositoryRename = func(context.Context, string, string) error {
				t.Fatal("cached provenance unexpectedly required a provider read")
				return context.Canceled
			}
			for _, inspect := range []func(context.Context) State{f.service.InspectCached, f.service.Refresh} {
				if state := inspect(f.ctx); state.State == StateSynchronized || state.Changed {
					t.Fatalf("stored proof bypassed changed history: %#v", state)
				}
			}
			after, err := f.db.GetRunsByRepo(f.repo.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("inspection rewrote changed history: %+v, %v", after, err)
			}
		})
	}
}

func TestAcceptRepositoryRenameSequentialRenames(t *testing.T) {
	t.Parallel()
	for _, separateRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("separate_run=%t", separateRun), func(t *testing.T) {
			t.Parallel()
			f, _, previous, current := prepareRenameRerunFixture(t)
			if state := f.service.AcceptRepositoryRename(f.ctx, previous); state.State != StateSynchronized {
				t.Fatalf("first rename: %#v", state)
			}
			selectedID := f.run.ID
			if separateRun {
				mustWrite(t, filepath.Join(f.local, "later.txt"), "later validation fix\n")
				mustRun(t, f.local, "add", "later.txt")
				mustRun(t, f.local, "commit", "-m", "later validation fix")
				laterHead := mustRun(t, f.local, "rev-parse", "HEAD")
				mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/sync")
				mustRun(t, f.service.GateDir, "fetch", f.local, "refs/heads/feature/sync:refs/heads/feature/sync")
				later, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunHeadSHA(later.ID, laterHead); err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunPushBinding(later.ID, db.PushBinding{HeadSHA: laterHead, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(current), Ref: "refs/heads/feature/sync"}); err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunErrorStatus(later.ID, "later validation still failed", types.RunFailed); err != nil {
					t.Fatal(err)
				}
				selectedID = later.ID
			}
			next := "https://github.com/org/next"
			repo, err := f.db.UpdateRepoMetadata(f.repo.ID, next, "main")
			if err != nil {
				t.Fatal(err)
			}
			f.service.Repo = repo
			f.service.VerifyRepositoryRename = func(_ context.Context, old, target string) error {
				if old != current || target != next {
					return fmt.Errorf("unexpected identity read: %s -> %s", old, target)
				}
				return nil
			}
			f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
				if remote != next {
					return "", fmt.Errorf("unexpected target %s", remote)
				}
				return gitpkg.LsRemote(ctx, dir, f.remote, ref)
			}
			f.service.fetchRemote = func(ctx context.Context, dir, remote, branch, ref string) error {
				if remote != next {
					return fmt.Errorf("unexpected target %s", remote)
				}
				return gitpkg.FetchRemoteBranchToPrivateRef(ctx, dir, f.remote, branch, ref)
			}
			before, err := f.db.GetRunsByRepo(f.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			refs := make(map[string]string)
			for _, dir := range []string{f.local, f.remote, f.service.GateDir} {
				refs[dir] = mustRun(t, dir, "show-ref")
			}
			for _, wantChanged := range []bool{true, false} {
				state := f.service.AcceptRepositoryRename(f.ctx, current)
				if state.State != StateSynchronized || state.Pipeline.RunID != selectedID || state.Changed != wantChanged || state.NextAction == nil || state.NextAction.Code != "rerun_pipeline" {
					t.Fatalf("second rename: %#v", state)
				}
			}
			for dir, before := range refs {
				if after := mustRun(t, dir, "show-ref"); after != before {
					t.Fatalf("second rename moved refs in %s: before=%s after=%s", dir, before, after)
				}
			}
			after, err := f.db.GetRunsByRepo(f.repo.ID)
			nextFingerprint := TargetFingerprint(next)
			before[0].PushTargetFingerprint = &nextFingerprint
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("second rename changed historical runs: before=%+v after=%+v, %v", before, after, err)
			}
			reopened, err := db.Open(filepath.Join(filepath.Dir(f.local), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			f.service.DB = reopened
			f.service.VerifyRepositoryRename = nil
			for _, inspect := range []func(context.Context) State{f.service.InspectCached, f.service.Refresh} {
				if state := inspect(f.ctx); state.State != StateSynchronized || state.Pipeline.RunID != selectedID || state.Pipeline.Status != string(types.RunFailed) {
					t.Fatalf("reopened sequential rename selection: %#v", state)
				}
			}
			// In the separate-run case this invalidates the intermediate A-to-B
			// witness while leaving the final B-to-C binding intact.
			fingerprint := nextFingerprint
			if separateRun {
				fingerprint = TargetFingerprint(current)
			}
			if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: fingerprint, Ref: "refs/heads/feature/sync"}); err != nil {
				t.Fatal(err)
			}
			for _, inspect := range []func(context.Context) State{f.service.InspectCached, f.service.Refresh} {
				if state := inspect(f.ctx); state.State == StateSynchronized || state.Changed {
					t.Fatalf("changed intermediate witness retained continuity: %#v", state)
				}
			}
		})
	}
}
