package steps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/draw"
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
	maxPublishedImageBytes                = 10 * 1024 * 1024
	maxPublishedImagesTotalBytes          = 25 * 1024 * 1024
	maxPublishedImagesPerRun              = 20
	maxPublishedImagePixels               = 8_000_000
	maxPublishedImageWorkingBytes         = 128 * 1024 * 1024
	maxPublishedImageWorkingBytesPerPixel = 12
	maxEvidenceImageCandidates            = 64
	maxEvidenceCandidateBytes             = 4 * maxPublishedImagesTotalBytes
	unpublishedImageExplanation           = "Image evidence was not published."
	disabledImagePublicationExplanation   = "Image evidence publication is disabled."
	fixedEvidenceRepoDir                  = ".no-mistakes/evidence"
	generatedEvidenceDir                  = ".generated"
	generatedEvidenceManifestName         = "manifest.json"
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
	Dir                 string
	RepoDir             string
	GeneratedRepoDir    string
	StoreInRepo         bool
	PublicationDisabled bool
}

func resolveTestEvidenceLocation(workDir, branch, runID string, ev config.Evidence) testEvidenceLocation {
	sourceDir := testEvidenceDir(runID)
	sub := filepath.FromSlash(fixedEvidenceRepoDir)
	generatedRepoDir := filepath.Join(workDir, sub, generatedEvidenceDir)
	if !ev.StoreInRepo {
		return testEvidenceLocation{
			Dir:                 sourceDir,
			GeneratedRepoDir:    generatedRepoDir,
			PublicationDisabled: true,
		}
	}
	segments := evidenceBranchSlug(branch)
	if len(segments) == 0 {
		segments = []string{runID}
	}
	relParts := append([]string{sub, generatedEvidenceDir}, segments...)
	rel := filepath.Join(relParts...)
	if repoPathHasSymlink(workDir, rel) {
		return testEvidenceLocation{Dir: sourceDir, GeneratedRepoDir: generatedRepoDir}
	}
	parts := append([]string{workDir}, relParts...)
	return testEvidenceLocation{
		Dir:              sourceDir,
		RepoDir:          filepath.Join(parts...),
		GeneratedRepoDir: generatedRepoDir,
		StoreInRepo:      true,
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
	return prepareTestEvidenceArtifactsWithCanonicalizer(workDir, location, artifacts, canonicalizePublishedImage)
}

func prepareTestEvidenceArtifactsWithCanonicalizer(workDir string, location testEvidenceLocation, artifacts []types.TestArtifact, canonicalize func(string, []byte) ([]byte, bool)) []types.TestArtifact {
	prepared := append([]types.TestArtifact(nil), artifacts...)
	for i := range prepared {
		prepared[i].Published = false
	}
	if !location.StoreInRepo {
		for i := range prepared {
			if isImageArtifact(prepared[i].Kind, prepared[i].Path) && sanitizeArtifactURL(prepared[i].URL) == "" {
				explanation := unpublishedImageExplanation
				if location.PublicationDisabled {
					explanation = disabledImagePublicationExplanation
				}
				prepared[i] = unpublishedImageArtifactWithExplanation(prepared[i], explanation)
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
	publicationExhausted := false

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
		if publicationExhausted || publishedCount >= maxPublishedImagesPerRun || totalBytes >= maxPublishedImagesTotalBytes {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}
		data, ext, _, ok := readBoundedImageCandidate(source, &candidateBytes)
		if !ok {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}
		data, ok = canonicalize(ext, data)
		if !ok {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}
		size := int64(len(data))
		if size <= 0 || size > maxPublishedImageBytes {
			prepared[i] = unpublishedImageArtifact(artifact)
			continue
		}

		sum := sha256.Sum256(data)
		fullHash := fmt.Sprintf("%x", sum[:])
		hash := fullHash[:32]
		target := filepath.Join(location.RepoDir, hash+".png")
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
			if totalBytes+size > maxPublishedImagesTotalBytes {
				prepared[i] = unpublishedImageArtifact(artifact)
				publicationExhausted = true
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
	return unpublishedImageArtifactWithExplanation(artifact, unpublishedImageExplanation)
}

func unpublishedImageArtifactWithExplanation(artifact types.TestArtifact, explanation string) types.TestArtifact {
	artifact.Path = ""
	artifact.URL = ""
	artifact.Content = explanation
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
	data, ok = canonicalizePublishedImage(ext, data)
	return data, ".png", ok
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

func canonicalizePublishedImage(ext string, data []byte) ([]byte, bool) {
	var decoded image.Image
	var cfg image.Config
	var err error
	switch strings.ToLower(ext) {
	case ".png":
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
		if err == nil && validImageDimensions(cfg, 1) {
			decoded, err = png.Decode(bytes.NewReader(data))
		}
	case ".jpg", ".jpeg":
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
		if err == nil && validImageDimensions(cfg, 1) {
			decoded, err = jpeg.Decode(bytes.NewReader(data))
		}
	default:
		return nil, false
	}
	if err != nil || decoded == nil {
		return nil, false
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != cfg.Width || bounds.Dy() != cfg.Height {
		return nil, false
	}
	canonical := image.NewNRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(canonical, canonical.Bounds(), decoded, bounds.Min, draw.Src)
	decoded = nil
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canonical); err != nil || encoded.Len() <= 0 || encoded.Len() > maxPublishedImageBytes {
		return nil, false
	}
	return encoded.Bytes(), true
}

func supportedImageExtension(ext string, data []byte) (string, bool) {
	if strings.ToLower(ext) != ".png" {
		return "", false
	}
	canonical, ok := canonicalizePublishedImage(ext, data)
	return ".png", ok && bytes.Equal(canonical, data)
}

func validImageDimensions(cfg image.Config, frames int) bool {
	if cfg.Width <= 0 || cfg.Height <= 0 || frames <= 0 {
		return false
	}
	width := uint64(cfg.Width)
	height := uint64(cfg.Height)
	frameCount := uint64(frames)
	if width > ^uint64(0)/height || width*height > ^uint64(0)/frameCount {
		return false
	}
	pixels := width * height * frameCount
	return pixels <= maxPublishedImagePixels &&
		pixels <= maxPublishedImageWorkingBytes/maxPublishedImageWorkingBytesPerPixel
}

func writePublishedImage(target string, data []byte) error {
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("published image target is not a regular file")
		}
		matches, err := publishedImageFileMatches(target, data)
		if err != nil {
			return err
		}
		if matches {
			return nil
		}
		return fmt.Errorf("published image target already exists with different content")
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(target)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func publishedImageFileMatches(target string, expected []byte) (bool, error) {
	info, err := os.Lstat(target)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(len(expected)) {
		return false, nil
	}
	file, err := os.Open(target)
	if err != nil {
		return false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() != int64(len(expected)) || !os.SameFile(info, openedInfo) {
		return false, nil
	}
	expectedHash := sha256.Sum256(expected)
	actualHash := sha256.New()
	n, err := io.Copy(actualHash, io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return false, err
	}
	return n == int64(len(expected)) && bytes.Equal(actualHash.Sum(nil), expectedHash[:]), nil
}
