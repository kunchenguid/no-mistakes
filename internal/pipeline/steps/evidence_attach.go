package steps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxPRMediaAttachments = 50

// userAssetUploader uploads one local image or video to the forge's own
// attachment store and returns the URL the PR body embeds. github.Host
// (user-attachments) and gitlab.Host (project uploads) implement it; a forge
// whose host does not gets no media attachments. Tests inject a stub so unit
// tests never talk to a live forge.
type userAssetUploader interface {
	// ValidateUserAsset applies the forge's own client-side rules (supported
	// extensions, size limits). A file it refuses is never uploaded and keeps
	// today's rendering.
	ValidateUserAsset(path string) error
	UploadUserAsset(ctx context.Context, path string) (string, error)
}

func mediaAttachEnabled(sctx *pipeline.StepContext, provider scm.Provider) (bool, string) {
	if sctx == nil || sctx.Config == nil {
		return false, "no configuration available for media attachments"
	}
	if provider == scm.ProviderGitHub {
		host := ""
		if sctx.Repo != nil {
			host = resolvedHost(sctx, sctx.Repo.UpstreamURL)
		}
		if !github.SupportsUserAttachments(host) {
			return false, "media attachments are not supported on GitHub Enterprise Server"
		}
	}
	ev := sctx.Config.Test.Evidence
	if !ev.AttachMedia && !ev.StoreInRepo {
		return false, "test.evidence.attach_media and store_in_repo are both disabled"
	}
	return true, ""
}

// resolveMediaUploader returns the forge host's uploader, or nil and the reason
// this forge gets no media attachments.
func (s *PRStep) resolveMediaUploader(sctx *pipeline.StepContext, provider scm.Provider) (userAssetUploader, string) {
	if s != nil && s.mediaUploader != nil {
		return s.mediaUploader, ""
	}
	host, reason := buildHost(sctx, provider)
	if host == nil {
		if reason == "" {
			reason = "forge host is not available"
		}
		return nil, reason
	}
	u, ok := host.(userAssetUploader)
	if !ok {
		return nil, fmt.Sprintf("media attachments are not supported on %s", provider)
	}
	return u, ""
}

func collectPRTestingArtifacts(sctx *pipeline.StepContext, steps []*db.StepResult, rounds map[string][]*db.StepRound) []types.TestArtifact {
	if sctx == nil || sctx.Repo == nil || sctx.Run == nil {
		return nil
	}
	opts := testingSummaryOptionsForGitHub(sctx.Repo.UpstreamURL, sctx.Run.HeadSHA)
	opts.repoRoot = sctx.WorkDir
	opts.evidenceRoot = testEvidenceDir(sctx)
	for _, sr := range steps {
		if sr == nil || sr.StepName != types.StepTest {
			continue
		}
		return collectTestingArtifacts(sr, rounds[sr.ID], opts)
	}
	return nil
}

