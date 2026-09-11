package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestMirrorSettlementFailureRestoresArchivedBranch(t *testing.T) {
	for _, failure := range []string{"fetch", "recreate"} {
		t.Run(failure, func(t *testing.T) {
			dir, base, submitted := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, submitted, config.Commands{})
			sctx.Run.Branch = "feature"
			sctx.Run.SubmittedHeadSHA = &submitted
			gateDir := setupGateMirror(t, sctx)
			gitCmd(t, gateDir, "fetch", dir, submitted+":refs/heads/feature")
			gitCmd(t, dir, "checkout", "--detach", base)
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "reviewed rewrite")
			reviewed := gitCmd(t, dir, "rev-parse", "HEAD")
			plan, err := planGateMirrorReconciliation(sctx.Ctx, sctx, "refs/heads/feature", "feature", reviewed)
			if err != nil || !plan.Reconcile {
				t.Fatalf("plan = %+v, error = %v", plan, err)
			}

			// Model the already-verified upstream publication, then fail one of the
			// remaining local operations. All objects and refs are real Git state.
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			gitCmd(t, dir, "push", upstream, reviewed+":refs/heads/feature")
			hook := filepath.Join(gateDir, "hooks", "reference-transaction")
			wantError := "fetch pushed head"
			condition := "case \"$ref\" in refs/no-mistakes/fetch/*) exit 1;; esac"
			if failure == "recreate" {
				wantError = "update gate mirror ref refs/heads/feature to"
				condition = "if [ \"$ref\" = refs/heads/feature ] && [ \"$new\" = " + reviewed + " ]; then exit 1; fi"
			}
			// Reject the selected operation using Git's real transaction hook.
			// Archival, deletion, and restoration of the old head remain valid.
			script := "#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\nwhile read old new ref; do\n" + condition + "\ndone\nexit 0\n"
			if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			err = updateGateMirrorAfterPush(sctx.Ctx, sctx, "refs/heads/feature", reviewed, plan)
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("settlement error = %v, want %s", err, wantError)
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != submitted {
				t.Fatalf("mirror after failure = %s, want restored %s", got, submitted)
			}
			if got := gitCmd(t, gateDir, "rev-parse", plan.ArchiveTag); got != submitted {
				t.Fatalf("archive = %s, want %s", got, submitted)
			}
			if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != reviewed {
				t.Fatalf("upstream = %s, want %s", got, reviewed)
			}
			if err := os.Remove(hook); err != nil {
				t.Fatal(err)
			}
			if err := updateGateMirrorAfterPush(sctx.Ctx, sctx, "refs/heads/feature", reviewed, plan); err != nil {
				t.Fatalf("retry settlement: %v", err)
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != reviewed {
				t.Fatalf("mirror after retry = %s, want %s", got, reviewed)
			}
		})
	}
}
