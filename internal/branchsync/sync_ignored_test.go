package branchsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestApplyRefusesIgnoredCollisionCreatedBeforeApply(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
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
	mustWrite(t, filepath.Join(f.local, ".git", "info", "exclude"), "node_modules/\n")
	index, err := os.ReadFile(filepath.Join(f.local, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	f.service.beforeApply = func() {
		called = true
		mustWrite(t, filepath.Join(f.local, "node_modules", "sentinel"), "precious ignored bytes\n")
	}
	state := f.service.Apply(f.ctx)
	if !called || state.Safety != "blocked_ignored_path_collision" || !strings.Contains(state.Error, `"node_modules"`) {
		t.Errorf("late collision = %#v, seam called = %v", state, called)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Errorf("HEAD changed to %s", got)
	}
	after, err := os.ReadFile(filepath.Join(f.local, ".git", "index"))
	if err != nil || !bytes.Equal(index, after) {
		t.Errorf("index changed: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(f.local, "node_modules", "sentinel"))
	if err != nil || string(contents) != "precious ignored bytes\n" {
		t.Errorf("ignored bytes changed: %q, %v", contents, err)
	}
}
