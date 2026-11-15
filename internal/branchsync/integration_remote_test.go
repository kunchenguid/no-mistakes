package branchsync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/integration"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIntegrationRemoteSyncUsesRuntimeCredentials(t *testing.T) {
	for _, apply := range []bool{false, true} {
		name := "refresh"
		if apply {
			name = "apply"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSyncFixture(t)
			const credentialURL = "https://fixture-user:fixture-password@integration.invalid/owner/project.git"
			mustRun(t, f.local, "remote", "add", "custom", credentialURL)
			pin := &integration.Destination{
				Remote: "custom", URL: safeurl.Redact(credentialURL), TransportURL: safeurl.Redact(credentialURL),
				DefaultBranch: "main", Key: "credentialledabcdefghijkl",
			}
			var err error
			f.run, err = f.db.InsertRunWithDestination(f.repo.ID, "feature/sync", f.old, f.base, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, pin)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunHeadSHA(f.run.ID, f.pushed); err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
				HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(credentialURL), Ref: "refs/heads/feature/sync",
			}); err != nil {
				t.Fatal(err)
			}
			completeSyncRun(t, f)
			// The transport seam requires the synthetic URL's credentials,
			// then drives real Git against the local published repository.
			calls := 0
			f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
				calls++
				if remote != credentialURL {
					return "", fmt.Errorf("synthetic remote credentials were lost")
				}
				return git.LsRemote(ctx, dir, f.remote, ref)
			}
			f.service.fetchRemote = func(ctx context.Context, dir, remote, branch, localRef string) error {
				calls++
				if remote != credentialURL {
					return fmt.Errorf("synthetic remote credentials were lost")
				}
				return git.FetchRemoteBranchToPrivateRef(ctx, dir, f.remote, branch, localRef)
			}
			var state State
			wantCalls := 2
			if apply {
				state = f.service.Apply(f.ctx)
				wantCalls = 3
				if state.State != StateSynchronized || !state.Changed || state.Local.Head != f.pushed {
					t.Fatalf("credentialled apply = %#v", state)
				}
			} else {
				state = f.service.Refresh(f.ctx)
				if state.State != StateBehind || state.Safety != SafetySafeFastForward || state.Changed {
					t.Fatalf("credentialled refresh = %#v", state)
				}
			}
			if calls != wantCalls {
				t.Fatalf("Git calls = %d, want %d", calls, wantCalls)
			}
			stored, err := f.db.GetRun(f.run.ID)
			if err != nil || stored.Integration.TransportURL != pin.TransportURL {
				t.Fatalf("runtime credential recovery changed the stored identity: %+v, %v", stored, err)
			}
			if got := mustRun(t, f.local, "remote", "get-url", "custom"); got != credentialURL {
				t.Fatalf("sync changed the configured remote: %q", got)
			}
		})
	}
}

