package branchsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublishedSubmittedHeadDivergenceTable(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name      string
		commits   []pipelineCommit
		adoptable bool
	}{
		{
			name: "rebase only",
			commits: []pipelineCommit{
				{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
			},
		},
		{
			name: "rebase and fix in a new file",
			commits: []pipelineCommit{
				{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
				{message: "fix in a new file", files: map[string]string{"doc.txt": "pipeline doc\n"}},
			},
		},
		{
			name:      "fix next to a submitted line",
			adoptable: true,
			commits: []pipelineCommit{
				{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
				{message: "fix next to submitted line", files: map[string]string{"file.txt": "feature\nextra\n"}},
			},
		},
		{
			name:      "rewrite a submitted line",
			adoptable: true,
			commits: []pipelineCommit{
				{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
				{message: "rewrite submitted line", files: map[string]string{"file.txt": "rewritten\n"}},
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newPublishedDivergenceFixture(t, row.commits)
			submitted := mustRun(t, f.local, "rev-parse", "HEAD")

			state := f.service.Refresh(f.ctx)
			if row.adoptable {
				if state.Safety != "blocked_diverged" || state.Changed || state.NextAction == nil || state.NextAction.Code != "adopt_pushed_head" || state.NextAction.Command != adoptPushedHeadCommand {
					t.Fatalf("refresh = %#v", state)
				}
				cached := f.service.InspectCached(f.ctx)
				if cached.NextAction == nil || cached.NextAction.Code != "adopt_pushed_head" || cached.NextAction.Command != adoptPushedHeadCommand {
					t.Fatalf("cached status = %#v", cached.NextAction)
				}
				plain := f.service.Apply(f.ctx)
				if plain.Safety != "blocked_diverged" || plain.Changed || mustRun(t, f.local, "rev-parse", "HEAD") != submitted || syncAnchorExists(t, f) {
					t.Fatalf("plain sync = %#v head %s", plain, mustRun(t, f.local, "rev-parse", "HEAD"))
				}
				adopted := f.service.AdoptPushedHead(f.ctx)
				if adopted.State != StateSynchronized || !adopted.Changed || adopted.Safety != "already_synchronized" {
					t.Fatalf("adopt = %#v", adopted)
				}
				if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
					t.Fatalf("HEAD = %s, want %s", got, f.pushed)
				}
				if got := mustRun(t, f.local, "rev-parse", syncAnchorRef(f.run.ID)); got != submitted {
					t.Fatalf("anchor = %s, want %s", got, submitted)
				}
				return
			}
			if state.Safety != SafetySafeEquivalentAdvance || state.Changed || state.NextAction == nil || state.NextAction.Code != "sync" {
				t.Fatalf("refresh = %#v", state)
			}
		})
	}
}

func TestAdoptPushedHeadRefusesEachBrokenCondition(t *testing.T) {
	t.Parallel()

	t.Run("local head is not the submitted head", func(t *testing.T) {
		t.Parallel()
		f := newPublishedDivergenceFixture(t, rewriteSubmittedLine())
		mustWrite(t, filepath.Join(f.local, "local.txt"), "local\n")
		mustRun(t, f.local, "add", "local.txt")
		mustRun(t, f.local, "commit", "-m", "local only")
		head := mustRun(t, f.local, "rev-parse", "HEAD")

		state := f.service.Refresh(f.ctx)
		if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" || state.NextAction.Command != "git checkout --detach "+f.pushed {
			t.Fatalf("refresh next action = %#v", state.NextAction)
		}
		refused := f.service.AdoptPushedHead(f.ctx)
		assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_not_submitted", head)
	})

	t.Run("dirty worktree", func(t *testing.T) {
		t.Parallel()
		f := newPublishedDivergenceFixture(t, rewriteSubmittedLine())
		head := mustRun(t, f.local, "rev-parse", "HEAD")
		mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n")

		refused := f.service.AdoptPushedHead(f.ctx)
		assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_dirty", head)
	})

	t.Run("live remote is not the pushed head", func(t *testing.T) {
		t.Parallel()
		f := newPublishedDivergenceFixture(t, rewriteSubmittedLine())
		if state := f.service.Refresh(f.ctx); state.NextAction == nil || state.NextAction.Code != "adopt_pushed_head" {
			t.Fatalf("refresh = %#v", state)
		}
		head := mustRun(t, f.local, "rev-parse", "HEAD")
		f.service.lsRemote = func(context.Context, string, string, string) (string, error) {
			return head, nil
		}

		refused := f.service.AdoptPushedHead(f.ctx)
		assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_remote_mismatch", head)
	})

	t.Run("pushed commit is missing locally", func(t *testing.T) {
		t.Parallel()
		f := newSyncFixture(t)
		head := mustRun(t, f.local, "rev-parse", "HEAD")
		missing := strings.Repeat("b", 40)
		if err := f.db.UpdateRunHeadSHA(f.run.ID, missing); err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
			HeadSHA: missing, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync",
		}); err != nil {
			t.Fatal(err)
		}

		refused := f.service.AdoptPushedHead(f.ctx)
		assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_missing", head)
	})

	t.Run("head changes before the move", func(t *testing.T) {
		t.Parallel()
		f := newPublishedDivergenceFixture(t, rewriteSubmittedLine())
		if state := f.service.Refresh(f.ctx); state.Safety != "blocked_diverged" {
			t.Fatalf("refresh = %#v", state)
		}
		f.service.beforeAdoptPushedHead = func() {
			mustWrite(t, filepath.Join(f.local, "race.txt"), "race\n")
			mustRun(t, f.local, "add", "race.txt")
			mustRun(t, f.local, "commit", "-m", "race")
		}

		refused := f.service.AdoptPushedHead(f.ctx)
		head := mustRun(t, f.local, "rev-parse", "HEAD")
		assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_changed", head)
		if head == f.pushed {
			t.Fatal("race commit was replaced by the pushed head")
		}
	})

	t.Run("remote changes before the move", func(t *testing.T) {
		t.Parallel()
		f := newPublishedDivergenceFixture(t, rewriteSubmittedLine())
		if state := f.service.Refresh(f.ctx); state.Safety != "blocked_diverged" {
			t.Fatalf("refresh = %#v", state)
		}
		head := mustRun(t, f.local, "rev-parse", "HEAD")
		reads := 0
		f.service.lsRemote = func(context.Context, string, string, string) (string, error) {
			reads++
			if reads == 1 {
				return f.pushed, nil
			}
			return head, nil
		}

		refused := f.service.AdoptPushedHead(f.ctx)
		assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_changed", head)
	})
}

