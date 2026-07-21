package steps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	maxPublishedImageBytes       = 10 * 1024 * 1024
	maxPublishedImagesTotalBytes = 25 * 1024 * 1024
	maxPublishedImagesPerRun     = 20
	unpublishedImageExplanation  = "Image evidence was not published."
)

func testEvidenceRoot() string {
	return filepath.Join(os.TempDir(), "no-mistakes-evidence")
}

func testEvidenceDir(runID string) string {
	return filepath.Join(testEvidenceRoot(), runID)
}

// resolveTestEvidenceDir picks where the test step writes evidence artifacts.
//
// Published evidence lands under a readable, branch-named directory inside the
// worktree so it is committed, pushed, and rendered directly on the PR. When
// repository storage is disabled, or the configured directory is unsafe,
// evidence stays in a run-specific temporary directory and generated PR
// content omits its local path.
func resolveTestEvidenceDir(workDir, branch, runID string, ev config.Evidence) string {
	location := resolveTestEvidenceLocation(workDir, branch, runID, ev)
	return location.Dir
}

type testEvidenceLocation struct {
	Dir         string
	StoreInRepo bool
}

func resolveTestEvidenceLocation(workDir, branch, runID string, ev config.Evidence) testEvidenceLocation {
	if !ev.StoreInRepo {
		return testEvidenceLocation{Dir: testEvidenceDir(runID)}
	}
	sub, ok := safeRepoSubdir(ev.Dir)
	if !ok {
		return testEvidenceLocation{Dir: testEvidenceDir(runID)}
	}
	segments := evidenceBranchSlug(branch)
	if len(segments) == 0 {
		segments = []string{runID}
	}
	relParts := append([]string{sub}, segments...)
	rel := filepath.Join(relParts...)
	if repoPathHasSymlink(workDir, rel) {
		return testEvidenceLocation{Dir: testEvidenceDir(runID)}
	}
	parts := append([]string{workDir}, relParts...)
	return testEvidenceLocation{Dir: filepath.Join(parts...), StoreInRepo: true}
}

