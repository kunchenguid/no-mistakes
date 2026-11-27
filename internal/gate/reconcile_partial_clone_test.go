package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A blobless partial clone holds commits whose intermediate blobs were never
// downloaded. Staging such a head into the private mirror runs upload-pack in
// the checkout, which disables lazy fetching by default, so the mirror fetch
// must let the checkout download what the gate needs from its promisor remote.
func TestPlanStaleBranchReconciliationStagesAHeadFromABloblessPartialClone(t *testing.T) {
	t.Parallel()
	for _, available := range []bool{true, false} {
		name := "promisor_unavailable"
		if available {
			name = "promisor_available"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			origin := initReconcileRepo(t)
			reconcileGit(t, origin, "config", "uploadpack.allowFilter", "true")
			base := reconcileGit(t, origin, "rev-parse", "HEAD")
			reconcileGit(t, origin, "checkout", "-b", "feature/partial")
			writeReconcileFile(t, origin, "feature.txt", "intermediate content\n")
			reconcileGit(t, origin, "add", "feature.txt")
			reconcileGit(t, origin, "commit", "-m", "intermediate")
			missingBlob := reconcileGit(t, origin, "rev-parse", "HEAD:feature.txt")
			writeReconcileFile(t, origin, "feature.txt", "final content\n")
			reconcileGit(t, origin, "commit", "-am", "final")

			partial := filepath.Join(t.TempDir(), "partial clone's $HOME")
			reconcileGit(t, "", "clone", "--no-local", "--filter=blob:none", "-b", "feature/partial", origin, partial)
			if got := reconcileGit(t, partial, "config", "--get", "remote.origin.partialclonefilter"); got != "blob:none" {
				t.Fatalf("fixture is not a blobless partial clone: filter %q", got)
			}
			writeReconcileFile(t, partial, "local.txt", "local work\n")
			reconcileGit(t, partial, "add", "local.txt")
			reconcileGit(t, partial, "commit", "-m", "local work")
			liveHead := reconcileGit(t, partial, "rev-parse", "HEAD")
			if blobPresent(partial, missingBlob) {
				t.Fatal("fixture partial clone already has the intermediate blob")
			}

			// The gate's private branch is an older head, so staging the live
			// head must transfer the intermediate commit and its blob.
			gateDir := filepath.Join(t.TempDir(), "gate.git")
			reconcileGit(t, "", "init", "--bare", gateDir)
			reconcileGit(t, gateDir, "fetch", origin, base+":refs/heads/feature/partial")

			if !available {
				reconcileGit(t, partial, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
				_, err := PlanStaleBranchReconciliation(ctx, gateDir, partial, "feature/partial", liveHead, "", nil)
				if err == nil {
					t.Fatal("staging succeeded without the promisor remote")
				}
				t.Logf("Staging refusal: %v", err)
				for _, want := range []string{"partial clone", missingBlob} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error does not name %q:\n%v", want, err)
					}
				}
				_, after, found := strings.Cut(err.Error(), "run `")
				if !found {
					t.Fatalf("error does not include a recovery command:\n%v", err)
				}
				recovery, _, found := strings.Cut(after, "` to download it")
				if !found {
					t.Fatalf("error does not delimit the recovery command:\n%v", err)
				}
				reconcileGit(t, partial, "remote", "set-url", "origin", origin)
				cmd := exec.Command("sh", "-c", recovery)
				cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=0")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("run printed recovery command %s: %v\n%s", recovery, err, out)
				}
				if !blobPresent(partial, missingBlob) {
					t.Fatal("printed recovery command did not hydrate the missing blob")
				}
			}

			plan, err := PlanStaleBranchReconciliation(ctx, gateDir, partial, "feature/partial", liveHead, "", nil)
			if err != nil {
				t.Fatalf("stage head from partial clone: %v", err)
			}
			if plan.Reconcile {
				t.Fatalf("ancestor gate head needs no reconciliation: %+v", plan)
			}
			if !blobPresent(gateDir, missingBlob) {
				t.Fatal("gate did not receive the intermediate blob")
			}
			if !blobPresent(partial, missingBlob) {
				t.Fatal("partial clone did not hydrate the intermediate blob")
			}
		})
	}
}

func blobPresent(dir, oid string) bool {
	args := []string{"cat-file", "-e", oid}
	if strings.HasSuffix(dir, ".git") {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	return cmd.Run() == nil
}
