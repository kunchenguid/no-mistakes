package branchsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
)

func TestApplyRefusesIgnoredCollisionCreatedBeforeApply(t *testing.T) {
	t.Parallel()
	for _, ignoreCase := range []bool{false, true} {
		for _, diverged := range []bool{false, true} {
			t.Run("ignoreCase="+strconv.FormatBool(ignoreCase)+"/diverged="+strconv.FormatBool(diverged), func(t *testing.T) {
				f := newSyncFixture(t)
				if diverged {
					rebuildPipelineHead(t, f, []pipelineCommit{
						{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
					})
				}
				mustRun(t, f.local, "config", "core.ignorecase", strconv.FormatBool(ignoreCase))
				local := "node_modules"
				if ignoreCase {
					local = "Node_Modules"
				}
				pipeline := filepath.Join(filepath.Dir(f.local), "ignored-pipeline")
				mustRun(t, filepath.Dir(f.local), "clone", f.remote, pipeline)
				mustRun(t, pipeline, "checkout", "feature/sync")
				if err := os.Symlink("node_modules", filepath.Join(pipeline, "node_modules")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				mustRun(t, pipeline, "add", "node_modules")
				mustRun(t, pipeline, "commit", "-m", "incoming symlink")
				f.pushed = mustRun(t, pipeline, "rev-parse", "HEAD")
				mustRun(t, pipeline, "push", f.remote, "HEAD:refs/heads/feature/sync")
				if err := f.db.UpdateRunHeadSHA(f.run.ID, f.pushed); err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync"}); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(f.local, ".git", "info", "exclude"), local+"/\n")
				plan := f.service.Refresh(f.ctx)
				wantSafety := SafetySafeFastForward
				if diverged {
					wantSafety = SafetySafeEquivalentAdvance
				}
				if !CanApply(plan) || plan.Safety != wantSafety {
					t.Fatalf("fixture does not exercise expected advance: %#v", plan)
				}
				anchorRef := syncAnchorRef(f.run.ID)
				if _, err := gitpkg.Run(f.ctx, f.local, "show-ref", "--verify", anchorRef); err == nil {
					t.Fatal("fixture already has a recovery anchor")
				}
				index, err := os.ReadFile(filepath.Join(f.local, ".git", "index"))
				if err != nil {
					t.Fatal(err)
				}
				called := false
				f.service.beforeApply = func() {
					called = true
					mustWrite(t, filepath.Join(f.local, local, "sentinel"), "precious ignored bytes\n")
				}
				state := f.service.Apply(f.ctx)
				if !called || state.Safety != "blocked_ignored_path_collision" || !strings.Contains(state.Error, strconv.Quote(local)) {
					t.Errorf("late collision = %#v, seam called = %v", state, called)
				}
				if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
					t.Errorf("HEAD changed to %s", got)
				}
				if _, err := gitpkg.Run(f.ctx, f.local, "show-ref", "--verify", anchorRef); err == nil {
					t.Error("late collision created a recovery anchor")
				}
				after, err := os.ReadFile(filepath.Join(f.local, ".git", "index"))
				if err != nil || !bytes.Equal(index, after) {
					t.Errorf("index changed: %v", err)
				}
				contents, err := os.ReadFile(filepath.Join(f.local, local, "sentinel"))
				if err != nil || string(contents) != "precious ignored bytes\n" {
					t.Errorf("ignored bytes changed: %q, %v", contents, err)
				}
			})
		}
	}
}