func repoPathHasSymlink(workDir, rel string) bool {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return true
	}
	current := workDir
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return false
		}
		if err != nil {
			return true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// safeRepoSubdir validates a configured evidence directory as a relative path
// that stays inside the repo worktree. It returns the cleaned, OS-native path
// and false when the directory is empty, absolute, or escapes the worktree.
func safeRepoSubdir(dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" || filepath.IsAbs(dir) || hasPathRootPrefix(dir) || hasWindowsDrivePrefix(dir) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(dir))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	first, _, _ := strings.Cut(clean, string(filepath.Separator))
	if strings.EqualFold(first, ".git") {
		return "", false
	}
	return clean, true
}

func hasPathRootPrefix(path string) bool {
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`)
}

func hasWindowsDrivePrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	c := path[0]
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// evidenceBranchSlug turns a branch name into readable, filesystem-safe path
// segments. Branch separators are preserved as nested directories; unsafe
// characters are replaced with dashes and traversal segments are dropped.
func evidenceBranchSlug(branch string) []string {
	var segments []string
	for _, raw := range strings.Split(branch, "/") {
		seg := sanitizeEvidenceSegment(raw)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		segments = append(segments, seg)
	}
	return segments
}

// sanitizeEvidenceSegment keeps alphanumerics, dash, underscore, and dot,
// replacing every other rune with a dash, then collapses dash runs and trims
// leading/trailing dashes.
func sanitizeEvidenceSegment(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// prepareTestEvidenceArtifacts validates image evidence and gives each
// publishable image a content-addressed repository path. Repeated evidence and
// retries therefore reuse the same file. Images that cannot be published lose
// their path and degrade to a safe explanation in generated summaries.
func prepareTestEvidenceArtifacts(workDir string, location testEvidenceLocation, artifacts []types.TestArtifact) []types.TestArtifact {
	prepared := append([]types.TestArtifact(nil), artifacts...)
	if !location.StoreInRepo {
		for i := range prepared {
			if isImageArtifact(prepared[i].Kind, prepared[i].Path) && sanitizeArtifactURL(prepared[i].URL) == "" {
				prepared[i] = unpublishedImageArtifact(prepared[i])
			}
		}
		return prepared
	}

	publishedBySource := make(map[string]string)
	publishedByHash := make(map[string]string)
	keep := make(map[string]bool)
	totalBytes := int64(0)
	publishedCount := 0

	for i := range prepared {
		artifact := prepared[i]
		if !isImageArtifact(artifact.Kind, artifact.Path) {
			continue
		}
		if publicURL := sanitizeArtifactURL(artifact.URL); publicURL != "" {
			prepared[i].URL = publicURL
			prepared[i].Path = ""
			continue
		}

		source := evidenceArtifactFilesystemPath(artifact.Path, workDir, location.Dir)
		if rel, ok := publishedBySource[source]; source != "" && ok {
			prepared[i].Path = rel
			prepared[i].URL = ""
			keep[filepath.Join(workDir, filepath.FromSlash(rel))] = true
			continue
		}
		data, ext, ok := readPublishableImage(source)
		if !ok || publishedCount >= maxPublishedImagesPerRun || totalBytes+int64(len(data)) > maxPublishedImagesTotalBytes {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}

		sum := sha256.Sum256(data)
		hash := fmt.Sprintf("%x", sum[:16])
		target := filepath.Join(location.Dir, hash+ext)
		rel, ok := artifactPathRelativeToRoot(target, workDir)
		if !ok {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}
		rel = filepath.ToSlash(rel)

		if existing, ok := publishedByHash[hash]; ok {
			target = existing
		} else {
			if err := writePublishedImage(target, data); err != nil {
				prepared[i] = unpublishedImageArtifact(artifact)
				continue
			}
			publishedByHash[hash] = target
			publishedCount++
			totalBytes += int64(len(data))
		}
		publishedBySource[source] = rel
		keep[target] = true
		prepared[i].Path = rel
		prepared[i].URL = ""
	}

	pruneUnreferencedEvidenceImages(location.Dir, keep)
	return prepared
}

func unpublishedImageArtifact(artifact types.TestArtifact) types.TestArtifact {
	artifact.Path = ""
	artifact.URL = ""
	artifact.Content = unpublishedImageExplanation
	return artifact
}

func evidenceArtifactFilesystemPath(target, workDir, evidenceDir string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	candidates := []string{target}
	if !filepath.IsAbs(target) {
		candidates = []string{
			filepath.Join(workDir, filepath.FromSlash(target)),
			filepath.Join(evidenceDir, filepath.FromSlash(target)),
		}
	}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, ok := artifactPathRelativeToRoot(candidate, evidenceDir); !ok {
			continue
		}
		if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func readPublishableImage(filename string) ([]byte, string, bool) {
	if filename == "" {
		return nil, "", false
	}
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxPublishedImageBytes {
		return nil, "", false
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", false
	}
	ext, ok := supportedImageExtension(filepath.Ext(filename), data)
	return data, ext, ok
}

func supportedImageExtension(ext string, data []byte) (string, bool) {
	switch strings.ToLower(ext) {
	case ".png":
		return ".png", bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case ".jpg", ".jpeg":
		return ".jpg", len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
	case ".gif":
		return ".gif", bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
	case ".webp":
		return ".webp", len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	default:
		return "", false
	}
}

func writePublishedImage(target string, data []byte) error {
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("published image target is not a regular file")
		}
		if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".image-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

func pruneUnreferencedEvidenceImages(dir string, keep map[string]bool) {
	_ = filepath.WalkDir(dir, func(filename string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || keep[filename] {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(filename))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
			_ = os.Remove(filename)
		}
		return nil
	})
}
