package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconciliationRejectsSymbolicRefs(t *testing.T) {
	for _, phase := range []string{"plan", "apply"} {
		for _, symbolic := range []string{"branch", "dangling_branch", "archive_branch", "archive_other", "dangling_archive"} {
			t.Run(phase+"/"+symbolic, func(t *testing.T) {
				ctx := context.Background()
				work, gateDir, privateHead, liveHead := reconciliationRefFixture(t)
				plan, err := PlanMirrorPublicationReconciliation(ctx, gateDir, work, "feature", liveHead, privateHead)
				if err != nil || !plan.Reconcile {
					t.Fatalf("direct-ref fixture plan = %+v, err = %v", plan, err)
				}
				ref, target := plan.ArchiveTag, "refs/heads/feature"
				switch symbolic {
				case "branch":
					ref, target = plan.BranchRef, "refs/heads/other"
				case "dangling_branch":
					ref, target = plan.BranchRef, "refs/heads/missing"
				case "archive_other":
					target = "refs/heads/other"
				case "dangling_archive":
					target = "refs/heads/missing"
				}
				reconcileGit(t, gateDir, "symbolic-ref", ref, target)
				refsBefore := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
				if phase == "plan" {
					_, err = PlanMirrorPublicationReconciliation(ctx, gateDir, work, "feature", liveHead, privateHead)
				} else {
					_, err = ApplyStaleBranchReconciliation(ctx, gateDir, plan)
				}
				if err == nil || !strings.Contains(err.Error(), "symbolic") || !strings.Contains(err.Error(), ref) {
					t.Fatalf("%s accepted symbolic ref %s or lost its cause: %v", phase, ref, err)
				}
				if got := reconcileGit(t, gateDir, "symbolic-ref", "--no-recurse", ref); got != target {
					t.Fatalf("symbolic ref changed to %s, want %s", got, target)
				}
				if got := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"); got != refsBefore {
					t.Fatalf("refusal changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, got)
				}
				if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/other"); got != privateHead {
					t.Fatalf("refusal moved unrelated target to %s", got)
				}
				if strings.Contains(symbolic, "archive") && ArchivedHeadRecorded(ctx, gateDir, "feature", privateHead) {
					t.Fatal("symbolic archive accepted as exact abandoned-head evidence")
				}
			})
		}
	}
}

func TestReconciliationDirectArchiveSurvivesBranchRecreation(t *testing.T) {
	for _, archiveState := range []string{"new", "existing"} {
		t.Run(archiveState, func(t *testing.T) {
			ctx := context.Background()
			work, gateDir, privateHead, liveHead := reconciliationRefFixture(t)
			archive := "refs/tags/no-mistakes-abandoned/feature/" + privateHead
			if archiveState == "existing" {
				reconcileGit(t, gateDir, "update-ref", "--no-deref", archive, privateHead, strings.Repeat("0", len(privateHead)))
			}
			hook := `#!/bin/sh
[ "$1" = "prepared" ] || exit 0
while read old new ref; do
  if [ "$ref" = "refs/heads/feature" ] && [ "$new" = "0000000000000000000000000000000000000000" ]; then
    archived=$(git --git-dir=. rev-parse --verify "refs/tags/no-mistakes-abandoned/feature/$old") || exit 1
    [ "$archived" = "$old" ] || exit 1
    printf '%s' "$archived" > archive-before-delete.observed
  fi
done
`
			if err := os.WriteFile(filepath.Join(gateDir, "hooks", "reference-transaction"), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}
			result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature", liveHead, privateHead)
			if err != nil || !result.Reconciled || result.PreviousHead != privateHead || result.ArchivedTag != archive {
				t.Fatalf("direct Decision 41-A reconciliation = %+v, err = %v", result, err)
			}
			observed, err := os.ReadFile(filepath.Join(gateDir, "archive-before-delete.observed"))
			if err != nil || string(observed) != privateHead {
				t.Fatalf("deletion hook did not observe the exact archive: %q, err = %v", observed, err)
			}
			if refs := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname)", "refs/heads/feature"); refs != "" {
				t.Fatalf("reconciled branch still exists: %s", refs)
			}
			wantArchive := archive + " " + privateHead
			if got := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", archive); got != wantArchive {
				t.Fatalf("archive after deletion = %q, want direct ref %q", got, wantArchive)
			}
			reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != liveHead {
				t.Fatalf("ordinary recreation reached %s, want %s", got, liveHead)
			}
			if got := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", archive); got != wantArchive {
				t.Fatalf("archive moved after branch recreation: %q", got)
			}
			if !ArchivedHeadRecorded(ctx, gateDir, "feature", privateHead) {
				t.Fatal("direct abandoned head was not retained")
			}
			if got := reconcileGit(t, gateDir, "show", archive+":private.txt"); got != "private work" {
				t.Fatalf("abandoned content is not recoverable: %q", got)
			}
		})
	}
}

func reconciliationRefFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")
	writeReconcileFile(t, work, "private.txt", "private work\n")
	reconcileGit(t, work, "add", "private.txt")
	reconcileGit(t, work, "commit", "-m", "submitted private work")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")
	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "live.txt", "reviewed rewrite\n")
	reconcileGit(t, work, "add", "live.txt")
	reconcileGit(t, work, "commit", "-m", "reviewed rewrite")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature", privateHead+":refs/heads/other")
	return work, gateDir, privateHead, liveHead
}
