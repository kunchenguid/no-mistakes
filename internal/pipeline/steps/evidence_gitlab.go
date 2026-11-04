package steps

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

const (
	maxGitlabEvidenceImages   = 50
	maxGitlabEvidenceImageMiB = 10
)

// gitlabEvidenceUploader posts one local image to the GitLab project that owns
// the merge request. *gitlab.Host implements it. Tests inject a stub.
type gitlabEvidenceUploader interface {
	UploadProjectFile(ctx context.Context, path string) (gitlab.ProjectUpload, error)
}

// gitlabEvidenceSection uploads this run's images and returns one Evidence
// section. GitLab only. A later render strips the previous section and writes
// this one again, reusing an upload when the file hash was already sent.
func (s *PRStep) gitlabEvidenceSection(sctx *pipeline.StepContext, provider scm.Provider) string {
	if provider != scm.ProviderGitLab || sctx == nil || sctx.Repo == nil {
		return ""
	}
	runDir := testEvidenceDir(sctx)
	if runDir == "" {
		return ""
	}
	project := gitlab.ProjectPath(sctx.Repo.UpstreamURL)
	if project == "" {
		sctx.Log("skipping GitLab evidence uploads: project path is unknown")
		return ""
	}
	images, skipped, err := gitlabEvidenceImages(runDir)
	if err != nil {
		sctx.Log(fmt.Sprintf("skipping GitLab evidence uploads: %v", err))
		return ""
	}
	for _, rel := range skipped {
		sctx.Log(fmt.Sprintf("skipping GitLab evidence upload for %s: empty or larger than %d MiB", rel, maxGitlabEvidenceImageMiB))
	}
	if len(images) == 0 {
		return ""
	}
	if len(images) > maxGitlabEvidenceImages {
		for _, extra := range images[maxGitlabEvidenceImages:] {
			sctx.Log(fmt.Sprintf("skipping GitLab evidence upload for %s: more than %d images in one PR", extra.rel, maxGitlabEvidenceImages))
		}
		images = images[:maxGitlabEvidenceImages]
	}
	var uploader gitlabEvidenceUploader
	var lines []string
	for _, image := range images {
		markdown, ok := s.cachedGitlabEvidence(sctx, project, image)
		if !ok {
			if uploader == nil {
				uploader = s.resolveGitlabUploader(sctx, provider)
				if uploader == nil {
					break
				}
			}
			uploaded, err := uploader.UploadProjectFile(sctx.Ctx, image.path)
			if err != nil {
				sctx.Log(fmt.Sprintf("GitLab evidence upload failed for %s, leaving it out of the Evidence section: %v", image.rel, err))
				continue
			}
			if err := validateGitlabEvidenceMarkdown(uploaded); err != nil {
				sctx.Log(fmt.Sprintf("GitLab evidence upload for %s returned unusable markdown, leaving it out: %v", image.rel, err))
				continue
			}
			markdown = uploaded.Markdown
			if sctx.DB != nil {
				if err := sctx.DB.PutGitlabEvidenceUpload(project, image.digest, markdown); err != nil {
					sctx.Log(fmt.Sprintf("warning: failed to cache GitLab evidence upload for %s: %v", image.rel, err))
				}
			}
			sctx.Log(fmt.Sprintf("uploaded GitLab evidence image %s", image.rel))
		} else {
			sctx.Log(fmt.Sprintf("reused GitLab evidence upload for %s", image.rel))
		}
		lines = append(lines, fmt.Sprintf("`%s`\n\n%s", strings.ReplaceAll(image.rel, "`", "'"), markdown))
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Evidence\n\n" + strings.Join(lines, "\n\n")
}

type gitlabEvidenceImage struct {
	path   string
	rel    string
	digest string
}

func (s *PRStep) cachedGitlabEvidence(sctx *pipeline.StepContext, project string, image gitlabEvidenceImage) (string, bool) {
	if sctx.DB == nil || image.digest == "" {
		return "", false
	}
	markdown, found, err := sctx.DB.GetGitlabEvidenceUpload(project, image.digest)
	if err != nil {
		sctx.Log(fmt.Sprintf("GitLab evidence upload cache failed for %s, uploading again: %v", image.rel, err))
		return "", false
	}
	if !found {
		return "", false
	}
	if err := validateGitlabEvidenceMarkdown(gitlab.ProjectUpload{Markdown: markdown, URL: uploadURLFromMarkdown(markdown)}); err != nil {
		sctx.Log(fmt.Sprintf("cached GitLab evidence upload for %s is unusable, uploading again: %v", image.rel, err))
		return "", false
	}
	return markdown, true
}

func (s *PRStep) resolveGitlabUploader(sctx *pipeline.StepContext, provider scm.Provider) gitlabEvidenceUploader {
	if s != nil && s.gitlabUploader != nil {
		return s.gitlabUploader
	}
	host, reason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log("skipping GitLab evidence uploads: " + reason)
		return nil
	}
	uploader, ok := host.(gitlabEvidenceUploader)
	if !ok {
		sctx.Log("skipping GitLab evidence uploads: GitLab host cannot upload files")
		return nil
	}
	return uploader
}

func validateGitlabEvidenceMarkdown(upload gitlab.ProjectUpload) error {
	markdown := strings.TrimSpace(upload.Markdown)
	if markdown == "" || strings.ContainsAny(markdown, "\r\n") || !strings.HasPrefix(markdown, "![") || !strings.Contains(markdown, "](") {
		return fmt.Errorf("not a single image embed")
	}
	url := strings.TrimSpace(upload.URL)
	if url == "" {
		url = uploadURLFromMarkdown(markdown)
	}
	if !strings.Contains(url, "/uploads/") || !strings.Contains(markdown, url) {
		return fmt.Errorf("url is not a project upload")
	}
	return nil
}

func uploadURLFromMarkdown(markdown string) string {
	start := strings.Index(markdown, "](")
	if start < 0 {
		return ""
	}
	rest := markdown[start+2:]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func gitlabEvidenceImages(runDir string) (images []gitlabEvidenceImage, skipped []string, err error) {
	root := filepath.Clean(runDir)
	parent := filepath.Dir(root)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == reviewqa.DirName && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if !gitlabEvidenceImageExt(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Size() == 0 || info.Size() > maxGitlabEvidenceImageMiB*1024*1024 {
			if rel, relErr := filepath.Rel(parent, path); relErr == nil {
				skipped = append(skipped, filepath.ToSlash(rel))
			}
			return nil
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return nil
		}
		digest, err := mediaFileDigest(path)
		if err != nil {
			return nil
		}
		images = append(images, gitlabEvidenceImage{
			path:   path,
			rel:    filepath.ToSlash(rel),
			digest: digest,
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(images, func(i, j int) bool { return images[i].rel < images[j].rel })
	return images, skipped, nil
}

func gitlabEvidenceImageExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	default:
		return false
	}
}
