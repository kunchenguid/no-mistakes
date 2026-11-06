package gate

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReconcileStaleBranchRebasedAcrossBaseDeletion(t *testing.T) {
	for _, change := range []string{"kept", "missing_edit", "missing_addition", "branch_deletion"} {
		t.Run(change, func(t *testing.T) {
			work := initReconcileRepo(t)
			reconcileGit(t, work, "branch", "-M", "main")
			writeReconcileFile(t, work, "doomed.txt", "original doomed\n")
			writeReconcileFile(t, work, "keep.txt", "original keep\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "base files")
			base := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "checkout", "-b", "feature")
			writeReconcileFile(t, work, "doomed.txt", "private doomed\n")
			writeReconcileFile(t, work, "keep.txt", "private keep\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "edit both files")
			writeReconcileFile(t, work, "other.txt", "private addition\n")
			reconcileGit(t, work, "add", "other.txt")
			reconcileGit(t, work, "commit", "-m", "add other file")
			privateHead := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "checkout", "main")
			if change != "branch_deletion" {
				reconcileGit(t, work, "rm", "doomed.txt")
				reconcileGit(t, work, "commit", "-m", "base deletes doomed")
			} else {
				writeReconcileFile(t, work, "base.txt", "advanced base\n")
				reconcileGit(t, work, "add", "base.txt")
				reconcileGit(t, work, "commit", "-m", "advance base without deletion")
			}
			newBase := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "checkout", "feature")
			if change != "branch_deletion" {
				if _, err := reconcileGitAllowFail(work, "rebase", "main"); err == nil {
					t.Fatal("fixture must encounter the modify/delete conflict")
				}
				reconcileGit(t, work, "rm", "doomed.txt")
				t.Setenv("GIT_EDITOR", "true")
				reconcileGit(t, work, "rebase", "--continue")
			} else {
				reconcileGit(t, work, "rebase", "main")
				reconcileGit(t, work, "rm", "doomed.txt")
				reconcileGit(t, work, "commit", "-m", "branch discards required edit")
			}
			switch change {
			case "missing_edit":
				writeReconcileFile(t, work, "keep.txt", "original keep\n")
				reconcileGit(t, work, "add", "keep.txt")
				reconcileGit(t, work, "commit", "-m", "discard surviving edit")
			case "missing_addition":
				reconcileGit(t, work, "rm", "other.txt")
				reconcileGit(t, work, "commit", "-m", "discard addition")
			}
			liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
			liveTree := reconcileGit(t, work, "rev-parse", "HEAD^{tree}")
			if base == newBase {
				t.Fatal("fixture did not advance main")
			}
			gateDir := filepath.Join(t.TempDir(), "gate.git")
			reconcileGit(t, "", "init", "--bare", gateDir)
			reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature")
			plan, err := PlanStaleBranchReconciliation(context.Background(), gateDir, work, "feature", liveHead, "")
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != privateHead {
				t.Fatalf("planning moved mirror to %s, want %s", got, privateHead)
			}
			if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
				t.Fatalf("planning created archive %s", tags)
			}
			if change != "kept" {
				if err == nil || plan.Reconcile {
					t.Fatalf("lost content accepted: plan=%+v err=%v", plan, err)
				}
				return
			}
			if err != nil || !plan.Reconcile {
				t.Fatalf("accepted base deletion refused: plan=%+v err=%v", plan, err)
			}
			result, err := ApplyStaleBranchReconciliation(context.Background(), gateDir, plan)
			if err != nil || !result.Reconciled || result.PreviousHead != privateHead {
				t.Fatalf("apply = %+v, err=%v", result, err)
			}
			if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag+"^{commit}"); got != privateHead {
				t.Fatalf("archive = %s, want exact old mirror %s", got, privateHead)
			}
			if _, err := reconcileGitAllowFail(gateDir, "show-ref", "--verify", "refs/heads/feature"); err == nil {
				t.Fatal("old mirror ref remains after archival")
			}
			reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature^{tree}"); got != liveTree {
				t.Fatalf("replacement tree = %s, want %s", got, liveTree)
			}
			if got := reconcileGit(t, gateDir, "show", "refs/heads/feature:keep.txt"); got != "private keep" {
				t.Fatalf("surviving edit = %q", got)
			}
			if got := reconcileGit(t, gateDir, "show", "refs/heads/feature:other.txt"); got != "private addition" {
				t.Fatalf("surviving addition = %q", got)
			}
		})
	}
}
