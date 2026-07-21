package steps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
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
	maxPublishedImagePixels      = 40 * 1024 * 1024
	maxEvidenceImageCandidates   = 64
	maxEvidenceCandidateBytes    = 4 * maxPublishedImagesTotalBytes
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
// Evidence is collected in a run-specific temporary directory. Validated
// images may also be published under a branch-named directory in the worktree.
func resolveTestEvidenceDir(workDir, branch, runID string, ev config.Evidence) string {
	location := resolveTestEvidenceLocation(workDir, branch, runID, ev)
	return location.Dir
}

type testEvidenceLocation struct {
	Dir         string
	RepoDir     string
	StoreInRepo bool
}

func resolveTestEvidenceLocation(workDir, branch, runID string, ev config.Evidence) testEvidenceLocation {
	sourceDir := testEvidenceDir(runID)
	if !ev.StoreInRepo {
		return testEvidenceLocation{Dir: sourceDir}
	}
	sub, ok := safeRepoSubdir(ev.Dir)
	if !ok {
		return testEvidenceLocation{Dir: sourceDir}
	}
	segments := evidenceBranchSlug(branch)
	if len(segments) == 0 {
		segments = []string{runID}
	}
	relParts := append([]string{sub}, segments...)
	rel := filepath.Join(relParts...)
	if repoPathHasSymlink(workDir, rel) {
		return testEvidenceLocation{Dir: sourceDir}
	}
	parts := append([]string{workDir}, relParts...)
	return testEvidenceLocation{
		Dir:         sourceDir,
		RepoDir:     filepath.Join(parts...),
		StoreInRepo: true,
	}
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
	for i := range prepared {
		prepared[i].Published = false
	}
	if !location.StoreInRepo {
		for i := range prepared {
			if isImageArtifact(prepared[i].Kind, prepared[i].Path) && sanitizeArtifactURL(prepared[i].URL) == "" {
				prepared[i] = unpublishedImageArtifact(prepared[i])
			}
		}
		return prepared
	}

	publishedBySource := make(map[string]types.TestArtifact)
	publishedByHash := make(map[string]types.TestArtifact)
	totalBytes := int64(0)
	candidateBytes := int64(0)
	candidateCount := 0
	publishedCount := 0

	for i := range prepared {
		artifact := prepared[i]
		if !isImageArtifact(artifact.Kind, artifact.Path) {
			continue
		}
		if publicURL := sanitizeArtifactURL(artifact.URL); publicURL != "" {
			prepared[i].URL = publicURL
			prepared[i].Path = ""
			prepared[i].SHA256 = ""
			prepared[i].Size = 0
			continue
		}

		candidateCount++
		if candidateCount > maxEvidenceImageCandidates {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}
		source := evidenceArtifactFilesystemPath(artifact.Path, workDir, location.Dir, location.RepoDir)
		if published, ok := publishedBySource[source]; source != "" && ok {
			prepared[i].Path = published.Path
			prepared[i].URL = ""
			prepared[i].SHA256 = published.SHA256
			prepared[i].Size = published.Size
			continue
		}
		data, ext, size, ok := readBoundedImageCandidate(source, &candidateBytes)
		if !ok {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}

		sum := sha256.Sum256(data)
		fullHash := fmt.Sprintf("%x", sum[:])
		hash := fullHash[:32]
		target := filepath.Join(location.RepoDir, hash+ext)
		rel, ok := artifactPathRelativeToRoot(target, workDir)
		if !ok {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}
		rel = filepath.ToSlash(rel)

		if existing, ok := publishedByHash[fullHash]; ok {
			prepared[i].Path = existing.Path
			prepared[i].URL = ""
			prepared[i].SHA256 = existing.SHA256
			prepared[i].Size = existing.Size
			publishedBySource[source] = prepared[i]
			continue
		} else {
			if publishedCount >= maxPublishedImagesPerRun || totalBytes+size > maxPublishedImagesTotalBytes {
				prepared[i] = unpublishedImageArtifact(artifact)
				continue
			}
			if _, ok := supportedImageExtension(ext, data); !ok {
				prepared[i] = unpublishedImageArtifact(artifact)
				continue
			}
			if err := os.MkdirAll(location.RepoDir, 0o755); err != nil {
				prepared[i] = unpublishedImageArtifact(artifact)
				continue
			}
			destinationRel, err := filepath.Rel(workDir, location.RepoDir)
			if !okRelativePath(destinationRel, err) || repoPathHasSymlink(workDir, destinationRel) {
				prepared[i] = unpublishedImageArtifact(artifact)
				continue
			}
			if err := writePublishedImage(target, data); err != nil {
				prepared[i] = unpublishedImageArtifact(artifact)
				continue
			}
			publishedCount++
			totalBytes += size
		}
		prepared[i].Path = rel
		prepared[i].URL = ""
		prepared[i].SHA256 = fullHash
		prepared[i].Size = size
		publishedBySource[source] = prepared[i]
		publishedByHash[fullHash] = prepared[i]
	}

	return prepared
}