// attachRunEvidenceMedia uploads image/video evidence at PR render time and
// returns a map of local path -> forge attachment URL. Any failure for a file
// leaves that file out of the map so the PR body keeps today's rendering
// rather than a dead link.
func (s *PRStep) attachRunEvidenceMedia(sctx *pipeline.StepContext, provider scm.Provider, steps []*db.StepResult, rounds map[string][]*db.StepRound) map[string]string {
	artifacts := collectPRTestingArtifacts(sctx, steps, rounds)
	var candidates []types.TestArtifact
	var skipped []string
	seenPaths := make(map[string]bool)
	for _, artifact := range artifacts {
		if reason := skipMediaAttach(artifact); reason != "" {
			if isImageArtifact(artifact.Kind, artifact.Path) || isVideoArtifact(artifact.Kind, artifact.Path) {
				skipped = append(skipped, fmt.Sprintf("%s: %s", artifactLabel(artifact), reason))
			}
			continue
		}
		path := filepath.Clean(artifact.Path)
		if seenPaths[path] {
			continue
		}
		seenPaths[path] = true
		artifact.Path = path
		candidates = append(candidates, artifact)
	}
	if len(candidates) == 0 && len(skipped) == 0 {
		return nil
	}
	ok, reason := mediaAttachEnabled(sctx, provider)
	var uploader userAssetUploader
	if ok {
		uploader, reason = s.resolveMediaUploader(sctx, provider)
		ok = uploader != nil
	}
	if !ok {
		sctx.Log(fmt.Sprintf("skipping media attachments: %s", reason))
		return nil
	}
	for _, line := range skipped {
		sctx.Log("skipping media attachment for " + line)
	}
	var eligible []types.TestArtifact
	for _, artifact := range candidates {
		if err := uploader.ValidateUserAsset(artifact.Path); err != nil {
			sctx.Log(fmt.Sprintf("skipping media attachment for %s: %v", artifactLabel(artifact), err))
			continue
		}
		eligible = append(eligible, artifact)
	}
	if len(eligible) == 0 {
		return nil
	}
	if len(eligible) > maxPRMediaAttachments {
		for _, extra := range eligible[maxPRMediaAttachments:] {
			sctx.Log(fmt.Sprintf("skipping media attachment for %s: more than %d files in one PR", artifactLabel(extra), maxPRMediaAttachments))
		}
		eligible = eligible[:maxPRMediaAttachments]
	}

	attached := make(map[string]string, len(eligible))
	for _, artifact := range eligible {
		digest, err := mediaFileDigest(artifact.Path)
		if err != nil {
			sctx.Log(fmt.Sprintf("media attachment failed for %s, keeping today's rendering: fingerprint file: %v", artifactLabel(artifact), err))
			continue
		}
		cached, found, err := sctx.DB.GetRunMediaAttachment(sctx.Run.ID, artifact.Path, digest)
		if err != nil {
			sctx.Log(fmt.Sprintf("media attachment cache failed for %s, keeping today's rendering: %v", artifactLabel(artifact), err))
			continue
		}
		if found {
			attached[artifact.Path] = cached.URL
			sctx.Log(fmt.Sprintf("reused media attachment for %s", artifactLabel(artifact)))
			continue
		}
		url, err := uploader.UploadUserAsset(sctx.Ctx, artifact.Path)
		if err != nil {
			sctx.Log(fmt.Sprintf("media attachment failed for %s, keeping today's rendering: %v", artifactLabel(artifact), err))
			continue
		}
		if strings.TrimSpace(url) == "" {
			sctx.Log(fmt.Sprintf("media attachment returned no URL for %s, keeping today's rendering", artifactLabel(artifact)))
			continue
		}
		if err := sctx.DB.UpsertRunMediaAttachment(sctx.Run.ID, db.RunMediaAttachment{Path: artifact.Path, Digest: digest, URL: url}); err != nil {
			sctx.Log(fmt.Sprintf("warning: failed to cache media attachment for %s: %v", artifactLabel(artifact), err))
		}
		attached[artifact.Path] = url
		sctx.Log(fmt.Sprintf("uploaded media attachment for %s", artifactLabel(artifact)))
	}
	if len(attached) == 0 {
		return nil
	}
	return attached
}

func mediaFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func skipMediaAttach(artifact types.TestArtifact) string {
	if artifact.URL != "" {
		return "artifact already has a remote URL"
	}
	if artifact.Path == "" {
		return "no local path"
	}
	if !isImageArtifact(artifact.Kind, artifact.Path) && !isVideoArtifact(artifact.Kind, artifact.Path) {
		return "not an image or video"
	}
	if !filepath.IsAbs(artifact.Path) {
		return "path is not an absolute evidence file"
	}
	return ""
}

func artifactLabel(artifact types.TestArtifact) string {
	if label := strings.TrimSpace(artifact.Label); label != "" {
		return label
	}
	return filepath.Base(artifact.Path)
}
