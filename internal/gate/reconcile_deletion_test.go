package gate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReconcileStaleBranchRebasedAcrossBaseDeletion(t *testing.T) {
	for _, change := range []string{"kept", "later_edit", "missing_edit", "missing_addition", "branch_deletion", "merge_failure", "tree_failure", "upstream_failure", "older_upstream", "selected_restores", "selected_deletes", "missing_base", "unknown_base", "missing_upstream", "stale_origin", "many_deletions"} {
		t.Run(change, func(t *testing.T) {
			work := initReconcileRepo(t)
			reconcileGit(t, work, "branch", "-M", "main")
			doomedPaths := []string{"doomed.txt"}
			if change == "many_deletions" {
				for i := 0; i < 64; i++ {
					doomedPaths = append(doomedPaths, fmt.Sprintf("doomed path %03d.txt", i))
				}
				doomedPaths = append(doomedPaths, "--literal-doomed.txt")
				if runtime.GOOS != "windows" {
					doomedPaths = append(doomedPaths, "doomed\nnewline.txt")
				}
			}
			for _, path := range doomedPaths {
				writeReconcileFile(t, work, path, "original doomed\n")
			}
			writeReconcileFile(t, work, "keep.txt", "original keep\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "base files")
			base := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "checkout", "-b", "feature")
			for _, path := range doomedPaths {
				writeReconcileFile(t, work, path, "private doomed\n")
			}
			writeReconcileFile(t, work, "keep.txt", "private keep\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "edit both files")
			firstPrivate := reconcileGit(t, work, "rev-parse", "HEAD")
			writeReconcileFile(t, work, "other.txt", "private addition\n")
			reconcileGit(t, work, "add", "other.txt")
			reconcileGit(t, work, "commit", "-m", "add other file")
			privateHead := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "checkout", "main")
			if change != "branch_deletion" && change != "selected_deletes" {
				reconcileGit(t, work, append([]string{"rm", "--"}, doomedPaths...)...)
				reconcileGit(t, work, "commit", "-m", "base deletes doomed")
			} else {
				writeReconcileFile(t, work, "base.txt", "advanced base\n")
				reconcileGit(t, work, "add", "base.txt")
				reconcileGit(t, work, "commit", "-m", "advance base without deletion")
			}
			newBase := reconcileGit(t, work, "rev-parse", "HEAD")
			upstream := filepath.Join(t.TempDir(), "upstream.git")
			reconcileGit(t, "", "init", "--bare", upstream)
			reconcileGit(t, upstream, "symbolic-ref", "HEAD", "refs/heads/main")
			upstreamHead := newBase
			if change == "older_upstream" {
				upstreamHead = reconcileGit(t, work, "rev-parse", base+"^")
			}
			reconcileGit(t, work, "push", upstream, upstreamHead+":refs/heads/main")
			reconcileGit(t, work, "remote", "add", "origin", upstream)
			selectedBase := "main"
			if change == "selected_restores" || change == "selected_deletes" {
				selectedBase = "integration"
				reconcileGit(t, work, "checkout", "-b", selectedBase)
				if change == "selected_restores" {
					writeReconcileFile(t, work, "doomed.txt", "original doomed\n")
					reconcileGit(t, work, "add", "doomed.txt")
				} else {
					reconcileGit(t, work, "rm", "doomed.txt")
				}
				reconcileGit(t, work, "commit", "-m", "selected integration base")
				reconcileGit(t, work, "push", upstream, "HEAD:refs/heads/"+selectedBase)
			}
			reconcileGit(t, work, "checkout", "feature")
			if change != "branch_deletion" && change != "selected_restores" {
				if _, err := reconcileGitAllowFail(work, "rebase", selectedBase); err == nil {
					t.Fatal("fixture must encounter the modify/delete conflict")
				}
				reconcileGit(t, work, append([]string{"rm", "--"}, doomedPaths...)...)
				t.Setenv("GIT_EDITOR", "true")
				reconcileGit(t, work, "rebase", "--continue")
			} else {
				reconcileGit(t, work, "rebase", selectedBase)
				reconcileGit(t, work, "rm", "doomed.txt")
				reconcileGit(t, work, "commit", "-m", "branch discards required edit")
			}
			switch change {
			case "later_edit":
				writeReconcileFile(t, work, "keep.txt", "later private keep\n")
				reconcileGit(t, work, "add", "keep.txt")
				reconcileGit(t, work, "commit", "-m", "edit the surviving work later")
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
			if change == "merge_failure" || change == "tree_failure" {
				if runtime.GOOS == "windows" {
					t.Skip("Git command failure injection uses a POSIX shell wrapper")
				}
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				command := "merge-tree"
				if change == "tree_failure" {
					command = "write-tree"
				}
				bin := t.TempDir()
				wrapper := fmt.Sprintf("#!/bin/sh\nfor arg do\n  [ \"$arg\" != %q ] || exit 37\ndone\nexec %q \"$@\"\n", command, realGit)
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			if change == "upstream_failure" {
				upstream = filepath.Join(t.TempDir(), "missing.git")
				reconcileGit(t, work, "remote", "set-url", "origin", upstream)
			}
			if change == "missing_base" {
				selectedBase = ""
			} else if change == "unknown_base" {
				selectedBase = "missing"
			}
			proofUpstream := ReconciliationUpstream{URL: upstream, BaseBranch: selectedBase}
			if change == "missing_upstream" {
				proofUpstream.URL = ""
			}
			if change == "stale_origin" {
				reconcileGit(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
			}
			trace := filepath.Join(t.TempDir(), "git.trace")
			if change == "many_deletions" {
				t.Setenv("GIT_TRACE", trace)
			}
			resolutions := 0
			resolveUpstream := func() ReconciliationUpstream {
				resolutions++
				return proofUpstream
			}
			plan, err := PlanStaleBranchReconciliation(context.Background(), gateDir, work, "feature", liveHead, "", resolveUpstream)
			publicationPlan, publicationErr := PlanMirrorPublicationReconciliation(context.Background(), gateDir, work, "feature", liveHead, resolveUpstream)
			if (err == nil) != (publicationErr == nil) || plan.Reconcile != publicationPlan.Reconcile {
				t.Fatalf("submission/publication differ: plan=%+v err=%v; publication=%+v err=%v", plan, err, publicationPlan, publicationErr)
			}
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != privateHead {
				t.Fatalf("planning moved mirror to %s, want %s", got, privateHead)
			}
			if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
				t.Fatalf("planning created archive %s", tags)
			}
			if change != "kept" && change != "later_edit" && change != "selected_deletes" && change != "stale_origin" && change != "many_deletions" {
				if err == nil || plan.Reconcile {
					t.Fatalf("lost content accepted: plan=%+v err=%v", plan, err)
				}
				for _, commit := range []string{firstPrivate, privateHead} {
					if !strings.Contains(err.Error(), commit) {
						t.Fatalf("refusal omitted at-risk commit %s: %v", commit, err)
					}
				}
				return
			}
			if err != nil || !plan.Reconcile {
				t.Fatalf("accepted base deletion refused: plan=%+v err=%v", plan, err)
			}
			if change == "many_deletions" {
				raw, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				updates := 0
				for _, line := range strings.Split(string(raw), "\n") {
					if strings.Contains(line, " update-index ") {
						updates++
					}
				}
				if resolutions != 2 {
					t.Fatalf("upstream resolutions = %d, want one per planning proof (2 total)", resolutions)
				}
				if updates != 2 {
					t.Fatalf("index update processes = %d, want one per planning proof (2 total)", updates)
				}
			}
			result, err := ApplyStaleBranchReconciliation(context.Background(), gateDir, plan)
			if err != nil || !result.Reconciled || result.PreviousHead != privateHead {
				t.Fatalf("apply = %+v, err=%v", result, err)
			}
			if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag+"^{commit}"); got != privateHead {
				t.Fatalf("archive = %s, want exact old mirror %s", got, privateHead)
			}
			if observed, err := os.ReadFile(filepath.Join(gateDir, "archive-before-delete.observed")); err != nil || string(observed) != privateHead {
				t.Fatalf("deletion did not observe the exact archive: %q, err=%v", observed, err)
			}
			if _, err := reconcileGitAllowFail(gateDir, "show-ref", "--verify", "refs/heads/feature"); err == nil {
				t.Fatal("old mirror ref remains after archival")
			}
			reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature^{tree}"); got != liveTree {
				t.Fatalf("replacement tree = %s, want %s", got, liveTree)
			}
			wantKeep := "private keep"
			if change == "later_edit" {
				wantKeep = "later private keep"
			}
			if got := reconcileGit(t, gateDir, "show", "refs/heads/feature:keep.txt"); got != wantKeep {
				t.Fatalf("surviving edit = %q", got)
			}
			if got := reconcileGit(t, gateDir, "show", "refs/heads/feature:other.txt"); got != "private addition" {
				t.Fatalf("surviving addition = %q", got)
			}
		})
	}
}
