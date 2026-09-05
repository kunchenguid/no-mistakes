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

// userAssetUploader uploads one local image or video to GitHub user-attachments.
// github.Host implements it. Tests inject a stub so unit tests never talk to
// live GitHub.
type userAssetUploader interface {
	UploadUserAsset(ctx context.Context, path string) (string, error)
}

func mediaAttachEnabled(sctx *pipeline.StepContext, provider scm.Provider) (bool, string) {
	if provider != scm.ProviderGitHub {
		return false, "media attachments are GitHub-only"
	}
	if sctx == nil || sctx.Config == nil {
		return false, "no configuration available for media attachments"
	}
	host := ""
	if sctx.Repo != nil {
		host = resolvedHost(sctx, sctx.Repo.UpstreamURL)
	}
	if !github.SupportsUserAttachments(host) {
		return false, "media attachments are not supported on GitHub Enterprise Server"
	}
	ev := sctx.Config.Test.Evidence
	if !ev.AttachMedia && !ev.StoreInRepo {
		return false, "test.evidence.attach_media and store_in_repo are both disabled"
	}
	return true, ""
}

func (s *PRStep) resolveMediaUploader(sctx *pipeline.StepContext, provider scm.Provider) userAssetUploader {
	if s != nil && s.mediaUploader != nil {
		return s.mediaUploader
	}
	host, _ := buildHost(sctx, provider)
	u, _ := host.(userAssetUploader)
	return u
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
// returns a map of local path -> user-attachments URL. Any failure for a file
// leaves that file out of the map so the PR body keeps today's rendering
// rather than a dead link.
func (s *PRStep) attachRunEvidenceMedia(sctx *pipeline.StepContext, provider scm.Provider, steps []*db.StepResult, rounds map[string][]*db.StepRound) map[string]string {
	artifacts := collectPRTestingArtifacts(sctx, steps, rounds)
	var eligible []types.TestArtifact
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
		eligible = append(eligible, artifact)
	}
	ok, reason := mediaAttachEnabled(sctx, provider)
	if !ok {
		if len(eligible) > 0 || len(skipped) > 0 {
			sctx.Log(fmt.Sprintf("skipping GitHub media attachments: %s", reason))
		}
		return nil
	}
	for _, line := range skipped {
		sctx.Log("skipping GitHub media attachment for " + line)
	}
	if len(eligible) == 0 {
		return nil
	}
	if len(eligible) > maxPRMediaAttachments {
		for _, extra := range eligible[maxPRMediaAttachments:] {
			sctx.Log(fmt.Sprintf("skipping GitHub media attachment for %s: more than %d files in one PR", artifactLabel(extra), maxPRMediaAttachments))
		}
		eligible = eligible[:maxPRMediaAttachments]
	}

	attached := make(map[string]string, len(eligible))
	type pendingUpload struct {
		artifact types.TestArtifact
		digest   string
	}
	pending := make([]pendingUpload, 0, len(eligible))
	for _, artifact := range eligible {
		digest, err := mediaFileDigest(artifact.Path)
		if err != nil {
			sctx.Log(fmt.Sprintf("GitHub media attachment failed for %s, keeping today's rendering: fingerprint file: %v", artifactLabel(artifact), err))
			continue
		}
		cached, found, err := sctx.DB.GetRunMediaAttachment(sctx.Run.ID, artifact.Path, digest)
		if err != nil {
			sctx.Log(fmt.Sprintf("GitHub media attachment cache failed for %s, keeping today's rendering: %v", artifactLabel(artifact), err))
			continue
		}
		if found {
			attached[artifact.Path] = cached.URL
			sctx.Log(fmt.Sprintf("reused GitHub media attachment for %s", artifactLabel(artifact)))
			continue
		}
		pending = append(pending, pendingUpload{artifact: artifact, digest: digest})
	}
	if len(pending) == 0 {
		if len(attached) == 0 {
			return nil
		}
		return attached
	}

	uploader := s.resolveMediaUploader(sctx, provider)
	if uploader == nil {
		sctx.Log("skipping GitHub media attachments: GitHub host is not available")
		if len(attached) == 0 {
			return nil
		}
		return attached
	}
	for _, item := range pending {
		artifact := item.artifact
		url, err := uploader.UploadUserAsset(sctx.Ctx, artifact.Path)
		if err != nil {
			sctx.Log(fmt.Sprintf("GitHub media attachment failed for %s, keeping today's rendering: %v", artifactLabel(artifact), err))
			continue
		}
		if strings.TrimSpace(url) == "" {
			sctx.Log(fmt.Sprintf("GitHub media attachment returned no URL for %s, keeping today's rendering", artifactLabel(artifact)))
			continue
		}
		if err := sctx.DB.UpsertRunMediaAttachment(sctx.Run.ID, db.RunMediaAttachment{Path: artifact.Path, Digest: item.digest, URL: url}); err != nil {
			sctx.Log(fmt.Sprintf("warning: failed to cache GitHub media attachment for %s: %v", artifactLabel(artifact), err))
		}
		attached[artifact.Path] = url
		sctx.Log(fmt.Sprintf("uploaded GitHub media attachment for %s", artifactLabel(artifact)))
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
	if _, err := github.ValidateUserAsset(artifact.Path); err != nil {
		return err.Error()
	}
	return ""
}

func artifactLabel(artifact types.TestArtifact) string {
	if label := strings.TrimSpace(artifact.Label); label != "" {
		return label
	}
	return filepath.Base(artifact.Path)
}
