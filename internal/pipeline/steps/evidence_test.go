package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestResolveTestEvidenceDir_DefaultUsesTempRunID(t *testing.T) {
	got := resolveTestEvidenceDir("/work/tree", "feature/foo", "run-123", config.Evidence{StoreInRepo: false, Dir: ".no-mistakes/evidence"})
	want := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
	if got != want {
		t.Errorf("default dir = %q, want %q", got, want)
	}
}

func TestResolveTestEvidenceDir_InRepoKeyedByBranch(t *testing.T) {
	got := resolveTestEvidenceDir("/work/tree", "feature/add-login", "run-123", config.Evidence{StoreInRepo: true, Dir: ".no-mistakes/evidence"})
	want := filepath.Join("/work/tree", ".no-mistakes", "evidence", "feature", "add-login")
	if got != want {
		t.Errorf("in-repo dir = %q, want %q", got, want)
	}
}

func TestResolveTestEvidenceDir_SanitizesUnsafeBranch(t *testing.T) {
	got := resolveTestEvidenceDir("/work/tree", "../../etc/pa ss~wd", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	// Traversal segments dropped, spaces/unsafe chars replaced with dashes,
	// result stays under <workdir>/evidence.
	want := filepath.Join("/work/tree", "evidence", "etc", "pa-ss-wd")
	if got != want {
		t.Errorf("sanitized dir = %q, want %q", got, want)
	}
}

func TestResolveTestEvidenceDir_EmptyBranchFallsBack(t *testing.T) {
	got := resolveTestEvidenceDir("/work/tree", "///", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	want := filepath.Join("/work/tree", "evidence", "run-123")
	if got != want {
		t.Errorf("empty-branch dir = %q, want %q", got, want)
	}
}

func TestResolveTestEvidenceDir_UnsafeConfigDirFallsBackToTemp(t *testing.T) {
	// An absolute or escaping configured dir must not let evidence land outside
	// the worktree; fall back to the temp directory instead.
	for _, dir := range []string{"/abs/evidence", "../escape", "a/../../b", ".git", ".git/hooks"} {
		got := resolveTestEvidenceDir("/work/tree", "feature/foo", "run-123", config.Evidence{StoreInRepo: true, Dir: dir})
		want := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
		if got != want {
			t.Errorf("dir %q: got %q, want temp fallback %q", dir, got, want)
		}
	}
}

func TestSafeRepoSubdirRejectsWindowsDriveAbsolutePath(t *testing.T) {
	if got, ok := safeRepoSubdir("C:/abs/evidence"); ok {
		t.Fatalf("safeRepoSubdir accepted Windows absolute path as %q", got)
	}
}

func TestSafeRepoSubdirRejectsWindowsRootedPath(t *testing.T) {
	if got, ok := safeRepoSubdir(`\abs\evidence`); ok {
		t.Fatalf("safeRepoSubdir accepted Windows rooted path as %q", got)
	}
}

func TestResolveTestEvidenceDir_SymlinkConfigDirFallsBackToTemp(t *testing.T) {
	workDir := t.TempDir()
	externalDir := t.TempDir()
	symlinkDir := filepath.Join(workDir, "evidence")
	if err := os.Symlink(externalDir, symlinkDir); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	got := resolveTestEvidenceDir(workDir, "feature/foo", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	want := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
	if got != want {
		t.Errorf("symlink evidence dir = %q, want temp fallback %q", got, want)
	}
}
