package branchsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type syncFixture struct {
	t       *testing.T
	ctx     context.Context
	db      *db.DB
	repo    *db.Repo
	run     *db.Run
	service *Service
	local   string
	remote  string
	base    string
	old     string
	pushed  string
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/sync")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "feature")
	old := mustRun(t, local, "rev-parse", "HEAD")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "clone", local, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/sync")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "pipeline fix\n")
	mustRun(t, pipeline, "add", "fix.txt")
	mustRun(t, pipeline, "commit", "-m", "pipeline fix")
	pushed := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", remote, "HEAD:refs/heads/feature/sync")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/sync", old, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, pushed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{
		HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(remote), Ref: "refs/heads/feature/sync",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &syncFixture{t: t, ctx: ctx, db: database, repo: repo, run: run, service: &Service{DB: database, Repo: repo, WorkDir: local}, local: local, remote: remote, base: base, old: old, pushed: pushed}
}

type pipelineCommit struct {
	message string
	files   map[string]string
}

func newSplitLocalSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	mustWrite(t, filepath.Join(f.local, "second.txt"), "second\n")
	mustRun(t, f.local, "add", "second.txt")
	mustRun(t, f.local, "commit", "-m", "second local")
	f.old = mustRun(t, f.local, "rev-parse", "HEAD")
	return f
}