func TestIntegrationRemoteUnpinnedSyncUsesRecordedPushURL(t *testing.T) {
	for _, fork := range []bool{false, true} {
		for _, operation := range []string{"refresh", "apply", "recover"} {
			t.Run(fmt.Sprintf("%s/fork=%t", operation, fork), func(t *testing.T) {
				t.Parallel()
				f := newSyncFixture(t)
				wantHead := f.pushed
				if operation == "recover" {
					gate := filepath.Join(t.TempDir(), "gate.git")
					mustRun(t, f.local, "clone", "--bare", f.remote, gate)
					f.service.GateDir = gate
					wantHead = forceRewriteRemote(t, f, "unpinned-rewrite")
				}
				const credentialURL = "https://fixture-user:fixture-password@integration.invalid/owner/project.git"
				const recordedURL = "https://integration.invalid/owner/project.git"
				mustRun(t, f.local, "remote", "add", "transport", credentialURL)
				upstream, forkURL, kind := recordedURL, "", "upstream"
				if fork {
					upstream, forkURL, kind = f.remote, recordedURL, "fork"
				}
				var err error
				f.repo, err = f.db.ReplaceRepoURLs(f.repo.ID, upstream, forkURL)
				if err != nil {
					t.Fatal(err)
				}
				f.service.Repo = f.repo
				if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
					HeadSHA: f.pushed, TargetKind: kind, TargetFingerprint: TargetFingerprint(recordedURL), Ref: "refs/heads/feature/sync",
				}); err != nil {
					t.Fatal(err)
				}
				calls := 0
				f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
					calls++
					if remote != recordedURL {
						t.Errorf("unpinned lookup transport = %q, want recorded URL", remote)
					}
					return git.LsRemote(ctx, dir, f.remote, ref)
				}
				f.service.fetchRemote = func(ctx context.Context, dir, remote, branch, localRef string) error {
					calls++
					if remote != recordedURL {
						t.Errorf("unpinned fetch transport = %q, want recorded URL", remote)
					}
					return git.FetchRemoteBranchToPrivateRef(ctx, dir, f.remote, branch, localRef)
				}
				var state State
				wantCalls := 3
				switch operation {
				case "refresh":
					state = f.service.Refresh(f.ctx)
					wantCalls = 2
					if state.State != StateBehind || state.Safety != SafetySafeFastForward || state.Changed {
						t.Fatalf("unpinned refresh = %#v", state)
					}
				case "apply":
					state = f.service.Apply(f.ctx)
					if state.State != StateSynchronized || !state.Changed || state.Local.Head != wantHead {
						t.Fatalf("unpinned apply = %#v", state)
					}
				case "recover":
					state = f.service.Recover(f.ctx, false)
					if !state.Recovered || state.Changed || state.Pipeline.PushedHead != wantHead {
						t.Fatalf("unpinned recover = %#v", state)
					}
				}
				if calls != wantCalls {
					t.Fatalf("Git calls = %d, want %d", calls, wantCalls)
				}
			})
		}
	}
}

func selectedSyncFixture(t *testing.T, fork bool) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustRun(t, f.local, "init", "--bare", origin)
	mustRun(t, f.local, "push", origin, "HEAD:refs/heads/feature/sync")
	mustRun(t, f.local, "remote", "add", "origin", origin)
	mustRun(t, f.local, "remote", "add", "custom", f.remote)
	forkURL := ""
	if fork {
		forkURL = f.remote
		mustRun(t, f.local, "remote", "rename", "custom", "fork")
	}
	var err error
	f.repo, err = f.db.ReplaceRepoURLs(f.repo.ID, origin, forkURL)
	if err != nil {
		t.Fatal(err)
	}
	f.service.Repo = f.repo
	destination := &integration.Destination{
		Remote: "custom", URL: f.remote, TransportURL: f.remote,
		DefaultBranch: "main", Key: "abcdefghijklmnopqrstuvwx",
	}
	if fork {
		destination.URL, destination.TransportURL = origin, origin
	}
	f.run, err = f.db.InsertRunWithDestination(f.repo.ID, "feature/sync", f.old, f.base, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.pushed); err != nil {
		t.Fatal(err)
	}
	kind := "upstream"
	if fork {
		kind = "fork"
	}
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: f.pushed, TargetKind: kind, TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync",
	}); err != nil {
		t.Fatal(err)
	}
	completeSyncRun(t, f)
	return f
}

