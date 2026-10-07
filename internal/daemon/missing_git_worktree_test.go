package daemon

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestMissingGitMetadataWorkSurvivesCleanup(t *testing.T) {
	for _, route := range []string{"immediate", "startup", "age", "count", "orphan"} {
		for _, metadata := range []string{"missing", "broken", "unreadable"} {
			for _, mutation := range []string{"dirty", "commit"} {
				t.Run(route+"/"+metadata+"/"+mutation, func(t *testing.T) {
					placement := "default"
					if route == "startup" {
						placement = "recorded"
					}
					p, database, repo, run, dir := unfinishedCleanupFixture(t, "clean", placement)
					refusedDirtyWorktree(t, dir, mutation)
					before := unfinishedWorktreeSnapshot(t, dir)
					pointer := filepath.Join(dir, ".git")
					original, err := os.ReadFile(pointer)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(pointer); err != nil {
						t.Fatal(err)
					}
					if metadata == "broken" {
						if err := os.WriteFile(pointer, []byte("gitdir: missing-metadata\n"), 0600); err != nil {
							t.Fatal(err)
						}
					} else if metadata == "unreadable" {
						// A directory in place of the pointer fails Git inspection
						// deterministically, including when tests run as root.
						if err := os.Mkdir(pointer, 0700); err != nil {
							t.Fatal(err)
						}
					}
					var owner *db.Run = run
					if route == "orphan" {
						owner = nil
					}
					if reason := worktreeCleanupReason(database, owner, dir); reason == "" {
						t.Errorf("cleanup eligibility allowed retained bytes with %s metadata", metadata)
					}
					switch route {
					case "immediate":
						NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
					case "startup":
						cleanupOrphanWorktrees(database, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: dir}})
					case "orphan":
						orphan := filepath.Join(p.WorktreesDir(), repo.ID, "no-run-record")
						if err := os.Rename(dir, orphan); err != nil {
							t.Fatal(err)
						}
						dir = orphan
						pointer = filepath.Join(dir, ".git")
						reapWorktrees(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
					case "age":
						reapWorktrees(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
					case "count":
						if err := os.MkdirAll(p.WorktreeDir(repo.ID, "newer-empty"), 0700); err != nil {
							t.Fatal(err)
						}
						old := time.Now().Add(-time.Hour)
						if err := os.Chtimes(dir, old, old); err != nil {
							t.Fatal(err)
						}
						reapWorktrees(database, p, worktreeReapPolicy{MaxRuns: 1}, time.Now())
					}
					if _, err := os.Stat(dir); err != nil {
						t.Fatalf("cleanup deleted retained work: %v", err)
					}
					if metadata != "missing" {
						if err := os.Remove(pointer); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(pointer, original, 0600); err != nil {
						t.Fatal(err)
					}
					if after := unfinishedWorktreeSnapshot(t, dir); !reflect.DeepEqual(before, after) {
						t.Fatal("cleanup changed retained HEAD/index/bytes")
					}
					stored, err := database.GetRun(run.ID)
					if err != nil || !reflect.DeepEqual(stored, run) {
						t.Fatalf("cleanup changed run custody: %+v %v", stored, err)
					}
					t.Logf("retained metadata=%s mutation=%s HEAD=%s raw-index-sha256=%x", metadata, mutation, before["head"], sha256.Sum256([]byte(before["index/."])))
				})
			}
		}
	}
}

func TestMissingGitMetadataGoneAndEmptyControls(t *testing.T) {
	f := newWorktreeReapFixture(t)
	if reason := worktreeCleanupReason(f.db, nil, filepath.Join(f.p.WorktreesDir(), "actually-gone")); reason != "" {
		t.Fatalf("gone directory retained: %s", reason)
	}
	id := f.seed("empty", "completed", time.Hour)
	dir := f.p.WorktreeDir(f.repo.ID, id)
	if reason := worktreeCleanupReason(f.db, nil, dir); reason != "" {
		t.Fatalf("empty directory retained: %s", reason)
	}
	reapWorktrees(f.db, f.p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
	if f.exists(id) {
		t.Fatal("empty directory survived ordinary cleanup")
	}
}
