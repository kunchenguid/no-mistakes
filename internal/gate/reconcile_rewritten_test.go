package gate

import (
	"context"
	"strings"
	"testing"
)

func TestPlanStaleBranchReconciliationRewrittenRecoveryAnchors(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"gate", "worktree"} {
		for _, shape := range []string{"full", "partial", "symbolic", "dangling-symbolic", "tree", "unrelated", "recover-local", "lookalike"} {
			t.Run(location+"/"+shape, func(t *testing.T) {
				t.Parallel()
				work, gateDir, privateHead, first, liveHead := setupAnchoredPrivateBranch(t)
				dir := gateDir
				if location == "worktree" {
					dir = work
				} else {
					reconcileGit(t, gateDir, "fetch", work, liveHead+":refs/heads/live")
				}
				anchor := "refs/no-mistakes/recover-rewritten/run/1"
				switch shape {
				case "full":
					reconcileGit(t, dir, "update-ref", anchor, privateHead)
				case "partial":
					reconcileGit(t, dir, "update-ref", anchor, first)
				case "symbolic":
					reconcileGit(t, dir, "update-ref", "refs/heads/symbolic-target", privateHead)
					reconcileGit(t, dir, "symbolic-ref", anchor, "refs/heads/symbolic-target")
				case "dangling-symbolic":
					reconcileGit(t, dir, "symbolic-ref", anchor, "refs/heads/missing")
				case "tree":
					reconcileGit(t, dir, "update-ref", anchor, privateHead+"^{tree}")
				case "unrelated":
					reconcileGit(t, dir, "update-ref", anchor, liveHead)
				case "recover-local":
					reconcileGit(t, dir, "update-ref", "refs/no-mistakes/recover-local/run", privateHead)
				case "lookalike":
					reconcileGit(t, dir, "update-ref", "refs/no-mistakes/recover-rewritten-other/run/1", privateHead)
				}
				beforeGate := reconcileGit(t, gateDir, "for-each-ref")
				beforeWork := reconcileGit(t, work, "for-each-ref")
				plan, err := PlanStaleBranchReconciliation(context.Background(), gateDir, work, "feature/reconcile", liveHead, "")
				if got := reconcileGit(t, gateDir, "for-each-ref"); got != beforeGate {
					t.Fatalf("planning changed gate refs:\nbefore: %s\nafter: %s", beforeGate, got)
				}
				if got := reconcileGit(t, work, "for-each-ref"); got != beforeWork {
					t.Fatalf("planning changed worktree refs:\nbefore: %s\nafter: %s", beforeWork, got)
				}
				if shape != "full" {
					if err == nil || plan.Reconcile || !strings.Contains(err.Error(), privateHead) {
						t.Fatalf("unpreserved head not refused: plan=%+v err=%v", plan, err)
					}
					if shape == "partial" && strings.Contains(err.Error(), first) {
						t.Fatalf("partial anchor failed to credit the first commit: %v", err)
					}
					if !strings.Contains(err.Error(), "refs/no-mistakes/recover-rewritten/<run>/<generation>") {
						t.Fatalf("refusal did not name rewritten recovery satisfier: %v", err)
					}
					return
				}
				if err != nil || !plan.Reconcile {
					t.Fatalf("preserved rewritten head was refused: plan=%+v err=%v", plan, err)
				}
				for _, commit := range []string{first, privateHead} {
					if got := plan.PreservedByRecovery[commit]; got != anchor {
						t.Fatalf("commit %s credited to %q, want %q", commit, got, anchor)
					}
				}
				result, err := ApplyStaleBranchReconciliation(context.Background(), gateDir, plan)
				if err != nil || !result.Reconciled {
					t.Fatalf("apply preserved plan: result=%+v err=%v", result, err)
				}
				if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != privateHead {
					t.Fatalf("archive = %s, want %s", got, privateHead)
				}
				if got := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname)", "refs/heads/feature/reconcile"); got != "" {
					t.Fatalf("archived branch was not removed: %s", got)
				}
				if got := reconcileGit(t, dir, "rev-parse", anchor); got != privateHead {
					t.Fatalf("recovery anchor changed to %s", got)
				}
			})
		}
	}
}