func TestIntegrationRemoteSyncUsesPublishedTarget(t *testing.T) {
	for _, fork := range []bool{false, true} {
		name := "selected upstream"
		if fork {
			name = "fork head"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := selectedSyncFixture(t, fork)
			origin := f.repo.UpstreamURL
			originConfig := mustRun(t, f.local, "config", "--get-regexp", "remote.origin.")
			state := f.service.InspectCached(f.ctx)
			if state.State != StateBehind || state.Target.URL != f.remote {
				t.Fatalf("cached selected push binding = %#v", state)
			}
			wantRemote, wantKind := "custom", "upstream"
			if fork {
				wantRemote, wantKind = "fork", "fork"
			}
			if state.Target.Remote != wantRemote || state.Target.Kind != wantKind {
				t.Fatalf("push destination = %#v", state.Target)
			}
			state = f.service.Apply(f.ctx)
			if state.State != StateSynchronized || !state.Changed || state.Local.Head != f.pushed {
				t.Fatalf("selected sync = %#v", state)
			}
			if got := mustRun(t, f.local, "show", "HEAD:fix.txt"); got != "pipeline fix" {
				t.Fatalf("pipeline content = %q", got)
			}
			if got := mustRun(t, origin, "rev-parse", "refs/heads/feature/sync"); got != f.old {
				t.Fatalf("sync changed origin: %s", got)
			}
			if got := mustRun(t, f.local, "config", "--get-regexp", "remote.origin."); got != originConfig {
				t.Fatalf("sync changed origin configuration: %s", got)
			}
		})
	}
}

func TestIntegrationRemoteSyncRejectsWrongPushBinding(t *testing.T) {
	t.Parallel()
	f := selectedSyncFixture(t, false)
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.repo.UpstreamURL), Ref: "refs/heads/feature/sync",
	}); err != nil {
		t.Fatal(err)
	}
	state := f.service.Apply(f.ctx)
	if state.Changed || state.State != StateTargetChanged {
		t.Fatalf("wrong target binding accepted: %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("refused sync moved HEAD: %s", got)
	}
}

func TestIntegrationRemoteSyncRecoversRewrittenSelectedTarget(t *testing.T) {
	t.Parallel()
	f := selectedSyncFixture(t, false)
	gate := filepath.Join(t.TempDir(), "gate.git")
	mustRun(t, f.local, "clone", "--bare", f.remote, gate)
	f.service.GateDir = gate
	rewritten := forceRewriteRemote(t, f, "selected-rewrite")
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || state.Changed || state.Pipeline.PushedHead != rewritten {
		t.Fatalf("selected target recovery = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("rebind changed local HEAD: %s", got)
	}
	if got := mustRun(t, gate, "rev-parse", state.Recovery.ArchiveRef+"^{commit}"); got != f.pushed {
		t.Fatalf("published head was not preserved: %s", got)
	}
}

func TestIntegrationRemoteSyncKeepsUnpublishedHeadOwned(t *testing.T) {
	t.Parallel()
	f := selectedSyncFixture(t, false)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.base); err != nil {
		t.Fatal(err)
	}
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned {
		t.Fatalf("unpublished selected run lost ownership: %#v", state)
	}
}

func TestIntegrationRemoteSyncAdoptsPublishedSelectedHead(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunCancelled)
	if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
		t.Fatal(err)
	}
	destination := &integration.Destination{
		Remote: "custom", URL: f.remote, TransportURL: f.remote,
		DefaultBranch: "main", Key: "abcdefghijklmnopqrstuvwx",
	}
	var err error
	f.run, err = f.db.InsertRunWithDestination(f.repo.ID, "feature/recover", f.submitted, f.base, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, f.preserved); err != nil {
		t.Fatal(err)
	}
	rebased := publishedRebaseAfterCustody(t, f)
	origin := filepath.Join(t.TempDir(), "product.git")
	mustRun(t, f.local, "init", "--bare", origin)
	mustRun(t, f.local, "remote", "rename", "origin", "custom")
	mustRun(t, f.local, "remote", "add", "origin", origin)
	f.repo, err = f.db.ReplaceRepoURLs(f.repo.ID, origin, "")
	if err != nil {
		t.Fatal(err)
	}
	f.service.Repo = f.repo
	state := f.service.AdoptPublished(f.ctx)
	if !state.Changed || state.Safety != "gate_ready" {
		t.Fatalf("selected published head not adopted: %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != rebased {
		t.Fatalf("gate head = %s, want %s", got, rebased)
	}
	if got := mustRun(t, origin, "for-each-ref", "--format=%(refname)"); got != "" {
		t.Fatalf("adoption changed origin: %s", got)
	}
}