func TestAdoptPushedHeadRefusesAnActiveRun(t *testing.T) {
	t.Parallel()

	f := newPublishedDivergenceFixture(t, rewriteSubmittedLine())
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	head := mustRun(t, f.local, "rev-parse", "HEAD")
	state := f.service.Refresh(f.ctx)
	if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("refresh next action = %#v", state.NextAction)
	}
	refused := f.service.AdoptPushedHead(f.ctx)
	assertAdoptionRefused(t, f, refused, "blocked_adopt_pushed_head_not_applicable", head)
}

func newPublishedDivergenceFixture(t *testing.T, commits []pipelineCommit) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	rebuildPipelineHead(t, f, commits)
	return f
}

func rewriteSubmittedLine() []pipelineCommit {
	return []pipelineCommit{
		{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
		{message: "rewrite submitted line", files: map[string]string{"file.txt": "rewritten\n"}},
	}
}

func assertAdoptionRefused(t *testing.T, f *syncFixture, state State, safety, head string) {
	t.Helper()
	if state.Changed || state.Safety != safety {
		t.Fatalf("adoption = safety %s changed %v, want %s\n%s", state.Safety, state.Changed, safety, state.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s", got, head)
	}
	if syncAnchorExists(t, f) {
		t.Fatalf("anchor exists at %s", mustRun(t, f.local, "rev-parse", syncAnchorRef(f.run.ID)))
	}
}

func syncAnchorExists(t *testing.T, f *syncFixture) bool {
	t.Helper()
	_, err := gitpkg.Run(f.ctx, f.local, "rev-parse", "--verify", "--quiet", syncAnchorRef(f.run.ID))
	return err == nil
}