func rebuildPipelineHead(t *testing.T, f *syncFixture, commits []pipelineCommit) {
	t.Helper()
	root := filepath.Dir(f.local)
	pipeline := filepath.Join(root, "pipeline-rebuild")
	mustRun(t, root, "clone", f.local, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "-B", "feature/sync", f.base)
	for _, commit := range commits {
		for name, contents := range commit.files {
			mustWrite(t, filepath.Join(pipeline, name), contents)
			mustRun(t, pipeline, "add", name)
		}
		mustRun(t, pipeline, "commit", "-m", commit.message)
	}
	f.pushed = mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", f.remote, "HEAD:refs/heads/feature/sync")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.pushed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync",
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	f.run, err = f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTargetIdentityNeverPersistsOrDisplaysHTTPUserinfo(t *testing.T) {
	credentialed := "https://token:secret@example.com/owner/repo.git"
	plain := "https://example.com/owner/repo.git"
	if TargetFingerprint(credentialed) != TargetFingerprint(plain) {
		t.Fatal("credential stripping changed target identity")
	}
	if got := displayTarget(credentialed); got != plain || strings.Contains(got, "secret") || strings.Contains(got, "token") {
		t.Fatalf("display target = %q", got)
	}
}

func TestInspectCachedPrePushAndPushInProgressAreNonSyncable(t *testing.T) {
	f := newSyncFixture(t)
	active, err := f.db.InsertRun(f.repo.ID, "feature/sync", f.old, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(active.ID, f.pushed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.NextAction != nil || !strings.Contains(state.Error, "do not make local follow-up commits") {
		t.Fatalf("pre-push state = %#v", state)
	}
	if err := f.db.SetRunPushActive(active.ID, true); err != nil {
		t.Fatal(err)
	}
	state = f.service.InspectCached(f.ctx)
	if state.State != StatePushInProgress || state.NextAction != nil {
		t.Fatalf("push-in-progress state = %#v", state)
	}
	if err := f.db.SetRunPushActive(active.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(active.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	// Once the owning run is terminal the same state stops being a dead end:
	// it stays non-syncable but must offer the guarded custody recovery.
	state = f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("terminal unpublished pipeline head = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("terminal unpublished pipeline head next action = %#v", state.NextAction)
	}
}

func TestInspectCachedBehindPerformsNoFetchOrMutation(t *testing.T) {
	f := newSyncFixture(t)
	beforeFetchHead := readOptional(t, filepath.Join(f.local, ".git", "FETCH_HEAD"))
	state := f.service.InspectCached(f.ctx)
	if state.State != StateBehind || state.Relation != RelationBehind || state.Safety != "refresh_required" {
		t.Fatalf("state = %#v", state)
	}
	if state.Local.Head != f.old || state.Pipeline.PushedHead != f.pushed {
		t.Fatalf("full heads missing: %#v", state)
	}
	if got := readOptional(t, filepath.Join(f.local, ".git", "FETCH_HEAD")); got != beforeFetchHead {
		t.Fatal("cached inspection mutated FETCH_HEAD")
	}
	if _, err := gitpkg.Run(f.ctx, f.local, "show-ref", "--verify", "refs/no-mistakes/sync/"+f.run.ID); err == nil {
		t.Fatal("cached inspection created a private fetch ref")
	}
}

func TestApplyCleanStrictBehindFastForwardsExactBoundHead(t *testing.T) {
	f := newSyncFixture(t)
	state := f.service.Apply(f.ctx)
	if state.State != StateSynchronized || !state.Changed || state.Local.Head != f.pushed {
		t.Fatalf("state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD = %s, want %s", got, f.pushed)
	}
	if parents := strings.Fields(mustRun(t, f.local, "show", "-s", "--format=%P", "HEAD")); len(parents) != 1 || parents[0] != f.old {
		t.Fatalf("fast-forward created unexpected history: %v", parents)
	}
}

func TestApplyEquivalentButDivergedRebaseWithPipelineCommitsAnchorsAndAdvances(t *testing.T) {
	f := newSyncFixture(t)
	rebuildPipelineHead(t, f, []pipelineCommit{
		{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
		{message: "pipeline doc", files: map[string]string{"doc.txt": "pipeline doc\n"}},
	})

	state := f.service.Apply(f.ctx)
	if state.State != StateSynchronized || state.Relation != RelationEqual || state.Safety != "already_synchronized" || !state.Changed {
		t.Fatalf("state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD = %s, want %s", got, f.pushed)
	}
	if got := mustRun(t, f.local, "rev-parse", syncAnchorRef(f.run.ID)); got != f.old {
		t.Fatalf("pre-sync anchor = %s, want %s", got, f.old)
	}
	if got := readOptional(t, filepath.Join(f.local, "doc.txt")); got != "pipeline doc\n" {
		t.Fatalf("pipeline commit not applied: %q", got)
	}
}

func TestEquivalentButDivergedClassification(t *testing.T) {
	tests := []struct {
		name      string
		commits   []pipelineCommit
		wantState string
		wantSafe  string
	}{
		{
			name: "reordered commits",
			commits: []pipelineCommit{
				{message: "second rebased first", files: map[string]string{"second.txt": "second\n"}},
				{message: "first rebased second", files: map[string]string{"file.txt": "feature\n"}},
				{message: "pipeline extra", files: map[string]string{"doc.txt": "pipeline doc\n"}},
			},
			wantState: StateDiverged,
			wantSafe:  "safe_equivalent_advance",
		},
		{
			name: "squashed vs split",
			commits: []pipelineCommit{
				{message: "feature squashed", files: map[string]string{"file.txt": "feature\n", "second.txt": "second\n"}},
				{message: "pipeline extra", files: map[string]string{"doc.txt": "pipeline doc\n"}},
			},
			wantState: StateDiverged,
			wantSafe:  "safe_equivalent_advance",
		},
		{
			name: "squashed local work then same path pipeline fix",
			commits: []pipelineCommit{
				{message: "feature squashed", files: map[string]string{"file.txt": "feature\n", "second.txt": "second\n"}},
				{message: "pipeline same path fix", files: map[string]string{"file.txt": "feature\npipeline fix\n"}},
			},
			wantState: StateDiverged,
			wantSafe:  "safe_equivalent_advance",
		},
		{
			name: "pipeline extra before equivalent work",
			commits: []pipelineCommit{
				{message: "pipeline extra", files: map[string]string{"doc.txt": "pipeline doc\n"}},
				{message: "second rebased first", files: map[string]string{"second.txt": "second\n"}},
				{message: "first rebased second", files: map[string]string{"file.txt": "feature\n"}},
			},
			wantState: StateDiverged,
			wantSafe:  "safe_equivalent_advance",
		},
		{
			name: "conflicting rebase output",
			commits: []pipelineCommit{
				{message: "feature changed differently", files: map[string]string{"file.txt": "feature but different\n", "second.txt": "second\n"}},
				{message: "pipeline extra", files: map[string]string{"doc.txt": "pipeline doc\n"}},
			},
			wantState: StateDiverged,
			wantSafe:  "blocked_diverged",
		},
		{
			name: "same path pipeline overwrite after represented work",
			commits: []pipelineCommit{
				{message: "feature squashed", files: map[string]string{"file.txt": "feature\n", "second.txt": "second\n"}},
				{message: "pipeline overwrite", files: map[string]string{"file.txt": "pipeline overwrite\n"}},
			},
			wantState: StateDiverged,
			wantSafe:  "blocked_diverged",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSplitLocalSyncFixture(t)
			rebuildPipelineHead(t, f, tc.commits)

			state := f.service.Refresh(f.ctx)
			if state.State != tc.wantState || state.Relation != RelationDiverged || state.Safety != tc.wantSafe || state.Changed {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestEquivalentDivergenceRefusesRenameSourceOmission(t *testing.T) {
	f := newSyncFixture(t)
	mustRun(t, f.local, "config", "diff.renames", "true")
	mustRun(t, f.local, "mv", "file.txt", "renamed.txt")
	mustRun(t, f.local, "commit", "-m", "rename feature file")
	f.old = mustRun(t, f.local, "rev-parse", "HEAD")
	rebuildPipelineHead(t, f, []pipelineCommit{
		{message: "feature copied", files: map[string]string{"file.txt": "feature\n", "renamed.txt": "feature\n"}},
	})

	state := f.service.Refresh(f.ctx)
	if state.State != StateDiverged || state.Relation != RelationDiverged || state.Safety != "blocked_diverged" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
}

func TestEquivalentDivergenceRefusesDifferentBinaryContent(t *testing.T) {
	f := newSyncFixture(t)
	mustWrite(t, filepath.Join(f.local, "blob.bin"), string([]byte{0x00, 0x01, 0x02, 0x03}))
	mustRun(t, f.local, "add", "blob.bin")
	mustRun(t, f.local, "commit", "-m", "local binary")
	f.old = mustRun(t, f.local, "rev-parse", "HEAD")
	rebuildPipelineHead(t, f, []pipelineCommit{
		{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
		{message: "binary changed differently", files: map[string]string{"blob.bin": string([]byte{0x00, 0x01, 0x02, 0x04})}},
	})

	state := f.service.Refresh(f.ctx)
	if state.State != StateDiverged || state.Relation != RelationDiverged || state.Safety != "blocked_diverged" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
}

func TestEquivalentDivergenceRefusesWrongRepeatedLineOccurrence(t *testing.T) {
	f := newSyncFixture(t)
	mustWrite(t, filepath.Join(f.local, "repeated.txt"), "foo\nfoo\n")
	mustRun(t, f.local, "add", "repeated.txt")
	mustRun(t, f.local, "commit", "-m", "add repeated lines")
	f.base = mustRun(t, f.local, "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(f.local, "repeated.txt"), "bar\nfoo\n")
	mustRun(t, f.local, "commit", "-am", "change first occurrence")
	f.old = mustRun(t, f.local, "rev-parse", "HEAD")
	rebuildPipelineHead(t, f, []pipelineCommit{
		{message: "change second occurrence", files: map[string]string{"repeated.txt": "foo\nbar\n"}},
	})

	state := f.service.Refresh(f.ctx)
	if state.State != StateDiverged || state.Relation != RelationDiverged || state.Safety != "blocked_diverged" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
}

func TestEquivalentDivergenceAcceptsShiftedPreservedHunk(t *testing.T) {
	f := newSyncFixture(t)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "alpha\nfeature\nomega\n")
	mustRun(t, f.local, "commit", "-am", "expand feature file")
	f.old = mustRun(t, f.local, "rev-parse", "HEAD")
	rebuildPipelineHead(t, f, []pipelineCommit{
		{message: "feature squashed", files: map[string]string{"file.txt": "alpha\nfeature\nomega\n"}},
		{message: "pipeline inserts earlier line", files: map[string]string{"file.txt": "inserted\nalpha\nfeature\nomega\n"}},
	})

	state := f.service.Refresh(f.ctx)
	if state.State != StateDiverged || state.Relation != RelationDiverged || state.Safety != "safe_equivalent_advance" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
}

func TestEquivalentDivergenceRefusesAmbiguousRepeatedContext(t *testing.T) {
	f := newSyncFixture(t)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "ctx\nfeature\nend\n")
	mustRun(t, f.local, "commit", "-am", "contextual feature")
	f.old = mustRun(t, f.local, "rev-parse", "HEAD")
	rebuildPipelineHead(t, f, []pipelineCommit{
		{message: "duplicate contextual feature", files: map[string]string{"file.txt": "ctx\nfeature\nend\nctx\nfeature\nend\n"}},
	})

	state := f.service.Refresh(f.ctx)
	if state.State != StateDiverged || state.Relation != RelationDiverged || state.Safety != "blocked_diverged" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
}

func TestEquivalentDivergenceRefusesUnrepresentedEdgeDeletion(t *testing.T) {
	cases := map[string]struct {
		base  string
		local string
	}{
		"start": {base: "delete\nkeep\n", local: "keep\n"},
		"end":   {base: "keep\ndelete\n", local: "keep\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSyncFixture(t)
			mustWrite(t, filepath.Join(f.local, "edge.txt"), tc.base)
			mustRun(t, f.local, "add", "edge.txt")
			mustRun(t, f.local, "commit", "-m", "add edge file")
			f.base = mustRun(t, f.local, "rev-parse", "HEAD")
			mustWrite(t, filepath.Join(f.local, "edge.txt"), tc.local)
			mustRun(t, f.local, "commit", "-am", "delete edge line")
			f.old = mustRun(t, f.local, "rev-parse", "HEAD")
			run, err := f.db.InsertRun(f.repo.ID, "feature/sync", f.old, f.base)
			if err != nil {
				t.Fatal(err)
			}
			f.run = run
			rebuildPipelineHead(t, f, []pipelineCommit{
				{message: "pipeline extra", files: map[string]string{"extra.txt": "extra\n"}},
			})
			if err := f.db.UpdateRunStatus(f.run.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			refreshedRun, err := f.db.GetRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.run = refreshedRun

			state := f.service.Refresh(f.ctx)
			if state.State != StateDiverged || state.Relation != RelationDiverged || state.Safety != "blocked_diverged" || state.Changed {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestApplyEmptyLocalUniquenessStillUsesStrictBehindFastForward(t *testing.T) {
	f := newSyncFixture(t)
	state := f.service.Apply(f.ctx)
	if state.State != StateSynchronized || !state.Changed {
		t.Fatalf("state = %#v", state)
	}
	if _, err := gitpkg.Run(f.ctx, f.local, "rev-parse", "--verify", "--quiet", syncAnchorRef(f.run.ID)); err == nil {
		t.Fatal("strict behind fast-forward should not create an equivalent-divergence anchor")
	}
}

func TestApplyReportsHonestFinalStateWhenPostMergeHookMutatesWorktree(t *testing.T) {
	f := newSyncFixture(t)
	hooks := filepath.Join(f.local, ".git", "hooks")
	hook := filepath.Join(hooks, "post-merge")
	mustWrite(t, hook, "#!/bin/sh\nprintf hook > hook-output.txt\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	state := f.service.Apply(f.ctx)
	if !state.Changed || state.Local.Head != f.pushed || state.State != StateDirty || state.Local.Clean || !strings.HasPrefix(state.Safety, "blocked_post_apply_") {
		t.Fatalf("hook final state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("honest final HEAD = %s", got)
	}
}

func TestApplyAlreadyEqualIsExitZeroNoopState(t *testing.T) {
	f := newSyncFixture(t)
	if first := f.service.Apply(f.ctx); !first.Changed {
		t.Fatalf("first apply = %#v", first)
	}
	second := f.service.Apply(f.ctx)
	if second.State != StateSynchronized || second.Changed || second.Safety != "already_synchronized" {
		t.Fatalf("second apply = %#v", second)
	}
}

func TestDirtyClassesRefuseBeforeNetworkAndLeaveHeadIndexWorktree(t *testing.T) {
	cases := map[string]func(*syncFixture){
		"unstaged": func(f *syncFixture) { mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n") },
		"staged": func(f *syncFixture) {
			mustWrite(t, filepath.Join(f.local, "staged.txt"), "dirty\n")
			mustRun(t, f.local, "add", "staged.txt")
		},
		"untracked": func(f *syncFixture) { mustWrite(t, filepath.Join(f.local, "untracked.txt"), "dirty\n") },
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSyncFixture(t)
			prepare(f)
			beforeIndex, err := os.ReadFile(filepath.Join(f.local, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(f.remote, f.remote+".offline"); err != nil {
				t.Fatal(err)
			}
			state := f.service.Apply(f.ctx)
			if state.State != StateDirty || !strings.HasPrefix(state.Safety, "blocked_") {
				t.Fatalf("state = %#v", state)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
				t.Fatalf("HEAD changed to %s", got)
			}
			afterIndex, _ := os.ReadFile(filepath.Join(f.local, ".git", "index"))
			if string(afterIndex) != string(beforeIndex) {
				t.Fatal("index changed")
			}
		})
	}
}

func TestOperationInProgressClassesRefuse(t *testing.T) {
	for _, tc := range []struct{ marker, safety string }{
		{"MERGE_HEAD", "blocked_merge_in_progress"},
		{"CHERRY_PICK_HEAD", "blocked_cherry_pick_in_progress"},
		{"REVERT_HEAD", "blocked_revert_in_progress"},
		{"BISECT_LOG", "blocked_bisect_in_progress"},
		{"sequencer/todo", "blocked_sequencer_in_progress"},
		{"rebase-merge/head-name", "blocked_rebase_in_progress"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			f := newSyncFixture(t)
			gitPath := mustRun(t, f.local, "rev-parse", "--git-path", tc.marker)
			if !filepath.IsAbs(gitPath) {
				gitPath = filepath.Join(f.local, gitPath)
			}
			mustWrite(t, gitPath, "state\n")
			state := f.service.Refresh(f.ctx)
			if state.State != StateDirty || state.Safety != tc.safety {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestLocalAheadAndDivergedRefuse(t *testing.T) {
	t.Run("ahead", func(t *testing.T) {
		f := newSyncFixture(t)
		if state := f.service.Apply(f.ctx); !state.Changed {
			t.Fatal("setup sync failed")
		}
		mustWrite(t, filepath.Join(f.local, "followup.txt"), "followup\n")
		mustRun(t, f.local, "add", "followup.txt")
		mustRun(t, f.local, "commit", "-m", "followup")
		state := f.service.Apply(f.ctx)
		if state.State != StateLocalAhead || state.Relation != RelationAhead || state.Changed {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("diverged", func(t *testing.T) {
		f := newSyncFixture(t)
		mustWrite(t, filepath.Join(f.local, "followup.txt"), "diverged\n")
		mustRun(t, f.local, "add", "followup.txt")
		mustRun(t, f.local, "commit", "-m", "diverged followup")
		state := f.service.Apply(f.ctx)
		if state.State != StateDiverged || state.Relation != RelationDiverged || state.Changed {
			t.Fatalf("state = %#v", state)
		}
	})
}

func TestRemoteDeviationMissingAndOfflineFailClosed(t *testing.T) {
	t.Run("advanced", func(t *testing.T) {
		f := newSyncFixture(t)
		writer := cloneRemoteBranch(t, f.remote)
		mustWrite(t, filepath.Join(writer, "advanced.txt"), "advanced\n")
		mustRun(t, writer, "add", "advanced.txt")
		mustRun(t, writer, "commit", "-m", "out of band")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/sync")
		if state := f.service.Refresh(f.ctx); state.State != StateRemoteAdvanced {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("rewritten", func(t *testing.T) {
		f := newSyncFixture(t)
		writer := cloneRemoteBranch(t, f.remote)
		mustRun(t, writer, "checkout", "--orphan", "rewrite")
		mustRun(t, writer, "rm", "-rf", ".")
		mustWrite(t, filepath.Join(writer, "rewrite.txt"), "rewrite\n")
		mustRun(t, writer, "add", "rewrite.txt")
		mustRun(t, writer, "commit", "-m", "rewrite")
		mustRun(t, writer, "push", "--force", "origin", "HEAD:refs/heads/feature/sync")
		if state := f.service.Refresh(f.ctx); state.State != StateRemoteRewritten {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("missing open", func(t *testing.T) {
		f := newSyncFixture(t)
		if err := f.db.UpdateRunPRState(f.run.ID, "open"); err != nil {
			t.Fatal(err)
		}
		mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
		if state := f.service.Refresh(f.ctx); state.State != StateRemoteMissing {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("missing merged noop", func(t *testing.T) {
		f := newSyncFixture(t)
		if err := f.db.UpdateRunPRState(f.run.ID, "merged"); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(f.local, "retired-wip.txt"), "must remain untouched\n")
		mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
		if state := f.service.Apply(f.ctx); state.State != StateMergedRemoteRemoved || state.Changed {
			t.Fatalf("state = %#v", state)
		}
		if got := readOptional(t, filepath.Join(f.local, "retired-wip.txt")); got != "must remain untouched\n" {
			t.Fatalf("retired local work changed: %q", got)
		}
	})
	t.Run("offline", func(t *testing.T) {
		f := newSyncFixture(t)
		if err := os.Rename(f.remote, f.remote+".offline"); err != nil {
			t.Fatal(err)
		}
		if state := f.service.Refresh(f.ctx); state.State != StateOffline {
			t.Fatalf("state = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
			t.Fatal("HEAD changed")
		}
	})
}

func TestTargetChangeLegacyDetachedAndGenerationRaceRefuse(t *testing.T) {
	t.Run("target changed", func(t *testing.T) {
		f := newSyncFixture(t)
		other := filepath.Join(t.TempDir(), "other.git")
		mustRun(t, filepath.Dir(other), "init", "--bare", other)
		updated, err := f.db.UpdateRepoMetadata(f.repo.ID, other, "main")
		if err != nil {
			t.Fatal(err)
		}
		f.service.Repo = updated
		if state := f.service.Refresh(f.ctx); state.State != StateTargetChanged {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("legacy", func(t *testing.T) {
		f := newSyncFixture(t)
		legacy, err := f.db.InsertRun(f.repo.ID, "feature/sync", f.old, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatus(legacy.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		if state := f.service.Refresh(f.ctx); state.State != StateLegacyUnbound {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("detached", func(t *testing.T) {
		f := newSyncFixture(t)
		mustRun(t, f.local, "checkout", "--detach", f.old)
		if state := f.service.Apply(f.ctx); state.State != StateAmbiguousContext {
			t.Fatalf("state = %#v", state)
		}
	})
	t.Run("generation race", func(t *testing.T) {
		f := newSyncFixture(t)
		f.service.beforeApply = func() {
			if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync"}); err != nil {
				t.Fatal(err)
			}
		}
		state := f.service.Apply(f.ctx)
		if state.Changed || state.Safety != "blocked_generation_changed" {
			t.Fatalf("state = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
			t.Fatal("HEAD changed")
		}
	})
}

func TestLinkedWorktreeMutatesOnlyInvokingWorktree(t *testing.T) {
	f := newSyncFixture(t)
	mustRun(t, f.local, "checkout", "main")
	mainHead := mustRun(t, f.local, "rev-parse", "HEAD")
	linked := filepath.Join(t.TempDir(), "linked")
	mustRun(t, f.local, "worktree", "add", linked, "feature/sync")
	service := &Service{DB: f.db, Repo: f.repo, WorkDir: linked}
	state := service.Apply(f.ctx)
	if state.State != StateSynchronized || !state.Changed {
		t.Fatalf("linked apply = %#v", state)
	}
	if got := mustRun(t, linked, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("linked HEAD = %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != mainHead {
		t.Fatalf("main worktree HEAD changed from %s to %s", mainHead, got)
	}
}

func TestWrongBranchRefusesAsAmbiguousContext(t *testing.T) {
	f := newSyncFixture(t)
	mustRun(t, f.local, "checkout", "main")
	state := f.service.Apply(f.ctx)
	if state.State != StateAmbiguousContext || state.Safety != "blocked_wrong_branch" {
		t.Fatalf("wrong-branch state = %#v", state)
	}
}

func TestForkTargetNeverReadsParentOrigin(t *testing.T) {
	f := newSyncFixture(t)
	parent := filepath.Join(t.TempDir(), "parent.git")
	mustRun(t, filepath.Dir(parent), "init", "--bare", parent)
	updated, err := f.db.UpdateRepoMetadataWithFork(f.repo.ID, parent, f.remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	f.service.Repo = updated
	// Rebind as a fork because target identity is part of the proof.
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "fork", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
	state := f.service.Refresh(f.ctx)
	if state.State != StateBehind || state.Target.Kind != "fork" || state.Remote.ObservedHead != f.pushed {
		t.Fatalf("state = %#v", state)
	}
}

func cloneRemoteBranch(t *testing.T, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "writer")
	mustRun(t, filepath.Dir(dir), "clone", remote, dir)
	configureIdentity(t, dir)
	mustRun(t, dir, "checkout", "feature/sync")
	return dir
}

func configureIdentity(t *testing.T, dir string) {
	t.Helper()
	mustRun(t, dir, "config", "user.email", "test@example.com")
	mustRun(t, dir, "config", "user.name", "Test User")
}

func mustRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitpkg.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readOptional(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
