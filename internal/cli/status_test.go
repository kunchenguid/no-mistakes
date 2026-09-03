package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStatusAlwaysRendersCachedLocalState(t *testing.T) {
	setupTestRepo(t)
	if _, err := executeCmd("init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	clean, err := executeCmd("status")
	if err != nil {
		t.Fatalf("clean status: %v", err)
	}
	for _, want := range []string{"repo:", "daemon:", "local state:  cached:", "(clean;", "no active run"} {
		if !strings.Contains(clean, want) {
			t.Fatalf("clean status missing %q:\n%s", want, clean)
		}
	}

	if err := os.WriteFile("uncommitted.txt", []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("make worktree dirty: %v", err)
	}

	dirty, err := executeCmd("status")
	if err != nil {
		t.Fatalf("dirty status: %v", err)
	}
	for _, want := range []string{"repo:", "daemon:", "local state:  cached:", "(dirty:", "no active run"} {
		if !strings.Contains(dirty, want) {
			t.Fatalf("dirty status missing %q:\n%s", want, dirty)
		}
	}
}

func TestStatusUsesCachedStateWithoutRemoteGitOrMutation(t *testing.T) {
	repoDir := setupTestRepo(t)
	if _, err := executeCmd("init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	// Keep the status command as the only process that can touch the local DB
	// while this test compares the read surface before and after it.
	if err := daemon.Stop(p); err != nil {
		t.Fatalf("stop daemon: %v", err)
	}

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	callsPath := filepath.Join(t.TempDir(), "remote-git-calls")
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$1" in
fetch|ls-remote) printf '%%s\\n' "$1" >> "$NM_STATUS_GIT_CALLS"; exit 97 ;;
esac
exec %q "$@"
`, gitBin)
	if runtime.GOOS == "windows" {
		wrapperPath += ".cmd"
		wrapper = fmt.Sprintf(`@echo off
if /I "%%~1"=="fetch" goto remote
if /I "%%~1"=="ls-remote" goto remote
%q %%*
exit /b %%ERRORLEVEL%%
:remote
echo %%~1>> "%%NM_STATUS_GIT_CALLS%%"
exit /b 97
`, gitBin)
	}
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write git wrapper: %v", err)
	}
	t.Setenv("NM_STATUS_GIT_CALLS", callsPath)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fetchHead := filepath.Join(repoDir, ".git", "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("cached-only-sentinel\\n"), 0o644); err != nil {
		t.Fatalf("seed FETCH_HEAD: %v", err)
	}
	before := snapshotStatusReadSurface(t, repoDir, p.DB())

	out, err := executeCmd("status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "local state:  cached:") {
		t.Fatalf("status did not render cached state:\n%s", out)
	}
	if calls, err := os.ReadFile(callsPath); err == nil && len(calls) > 0 {
		t.Fatalf("status must not call remote git operations; got %q", calls)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read remote git call log: %v", err)
	}

	after := snapshotStatusReadSurface(t, repoDir, p.DB())
	if !before.equal(after) {
		t.Fatalf("status mutated cached-only state:\n%s", before.diff(after))
	}
}

type statusReadSurface struct {
	fetchHead fileSnapshot
	index     fileSnapshot
	database  fileSnapshot
	dbWAL     fileSnapshot
	refs      []byte
	worktree  []byte
	objects   map[string][]byte
}

type fileSnapshot struct {
	present bool
	data    []byte
}

func snapshotStatusReadSurface(t *testing.T, repoDir, databasePath string) statusReadSurface {
	t.Helper()
	return statusReadSurface{
		fetchHead: snapshotFile(t, filepath.Join(repoDir, ".git", "FETCH_HEAD")),
		index:     snapshotFile(t, filepath.Join(repoDir, ".git", "index")),
		database:  snapshotFile(t, databasePath),
		dbWAL:     snapshotFile(t, databasePath+"-wal"),
		refs:      gitReadSurface(t, repoDir, "show-ref", "--head"),
		worktree:  gitReadSurface(t, repoDir, "status", "--porcelain=v1", "--untracked-files=all"),
		objects:   snapshotDirectoryFiles(t, filepath.Join(repoDir, ".git", "objects")),
	}
}

func snapshotDirectoryFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = contents
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot directory %s: %v", root, err)
	}
	return files
}

func snapshotFile(t *testing.T, path string) fileSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err == nil {
		return fileSnapshot{present: true, data: data}
	}
	if os.IsNotExist(err) {
		return fileSnapshot{}
	}
	t.Fatalf("read %s: %v", path, err)
	return fileSnapshot{}
}

func gitReadSurface(t *testing.T, repoDir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (s statusReadSurface) equal(other statusReadSurface) bool {
	return s.fetchHead.equal(other.fetchHead) &&
		s.index.equal(other.index) &&
		s.database.equal(other.database) &&
		s.dbWAL.equal(other.dbWAL) &&
		bytes.Equal(s.refs, other.refs) &&
		bytes.Equal(s.worktree, other.worktree) &&
		reflect.DeepEqual(s.objects, other.objects)
}

func (s statusReadSurface) diff(other statusReadSurface) string {
	var changed []string
	if !s.fetchHead.equal(other.fetchHead) {
		changed = append(changed, "FETCH_HEAD")
	}
	if !s.index.equal(other.index) {
		changed = append(changed, "index")
	}
	if !s.database.equal(other.database) {
		changed = append(changed, "gate database")
	}
	if !s.dbWAL.equal(other.dbWAL) {
		changed = append(changed, "gate database WAL")
	}
	if !bytes.Equal(s.refs, other.refs) {
		changed = append(changed, "refs")
	}
	if !bytes.Equal(s.worktree, other.worktree) {
		changed = append(changed, "worktree")
	}
	if !reflect.DeepEqual(s.objects, other.objects) {
		changed = append(changed, "Git objects")
	}
	return strings.Join(changed, ", ")
}

func (s fileSnapshot) equal(other fileSnapshot) bool {
	return s.present == other.present && bytes.Equal(s.data, other.data)
}

func TestCachedBranchSummary(t *testing.T) {
	tests := []struct {
		name  string
		state branchsync.State
		want  string
	}{
		{
			name: "clean branch",
			state: branchsync.State{
				State: branchsync.StateSynchronized,
				Local: branchsync.LocalState{Branch: "feature/state", Head: "0123456789abcdef", Clean: true},
			},
			want: "cached: feature/state 01234567 (clean; already synchronized with the pipeline-pushed head)",
		},
		{
			name: "dirty branch",
			state: branchsync.State{
				State: branchsync.StateDirty,
				Local: branchsync.LocalState{Branch: "feature/state", Head: "fedcba9876543210", Reason: "uncommitted changes"},
			},
			want: "cached: feature/state fedcba98 (dirty: uncommitted changes; dirty)",
		},
		{
			name: "cleanliness unavailable",
			state: branchsync.State{
				State: branchsync.StateDirty,
				Local: branchsync.LocalState{Branch: "feature/state", Head: "0123456789abcdef", Reason: "status_unavailable"},
			},
			want: "cached: feature/state 01234567 (cleanliness unavailable: run `git status`; local branch status is unavailable)",
		},
		{
			name: "unavailable cleanliness retains pipeline guidance",
			state: branchsync.State{
				State: branchsync.StatePipelineOwned,
				Local: branchsync.LocalState{Branch: "feature/state", Head: "0123456789abcdef", Reason: "status_unavailable"},
			},
			want: "cached: feature/state 01234567 (cleanliness unavailable: run `git status`; pipeline fix is not pushed yet; do not make local follow-up commits)",
		},
		{
			name:  "unavailable",
			state: branchsync.State{State: branchsync.StateAmbiguousContext},
			want:  "cached: unavailable (ambiguous context)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cachedBranchSummary(tt.state); got != tt.want {
				t.Fatalf("cachedBranchSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusFingerprintIncludesCachedSummary(t *testing.T) {
	run := &db.Run{ID: "run-1", Branch: "feature/test", Status: "running", HeadSHA: "head-one"}
	before := statusFingerprint("repo", "running", run, "cached: main 01234567 (clean; synchronized)")
	after := statusFingerprint("repo", "running", run, "cached: main 89abcdef (dirty; dirty)")
	if before == after {
		t.Fatal("changing displayed cached evidence must change the status fingerprint")
	}
}
