package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestMirrorReconciliationUsesSelectedBase(t *testing.T) {
	for _, source := range []string{"default", "config", "run", "refreshed_upstream"} {
		for _, restored := range []bool{false, true} {
			if source == "refreshed_upstream" && !restored {
				continue
			}
			name := source + "/deleted"
			if restored {
				name = source + "/restored"
			}
			t.Run(name, func(t *testing.T) {
				dir, _, _ := setupGitRepo(t)
				write := func(name, content string) {
					t.Helper()
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				commit := func(message string) string {
					t.Helper()
					gitCmd(t, dir, "add", "-A")
					gitCmd(t, dir, "commit", "-m", message)
					return gitCmd(t, dir, "rev-parse", "HEAD")
				}
				write("doomed.txt", "original doomed\n")
				write("keep.txt", "original keep\n")
				base := commit("base files")
				gitCmd(t, dir, "checkout", "-B", "main", base)
				gitCmd(t, dir, "checkout", "-B", "feature", base)
				write("doomed.txt", "private doomed\n")
				write("keep.txt", "private keep\n")
				commit("private edits")
				write("other.txt", "private addition\n")
				private := commit("private addition")
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, private, config.Commands{})
				sctx.Run.Branch = "feature"
				sctx.Run.SubmittedHeadSHA = nil
				sctx.Repo.DefaultBranch = "main"
				gateDir := setupGateMirror(t, sctx)
				gitCmd(t, gateDir, "fetch", dir, private+":refs/heads/feature")
				gitCmd(t, dir, "checkout", "main")
				gitCmd(t, dir, "rm", "doomed.txt")
				deletedHead := commit("main deletion")
				selected := "integration"
				gitCmd(t, dir, "checkout", "-b", selected)
				if restored {
					write("doomed.txt", "original doomed\n")
					commit("restore required file")
				}
				upstream := t.TempDir()
				gitCmd(t, upstream, "init", "--bare")
				gitCmd(t, upstream, "symbolic-ref", "HEAD", "refs/heads/main")
				gitCmd(t, dir, "remote", "set-url", "origin", upstream)
				gitCmd(t, dir, "push", "origin", "main", selected)
				gitCmd(t, dir, "checkout", "-B", "feature", selected)
				write("keep.txt", "private keep\n")
				write("other.txt", "private addition\n")
				if restored {
					gitCmd(t, dir, "rm", "doomed.txt")
				}
				live := commit("rebased surviving work")
				switch source {
				case "default":
					sctx.Repo.DefaultBranch = selected
				case "config":
					sctx.Config.PR.BaseBranch = selected
				case "run":
					sctx.Config.PR.BaseBranch = "main"
					sctx.Run.PRBaseBranch = &selected
				case "refreshed_upstream":
					// The invoking checkout changes origin after the gate was
					// created. Its gate worktree keeps the old URL, whose selected
					// branch deletes a file the current upstream requires.
					stale := t.TempDir()
					gitCmd(t, stale, "init", "--bare")
					gitCmd(t, stale, "symbolic-ref", "HEAD", "refs/heads/"+selected)
					gitCmd(t, dir, "push", stale, deletedHead+":refs/heads/"+selected)
					gitCmd(t, dir, "remote", "set-url", "origin", stale)
					gateWork := t.TempDir()
					gitCmd(t, gateWork, "clone", stale, ".")
					gitCmd(t, gateWork, "fetch", dir, live)
					gitCmd(t, gateWork, "checkout", "--detach", live)
					gitCmd(t, dir, "remote", "set-url", "origin", upstream)
					sctx.WorkDir = gateWork
					sctx.Repo.UpstreamURL = upstream
					sctx.Repo.URLsVerified = true
					sctx.Config.PR.BaseBranch = selected
					if got := resolveUpstreamURL(sctx); got != upstream {
						t.Fatalf("run upstream = %q, want refreshed %q", got, upstream)
					}
				}
				plan, err := planGateMirrorReconciliation(sctx.Ctx, sctx, "refs/heads/feature", "feature", live)
				if restored {
					if err == nil || plan.Reconcile {
						t.Fatalf("branch deletion accepted: plan=%+v err=%v", plan, err)
					}
				} else if err != nil || !plan.Reconcile {
					t.Fatalf("base deletion refused: plan=%+v err=%v", plan, err)
				}
				if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != private {
					t.Fatalf("planning changed mirror to %s, want %s", got, private)
				}
				if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
					t.Fatalf("planning archived mirror: %s", tags)
				}
			})
		}
	}
}
