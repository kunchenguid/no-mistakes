package steps

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// gitlabEvidenceUploader posts one local image to the GitLab project that owns
// the merge request. *gitlab.Host implements it. Tests inject a stub.
type gitlabEvidenceUploader interface {
	UploadProjectFile(ctx context.Context, path string) (gitlab.ProjectUpload, error)
}

// gitlabEvidenceSection uploads this run's images and returns one Evidence
// section. GitLab only. A later render strips the previous section and writes
// this one again, reusing an upload when the file hash was already sent.
//
// With test.evidence.link_base_url set, every entry names its file as a link
// under that base (evidence_links.go, shared with the Testing section). When
// uploads are off, that is the whole entry: the section lists the run's
// images as links and nothing leaves the machine. When uploads are on, the
// link replaces the path mention and the uploaded embed follows it as before.
func (s *PRStep) gitlabEvidenceSection(sctx *pipeline.StepContext, provider scm.Provider) string {
	if provider != scm.ProviderGitLab || sctx == nil || sctx.Repo == nil {
		return ""
	}
	runDir := testEvidenceDir(sctx)
	if runDir == "" {
		return ""
	}
	uploadOK, uploadReason := gitlabEvidenceUploadEnabled(sctx)
	linkBase := evidenceLinkBase(sctx.Config)
	if !uploadOK && linkBase == "" {
		sctx.Log("skipping GitLab evidence uploads: " + uploadReason)
		return ""
	}
	project := ""
	if uploadOK {
		project = gitlab.ProjectPath(sctx.Repo.UpstreamURL)
		if project == "" {
			sctx.Log("skipping GitLab evidence uploads: project path is unknown")
			return ""
		}
		project = strings.ToLower(resolvedHost(sctx, sctx.Repo.UpstreamURL)) + "/" + project
	} else {
		sctx.Log("listing GitLab evidence images as links under " + linkBase + " without uploading: " + uploadReason)
	}
	// The digest only keys the upload cache, so link-only mode lists the
	// files without reading a single image byte.
	images, problems, err := gitlabEvidenceImages(runDir, uploadOK)
	if err != nil {
		sctx.Log(fmt.Sprintf("skipping GitLab evidence uploads: %v", err))
		return ""
	}
	// mention is how an entry names its file: the link when link_base_url is
	// set, else the path relative to the evidence parent in a code span.
	mention := func(rel, path string) string {
		if link := renderEvidenceFileLink(linkBase, runDir, path, rel); link != "" {
			return link
		}
		return "`" + strings.ReplaceAll(rel, "`", "'") + "`"
	}
	var lines []string
	for _, problem := range problems {
		sctx.Log(fmt.Sprintf("GitLab evidence file unreadable at %s: %s", problem.rel, problem.reason))
		lines = append(lines, gitlabEvidenceNotUploaded(mention(problem.rel, problem.path), problem.reason))
	}
	var uploader gitlabEvidenceUploader
	for _, image := range images {
		if !uploadOK {
			lines = append(lines, mention(image.rel, image.path))
			continue
		}
		markdown, ok := s.cachedGitlabEvidence(sctx, project, image)
		if !ok {
			if uploader == nil {
				uploader = s.resolveGitlabUploader(sctx, provider)
				if uploader == nil {
					lines = append(lines, gitlabEvidenceNotUploaded(mention(image.rel, image.path), "GitLab uploader unavailable"))
					continue
				}
			}
			uploaded, err := uploader.UploadProjectFile(sctx.Ctx, image.path)
			if err != nil {
				sctx.Log(fmt.Sprintf("GitLab evidence upload failed for %s: %v", image.rel, err))
				lines = append(lines, gitlabEvidenceNotUploaded(mention(image.rel, image.path), err.Error()))
				continue
			}
			if err := validateGitlabEvidenceMarkdown(uploaded); err != nil {
				sctx.Log(fmt.Sprintf("GitLab evidence upload for %s returned unusable markdown: %v", image.rel, err))
				lines = append(lines, gitlabEvidenceNotUploaded(mention(image.rel, image.path), err.Error()))
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
		lines = append(lines, mention(image.rel, image.path)+"\n\n"+markdown)
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

func gitlabEvidenceUploadEnabled(sctx *pipeline.StepContext) (bool, string) {
	if sctx == nil || sctx.Config == nil {
		return false, "no configuration available for media attachments"
	}
	if !sctx.Config.Test.Evidence.GitLabAttachMedia {
		return false, "test.evidence.attach_media is disabled"
	}
	return true, ""
}

// gitlabEvidenceNotUploaded lists a file the section could not embed; mention
// is the entry's already-rendered file name (link or code span).
func gitlabEvidenceNotUploaded(mention, reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "unreadable"
	}
	return fmt.Sprintf("%s - not uploaded: %s", mention, reason)
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

type gitlabEvidenceProblem struct {
	path   string
	rel    string
	reason string
}

// gitlabEvidenceImages lists the image files under runDir, sorted by their
// path relative to the evidence parent. With digest set, each image is read
// and hashed for the upload cache; without it, no image content is read and
// digest stays empty.
func gitlabEvidenceImages(runDir string, digest bool) (images []gitlabEvidenceImage, problems []gitlabEvidenceProblem, err error) {
	root := filepath.Clean(runDir)
	parent := filepath.Dir(root)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root && os.IsNotExist(walkErr) {
				return nil
			}
			problems = append(problems, gitlabEvidenceProblem{path: path, rel: gitlabEvidenceRel(parent, path), reason: walkErr.Error()})
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
		rel, err := filepath.Rel(parent, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return nil
		}
		image := gitlabEvidenceImage{path: path, rel: filepath.ToSlash(rel)}
		if digest {
			sum, err := mediaFileDigest(path)
			if err != nil {
				problems = append(problems, gitlabEvidenceProblem{path: path, rel: filepath.ToSlash(rel), reason: err.Error()})
				return nil
			}
			image.digest = sum
		}
		images = append(images, image)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(images, func(i, j int) bool { return images[i].rel < images[j].rel })
	sort.Slice(problems, func(i, j int) bool { return problems[i].rel < problems[j].rel })
	return images, problems, nil
}

func gitlabEvidenceRel(parent, path string) string {
	rel, err := filepath.Rel(parent, path)
	if err != nil || rel == "." {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func gitlabEvidenceImageExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	default:
		return false
	}
}
