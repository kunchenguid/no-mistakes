//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMissingGitMetadataRetentionStillReapsEscapedProcesses(t *testing.T) {
	originalAge := orphanProcessMinAge
	orphanProcessMinAge = 0
	t.Cleanup(func() { orphanProcessMinAge = originalAge })
	for _, route := range []string{"immediate", "startup"} {
		t.Run(route, func(t *testing.T) {
			p, database, repo, run, dir := unfinishedCleanupFixture(t, "clean", "default")
			refusedDirtyWorktree(t, dir, "dirty")
			before := unfinishedWorktreeSnapshot(t, dir)
			pointer := filepath.Join(dir, ".git")
			original, err := os.ReadFile(pointer)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(pointer); err != nil {
				t.Fatal(err)
			}
			pid := startOrphanInWorktree(t, dir)
			if route == "immediate" {
				NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
			} else {
				sweepOrphanRunProcesses(database, p, nil, retainedDefaultTreeRunIDs(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour)))
			}
			if !pidGoneWithin(pid, 10*time.Second) {
				t.Error("escaped process survived missing-metadata retention")
			}
			if err := os.WriteFile(pointer, original, 0600); err != nil {
				t.Fatal(err)
			}
			if after := unfinishedWorktreeSnapshot(t, dir); !reflect.DeepEqual(before, after) {
				t.Error("process sweep changed retained bytes/index/HEAD")
			}
		})
	}
}

func TestUnfinishedWorktreeRetentionStillReapsEscapedProcesses(t *testing.T) {
	original := orphanProcessMinAge
	orphanProcessMinAge = 0
	t.Cleanup(func() { orphanProcessMinAge = original })
	for _, route := range []string{"immediate", "startup"} {
		t.Run(route, func(t *testing.T) {
			p, database, repo, run, dir := unfinishedCleanupFixture(t, "rebase", "default")
			before := unfinishedWorktreeSnapshot(t, dir)
			pid := startOrphanInWorktree(t, dir)
			if route == "immediate" {
				NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
			} else {
				sweepOrphanRunProcesses(database, p, nil, retainedDefaultTreeRunIDs(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour)))
			}
			if !pidGoneWithin(pid, 10*time.Second) {
				t.Error("escaped process survived unfinished-work retention")
			}
			after := unfinishedWorktreeSnapshot(t, dir)
			if !reflect.DeepEqual(before, after) {
				t.Error("process sweep changed retained HEAD/index")
			}
		})
	}
}

func TestCIRefusedDirtyWorkStillReapsEscapedProcesses(t *testing.T) {
	original := orphanProcessMinAge
	orphanProcessMinAge = 0
	t.Cleanup(func() { orphanProcessMinAge = original })
	for _, route := range []string{"immediate", "startup"} {
		t.Run(route, func(t *testing.T) {
			p, database, repo, run, dir := unfinishedCleanupFixture(t, "clean", "default")
			refusedDirtyWorktree(t, dir, "dirty")
			sr, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(sr.ID, ciRefusedFindings); err != nil {
				t.Fatal(err)
			}
			before := unfinishedWorktreeSnapshot(t, dir)
			pid := startOrphanInWorktree(t, dir)
			if route == "immediate" {
				NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
			} else {
				sweepOrphanRunProcesses(database, p, nil, retainedDefaultTreeRunIDs(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour)))
			}
			if !pidGoneWithin(pid, 10*time.Second) {
				t.Error("escaped process survived refused dirty retention")
			}
			if after := unfinishedWorktreeSnapshot(t, dir); !reflect.DeepEqual(before, after) {
				t.Error("process sweep changed refused bytes/index")
			}
		})
	}
}
