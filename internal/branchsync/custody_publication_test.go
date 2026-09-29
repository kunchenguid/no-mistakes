package branchsync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newCustodyPublicationFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, types.RunCancelled)
	if recovered := f.service.Recover(f.ctx, false); !recovered.Recovered {
		t.Fatalf("return custody: %#v", recovered)
	}
	mustRun(t, f.local, "reset", "--hard", f.submitted)
	mustWrite(t, filepath.Join(f.local, "correction.txt"), "corrected\n")
	mustRun(t, f.local, "add", "correction.txt")
	mustRun(t, f.local, "commit", "-m", "correct locally")
	state := f.service.InspectCached(f.ctx)
	if state.Safety != "publication_unverified" {
		t.Fatalf("expected ambiguous publication state, got %#v", state)
	}
	return f
}

func TestVerifyCustodyPublicationRechecksAssumptionsAfterRemoteRead(t *testing.T) {
	for _, name := range []string{"gate", "target", "head"} {
		t.Run(name, func(t *testing.T) {
			f := newCustodyPublicationFixture(t)
			f.service.lsRemote = func(context.Context, string, string, string) (string, error) {
				switch name {
				case "gate":
					mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
				case "target":
					other := filepath.Join(t.TempDir(), "other.git")
					mustRun(t, filepath.Dir(other), "init", "--bare", other)
					if _, err := f.db.UpdateRepoMetadata(f.repo.ID, other, "main"); err != nil {
						t.Fatal(err)
					}
				case "head":
					mustWrite(t, filepath.Join(f.local, "later.txt"), "later\n")
					mustRun(t, f.local, "add", "later.txt")
					mustRun(t, f.local, "commit", "-m", "change while checking")
				}
				return "", nil
			}
			checked := f.service.VerifyCustodyPublication(f.ctx)
			if checked.Safety != "blocked_assumptions_changed" || checked.NextAction != nil || checked.Error == "" {
				t.Fatalf("%s change did not fail closed: %#v", name, checked)
			}
		})
	}
}

func TestVerifyCustodyPublicationMissingTargetBranchIsNotPublished(t *testing.T) {
	f := newCustodyPublicationFixture(t)
	checked := f.service.VerifyCustodyPublication(f.ctx)
	if checked.Safety != "custody_returned" || checked.NextAction == nil || checked.NextAction.Code != "run_pipeline" || checked.Remote.ObservedHead != "" {
		t.Fatalf("missing target branch should permit only the exact recovered gate head: %#v", checked)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("read-only check changed gate head: %s", got)
	}
}
