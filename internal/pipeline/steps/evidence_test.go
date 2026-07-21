package steps

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
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
	want := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
	if got != want {
		t.Errorf("evidence source dir = %q, want %q", got, want)
	}
}

func TestResolveTestEvidenceDir_SanitizesUnsafeBranch(t *testing.T) {
	location := resolveTestEvidenceLocation("/work/tree", "../../etc/pa ss~wd", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	wantSource := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
	wantDestination := filepath.Join("/work/tree", "evidence", "etc", "pa-ss-wd")
	if location.Dir != wantSource || location.RepoDir != wantDestination {
		t.Errorf("location = %#v, want source %q and destination %q", location, wantSource, wantDestination)
	}
}

func TestResolveTestEvidenceDir_EmptyBranchFallsBack(t *testing.T) {
	location := resolveTestEvidenceLocation("/work/tree", "///", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	want := filepath.Join("/work/tree", "evidence", "run-123")
	if location.RepoDir != want {
		t.Errorf("empty-branch publication dir = %q, want %q", location.RepoDir, want)
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
	if got[0].SHA256 != fmt.Sprintf("%x", sum[:]) || got[0].Size != int64(len(content)) {
		t.Fatalf("published artifact manifest = %#v, want full hash and size", got[0])
	}
	if filepath.IsAbs(got[0].Path) || strings.Contains(got[0].Path, workDir) {
		t.Fatalf("published artifact exposed a local path: %#v", got[0])
	}
	if _, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(wantRel))); err != nil {
		t.Fatalf("published image missing: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source image should remain intact after publication: %v", err)
	}
}

func TestPrepareTestEvidenceArtifacts_PreservesUnrelatedDestinationFiles(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(location.RepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(location.Dir, "truncated.png")
	unrelated := filepath.Join(location.RepoDir, "existing.png")
	if err := os.WriteFile(source, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got := prepareTestEvidenceArtifacts(workDir, location, []types.TestArtifact{{Kind: "screenshot", Label: "Broken", Path: source}})

	if got[0].Path != "" {
		t.Fatalf("malformed image was published: %#v", got[0])
	}
	for _, filename := range []string{source, unrelated} {
		if _, err := os.Stat(filename); err != nil {
			t.Fatalf("publication removed %q: %v", filename, err)
		}
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
	entries, err := os.ReadDir(location.RepoDir)
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
	if err := os.MkdirAll(location.RepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blockedTarget := filepath.Join(location.RepoDir, fmt.Sprintf("%x.png", sum[:16]))
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
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("publication failure removed source evidence: %v", err)
	}
}

func TestPrepareTestEvidenceArtifacts_FullyValidatesSupportedImages(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"truncated.png":       []byte("\x89PNG\r\n\x1a\n"),
		"truncated.jpg":       {0xff, 0xd8, 0xff},
		"truncated.gif":       []byte("GIF89a"),
		"unsupported.webp":    append([]byte("RIFF\x04\x00\x00\x00WEBP"), []byte("VP8 ")...),
		"too-many-pixels.png": oversizedDimensionPNG(50000, 50000),
	}
	var artifacts []types.TestArtifact
	for name, data := range files {
		filename := filepath.Join(location.Dir, name)
		if err := os.WriteFile(filename, data, 0o644); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, types.TestArtifact{Kind: "image", Label: name, Path: filename})
	}

	got := prepareTestEvidenceArtifacts(workDir, location, artifacts)

	for _, artifact := range got {
		if artifact.Path != "" || artifact.URL != "" || artifact.Content != unpublishedImageExplanation {
			t.Fatalf("invalid image did not degrade safely: %#v", artifact)
		}
	}
}

func TestPrepareTestEvidenceArtifacts_RejectsValidGIFWithoutDecodingFrames(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := gif.Encode(&encoded, image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black}), nil); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(location.Dir, "animation.gif")
	if err := os.WriteFile(source, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got := prepareTestEvidenceArtifacts(workDir, location, []types.TestArtifact{{Kind: "gif", Label: "Animation", Path: source}})

	if len(got) != 1 || got[0].Path != "" || got[0].SHA256 != "" || got[0].Size != 0 || got[0].Content != unpublishedImageExplanation {
		t.Fatalf("GIF evidence did not degrade safely: %#v", got)
	}
}

func TestPrepareTestEvidenceArtifacts_DeduplicatesBeforeApplyingCaps(t *testing.T) {
	workDir := t.TempDir()
	location := resolveTestEvidenceLocation(workDir, "feature", "run-123", config.Evidence{StoreInRepo: true, Dir: "evidence"})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var artifacts []types.TestArtifact
	for i := 0; i < maxPublishedImagesPerRun; i++ {
		data := coloredPNGBytes(uint8(i))
		filename := filepath.Join(location.Dir, fmt.Sprintf("%02d.png", i))
		if err := os.WriteFile(filename, data, 0o644); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, types.TestArtifact{Kind: "image", Label: fmt.Sprintf("image-%d", i), Path: filename})
	}
	duplicate := filepath.Join(location.Dir, "duplicate.png")
	if err := os.WriteFile(duplicate, coloredPNGBytes(0), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts = append(artifacts, types.TestArtifact{Kind: "image", Label: "duplicate", Path: duplicate})

	got := prepareTestEvidenceArtifacts(workDir, location, artifacts)

	if got[len(got)-1].Path == "" || got[len(got)-1].Path != got[0].Path {
		t.Fatalf("duplicate at publication cap was not reused: first=%#v duplicate=%#v", got[0], got[len(got)-1])
	}
}

func coloredPNGBytes(value uint8) []byte {
	var encoded bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: value, G: value ^ 0x5a, B: value ^ 0xa5, A: 0xff})
	if err := png.Encode(&encoded, img); err != nil {
		panic(err)
	}
	return encoded.Bytes()
}

func oversizedDimensionPNG(width, height uint32) []byte {
	data := append([]byte{}, []byte("\x89PNG\r\n\x1a\n")...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8
	ihdr[9] = 2
	chunkType := []byte("IHDR")
	chunk := make([]byte, 4+len(chunkType)+len(ihdr)+4)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(ihdr)))
	copy(chunk[4:8], chunkType)
	copy(chunk[8:], ihdr)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(append(chunkType, ihdr...)))
	return append(data, chunk...)
}
