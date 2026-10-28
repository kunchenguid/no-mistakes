package branchsync

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecoverKeepLocalFromPartialClone(t *testing.T) {
	t.Parallel()
	for _, available := range []bool{true, false} {
		name := "promisor_unavailable"
		if available {
			name = "promisor_available"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRecoverFixture(t, types.RunCIMonitorInterrupted)
			mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
			mustWrite(t, filepath.Join(f.local, "removed.txt"), "only historical content\n")
			mustRun(t, f.local, "add", "removed.txt")
			mustRun(t, f.local, "commit", "-m", "add historical content")
			missingBlob := mustRun(t, f.local, "rev-parse", "HEAD:removed.txt")
			mustRun(t, f.local, "rm", "removed.txt")
			mustRun(t, f.local, "commit", "-m", "remove historical content")
			keptHead := mustRun(t, f.local, "rev-parse", "HEAD")
			mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/recover")
			mustRun(t, f.remote, "config", "uploadpack.allowFilter", "true")
			mustRun(t, f.remote, "config", "uploadpack.allowAnySHA1InWant", "true")
			partial := filepath.Join(t.TempDir(), "partial")
			mustRun(t, f.local, "clone", "--no-local", "--filter=blob:none", "--no-checkout", "-b", "feature/recover", f.remote, partial)
			mustRun(t, partial, "checkout", "feature/recover")
			if _, err := gitpkg.RunWithEnv(f.ctx, partial, []string{"GIT_NO_LAZY_FETCH=1"}, "cat-file", "-e", missingBlob); err == nil {
				t.Fatal("partial clone unexpectedly has the historical blob")
			}
			for range 2 {
				if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
					HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
				}); err != nil {
					t.Fatal(err)
				}
			}
			repo, err := f.db.UpdateRepoWorkingPath(f.repo.ID, partial)
			if err != nil {
				t.Fatal(err)
			}
			f.service.Repo = repo
			f.service.WorkDir = partial
			if !available {
				mustRun(t, partial, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
			}
			kept := f.service.Recover(f.ctx, true)
			if !available {
				if kept.Recovered || kept.Changed || kept.Safety != "blocked_recover_assumptions_changed" || f.custodyReturned() {
					t.Fatalf("unavailable promisor returned custody: %#v", kept)
				}
				if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
					t.Fatalf("failed recovery moved gate to %s", got)
				}
				if got := mustRun(t, partial, "rev-parse", "HEAD"); got != keptHead {
					t.Fatalf("failed recovery moved local head to %s", got)
				}
				return
			}
			if !kept.Recovered || kept.Changed {
				t.Fatalf("keep-local from partial clone = %#v", kept)
			}
			if got := mustRun(t, partial, "rev-parse", "HEAD"); got != keptHead {
				t.Fatalf("local head changed to %s", got)
			}
			if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != keptHead {
				t.Fatalf("gate head = %s, want %s", got, keptHead)
			}
			if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
				t.Fatalf("recovery anchor changed to %s", got)
			}
			if !f.custodyReturned() {
				t.Fatal("keep-local did not return custody")
			}
		})
	}
}
