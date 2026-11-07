package branchsync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
)

func newRewrittenSubmissionFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	root := filepath.Dir(f.local)
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "clone", "--bare", f.remote, gate)
	f.service.GateDir = gate

	mustRun(t, f.local, "fetch", f.remote, "refs/heads/feature/sync:refs/no-mistakes/test-published")
	mustRun(t, f.local, "reset", "--hard", f.base)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "deliberately rewritten feature\n")
	mustRun(t, f.local, "add", "file.txt")
	mustRun(t, f.local, "commit", "-m", "rewrite unpublished feature")
	state := f.service.InspectCached(f.ctx)
	if state.Safety != "rewrite_preparation_required" {
		t.Fatalf("fixture state = %#v", state)
	}
	return f
}

func TestPrepareRewrittenSubmissionRefusesCheckoutMovedDuringFinalRemoteLookup(t *testing.T) {
	f := newRewrittenSubmissionFixture(t)
	original := mustRun(t, f.local, "rev-parse", "HEAD")

	remoteCalls := 0
	f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
		remoteCalls++
		live, err := gitpkg.LsRemote(ctx, dir, remote, ref)
		if err == nil && remoteCalls == 2 {
			mustRun(t, f.local, "commit", "--allow-empty", "-m", "move checkout during final remote lookup")
		}
		return live, err
	}

	state := f.service.PrepareRewrittenSubmission(f.ctx)
	if state.Safety != "blocked_prepare_rewritten_assumptions_changed" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
	if remoteCalls != 2 {
		t.Fatalf("remote calls = %d, want the checkout move during the final lookup", remoteCalls)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got == original {
		t.Fatal("fixture did not move the checkout during the final remote lookup")
	}
	if _, exists, err := gitpkg.DirectRefTarget(context.Background(), f.service.GateDir, custody.RecoveryRef(f.run.ID)); err != nil || exists {
		t.Fatalf("moved checkout received recovery credit: exists=%v err=%v", exists, err)
	}
}
