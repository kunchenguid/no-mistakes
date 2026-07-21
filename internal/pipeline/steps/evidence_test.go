package steps

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
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

func TestPrepareTestEvidenceArtifacts_PublishesImageWithContentAddressedName(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(location.Dir, "checkout-local.png")
	content := testPNGBytes()
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}

	got := prepareTestEvidenceArtifacts(workDir, location, []types.TestArtifact{{
		Kind:  "screenshot",
		Label: "Checkout screenshot",
		Path:  source,
	}})

	sum := sha256.Sum256(content)
	wantRel := filepath.ToSlash(filepath.Join("evidence", "feature", fmt.Sprintf("%x.png", sum[:16])))
	if len(got) != 1 || got[0].Path != wantRel {
		t.Fatalf("published artifact = %#v, want path %q", got, wantRel)
	}
	if filepath.IsAbs(got[0].Path) || strings.Contains(got[0].Path, workDir) {
		t.Fatalf("published artifact exposed a local path: %#v", got[0])
	}
	if _, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(wantRel))); err != nil {
		t.Fatalf("published image missing: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source image should be replaced by deterministic publication, stat error = %v", err)
	}
}

func TestPrepareTestEvidenceArtifacts_RetryAndDuplicatesAreIdempotent(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(location.Dir, "checkout.png")
	if err := os.WriteFile(source, testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts := []types.TestArtifact{
		{Kind: "screenshot", Label: "First", Path: source},
		{Kind: "image", Label: "Duplicate", Path: source},
	}

	first := prepareTestEvidenceArtifacts(workDir, location, artifacts)
	second := prepareTestEvidenceArtifacts(workDir, location, first)

	if len(first) != 2 || len(second) != 2 || first[0].Path != first[1].Path || second[0].Path != first[0].Path || second[1].Path != first[0].Path {
		t.Fatalf("duplicate/retry paths changed: first=%#v second=%#v", first, second)
	}
	entries, err := os.ReadDir(location.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("published files = %d, want one content-addressed image", len(entries))
	}
}

func TestPrepareTestEvidenceArtifacts_DegradesInvalidImagesWithoutPaths(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(location.Dir, "missing.png")
	unsupported := filepath.Join(location.Dir, "capture.bmp")
	mismatch := filepath.Join(location.Dir, "not-really.png")
	oversized := filepath.Join(location.Dir, "oversized.png")
	if err := os.WriteFile(unsupported, []byte("BM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatch, []byte("not png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversized, append(testPNGBytes(), make([]byte, maxPublishedImageBytes)...), 0o644); err != nil {
		t.Fatal(err)
	}

	got := prepareTestEvidenceArtifacts(workDir, location, []types.TestArtifact{
		{Kind: "screenshot", Label: "Missing", Path: missing},
		{Kind: "image", Label: "Unsupported", Path: unsupported},
		{Kind: "screenshot", Label: "Mismatch", Path: mismatch},
		{Kind: "screenshot", Label: "Oversized", Path: oversized},
	})

	if len(got) != 4 {
		t.Fatalf("artifacts = %#v", got)
	}
	for _, artifact := range got {
		if artifact.Path != "" || artifact.URL != "" {
			t.Fatalf("invalid image retained a target: %#v", artifact)
		}
		if !strings.Contains(artifact.Content, "was not published") {
			t.Fatalf("invalid image lacks safe explanation: %#v", artifact)
		}
	}
}

func TestPrepareTestEvidenceArtifacts_OptOutNeverExposesLocalImagePath(t *testing.T) {
	workDir := t.TempDir()
	location := testEvidenceLocation{Dir: testEvidenceDir("run-123")}
	localPath := filepath.Join(location.Dir, "checkout.png")

	got := prepareTestEvidenceArtifacts(workDir, location, []types.TestArtifact{{
		Kind:  "screenshot",
		Label: "Checkout",
		Path:  localPath,
	}})

	if len(got) != 1 || got[0].Path != "" || got[0].URL != "" || !strings.Contains(got[0].Content, "was not published") {
		t.Fatalf("opt-out artifact did not degrade safely: %#v", got)
	}
	if strings.Contains(got[0].Content, localPath) || strings.Contains(got[0].Content, "/tmp/") {
		t.Fatalf("safe explanation exposed local path: %#v", got[0])
	}
}

func TestPrepareTestEvidenceArtifacts_PublishFailureDegradesSafely(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := testPNGBytes()
	source := filepath.Join(location.Dir, "checkout.png")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	blockedTarget := filepath.Join(location.Dir, fmt.Sprintf("%x.png", sum[:16]))
	if err := os.Mkdir(blockedTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	got := prepareTestEvidenceArtifacts(workDir, location, []types.TestArtifact{{
		Kind:  "screenshot",
		Label: "Checkout",
		Path:  source,
	}})

	if len(got) != 1 || got[0].Path != "" || got[0].URL != "" || !strings.Contains(got[0].Content, "was not published") {
		t.Fatalf("publication failure did not degrade safely: %#v", got)
	}
	if strings.Contains(got[0].Content, source) || strings.Contains(got[0].Content, workDir) {
		t.Fatalf("publication failure exposed a private path: %#v", got[0])
	}
}