func okRelativePath(rel string, err error) bool {
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func unpublishedImageArtifact(artifact types.TestArtifact) types.TestArtifact {
	artifact.Path = ""
	artifact.URL = ""
	artifact.Content = unpublishedImageExplanation
	artifact.SHA256 = ""
	artifact.Size = 0
	artifact.Published = false
	return artifact
}

func evidenceArtifactFilesystemPath(target, workDir string, evidenceRoots ...string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	candidates := []string{target}
	if !filepath.IsAbs(target) {
		candidates = []string{filepath.Join(workDir, filepath.FromSlash(target))}
		for _, root := range evidenceRoots {
			candidates = append(candidates, filepath.Join(root, filepath.FromSlash(target)))
		}
	}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		withinRoot := false
		for _, root := range evidenceRoots {
			if _, ok := artifactPathRelativeToRoot(candidate, root); ok {
				withinRoot = true
				break
			}
		}
		if !withinRoot {
			continue
		}
		if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func readPublishableImage(filename string) ([]byte, string, bool) {
	candidateBytes := int64(0)
	data, ext, _, ok := readBoundedImageCandidate(filename, &candidateBytes)
	if !ok {
		return nil, "", false
	}
	ext, ok = supportedImageExtension(ext, data)
	return data, ext, ok
}

func readBoundedImageCandidate(filename string, candidateBytes *int64) ([]byte, string, int64, bool) {
	if filename == "" {
		return nil, "", 0, false
	}
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxPublishedImageBytes {
		return nil, "", 0, false
	}
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png":
	case ".jpg", ".jpeg":
		ext = ".jpg"
	default:
		return nil, "", 0, false
	}
	if candidateBytes == nil || *candidateBytes+info.Size() > maxEvidenceCandidateBytes {
		return nil, "", 0, false
	}
	*candidateBytes += info.Size()
	file, err := os.Open(filename)
	if err != nil {
		return nil, "", 0, false
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() || !os.SameFile(info, openedInfo) {
		return nil, "", 0, false
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, "", 0, false
	}
	return data, ext, info.Size(), true
}

func supportedImageExtension(ext string, data []byte) (string, bool) {
	switch strings.ToLower(ext) {
	case ".png":
		return ".png", fullyDecodeImage(data, png.DecodeConfig, png.Decode)
	case ".jpg", ".jpeg":
		return ".jpg", fullyDecodeImage(data, jpeg.DecodeConfig, jpeg.Decode)
	default:
		return "", false
	}
}

type imageConfigDecoder func(io.Reader) (image.Config, error)
type imageDecoder func(io.Reader) (image.Image, error)

func fullyDecodeImage(data []byte, decodeConfig imageConfigDecoder, decode imageDecoder) bool {
	cfg, err := decodeConfig(bytes.NewReader(data))
	if err != nil || !validImageDimensions(cfg, 1) {
		return false
	}
	decoded, err := decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	bounds := decoded.Bounds()
	return bounds.Dx() == cfg.Width && bounds.Dy() == cfg.Height
}

func validImageDimensions(cfg image.Config, frames int) bool {
	if cfg.Width <= 0 || cfg.Height <= 0 || frames <= 0 {
		return false
	}
	pixels := uint64(cfg.Width) * uint64(cfg.Height) * uint64(frames)
	return pixels <= maxPublishedImagePixels
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
