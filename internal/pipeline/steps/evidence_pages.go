package steps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cloudflare"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The run's own branch name would replace that branch's site preview in a
// project that also serves previews.
const pagesEvidenceBranch = "no-mistakes-evidence"

const maxPagesFileBytes = 25 << 20

const pagesDeployTimeout = 5 * time.Minute

const noIndexPagesHeaders = "/*\n  X-Robots-Tag: noindex\n  Referrer-Policy: no-referrer\n"

// The artifact kind alone would publish a file of any type as a screenshot.
var pagesMediaExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true,
	".mp4": true, ".mov": true, ".webm": true,
}

func deployPages(sctx *pipeline.StepContext, req cloudflare.DeployRequest) (string, error) {
	name, args := "wrangler", req.Args()
	if !stepExecutableAvailable(sctx, name) {
		if !stepExecutableAvailable(sctx, "npx") {
			return "", errors.New("neither wrangler nor npx is on the daemon's PATH")
		}
		name, args = "npx", append([]string{"--yes", cloudflare.WranglerPackage}, args...)
	}
	work, err := os.MkdirTemp("", "nm-wrangler-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	outputFile := filepath.Join(work, "output.ndjson")
	ctx, cancel := context.WithTimeout(sctx.Ctx, pagesDeployTimeout)
	defer cancel()
	cmd := stepCmdContext(sctx, ctx, name, args...)
	// Run outside the repository so a wrangler config in it cannot reshape the deploy.
	cmd.Dir = work
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, cloudflare.OutputFileEnv+"="+outputFile, "WRANGLER_SEND_METRICS=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("wrangler pages deploy: %w: %s", err, outputTail(out, 600))
	}
	data, err := os.ReadFile(outputFile)
	if err != nil {
		return "", fmt.Errorf("read wrangler output: %w", err)
	}
	return cloudflare.ParseDeployOutput(data)
}

func outputTail(out []byte, limit int) string {
	text := strings.TrimSpace(string(out))
	if len(text) > limit {
		text = "..." + text[len(text)-limit:]
	}
	return text
}

func pagesMediaPublisher(project string) string {
	return "cloudflare-pages:" + project
}

func missingPagesCredentials(sctx *pipeline.StepContext) []string {
	env := stepEnvironment(sctx)
	if env == nil {
		env = os.Environ()
	}
	var missing []string
	for _, key := range cloudflare.CredentialEnv {
		if value, ok := envValue(env, key); !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, key)
		}
	}
	return missing
}

func skipPagesMedia(artifact types.TestArtifact) string {
	if artifact.URL != "" || artifact.Path == "" {
		return "no local evidence file"
	}
	if !pagesMediaExtensions[strings.ToLower(filepath.Ext(artifact.Path))] {
		return "not a png, jpg, gif, webp, svg, mp4, mov or webm file"
	}
	if !filepath.IsAbs(artifact.Path) {
		return "path is not an absolute evidence file"
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return err.Error()
	}
	if !info.Mode().IsRegular() {
		return "not a regular file"
	}
	if info.Size() == 0 {
		return "file is empty"
	}
	if info.Size() > maxPagesFileBytes {
		return "files must be at most 25 MiB on Cloudflare Pages"
	}
	return ""
}

// A file left out of the returned map keeps the GitHub fallback.
func publishPagesMedia(sctx *pipeline.StepContext, artifacts []types.TestArtifact) map[string]string {
	if sctx == nil || sctx.Config == nil || sctx.Run == nil || sctx.DB == nil {
		return nil
	}
	project := sctx.Config.Test.Evidence.CloudflarePagesProject
	if project == "" || !sctx.Config.Test.Evidence.AttachMedia {
		return nil
	}
	var eligible []types.TestArtifact
	seen := make(map[string]bool)
	for _, artifact := range artifacts {
		if !isImageArtifact(artifact.Kind, artifact.Path) && !isVideoArtifact(artifact.Kind, artifact.Path) {
			continue
		}
		if reason := skipPagesMedia(artifact); reason != "" {
			sctx.Log(fmt.Sprintf("skipping Cloudflare Pages evidence for %s: %s", artifactLabel(artifact), reason))
			continue
		}
		clean := filepath.Clean(artifact.Path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		artifact.Path = clean
		eligible = append(eligible, artifact)
	}
	if len(eligible) == 0 {
		return nil
	}
	if missing := missingPagesCredentials(sctx); len(missing) > 0 {
		sctx.Log(fmt.Sprintf("skipping Cloudflare Pages evidence, falling back to GitHub media attachments: %s not set in the daemon environment", strings.Join(missing, " and ")))
		return nil
	}
	if len(eligible) > maxPRMediaAttachments {
		eligible = eligible[:maxPRMediaAttachments]
	}

	publisher := pagesMediaPublisher(project)
	published := make(map[string]string, len(eligible))
	type pendingFile struct {
		artifact types.TestArtifact
		digest   string
		name     string
	}
	var pending []pendingFile
	for _, artifact := range eligible {
		digest, err := mediaFileDigest(artifact.Path)
		if err != nil {
			sctx.Log(fmt.Sprintf("Cloudflare Pages evidence failed for %s: fingerprint file: %v", artifactLabel(artifact), err))
			continue
		}
		cached, found, err := sctx.DB.GetRunMediaAttachment(sctx.Run.ID, artifact.Path, digest, publisher)
		if err != nil {
			sctx.Log(fmt.Sprintf("Cloudflare Pages evidence cache failed for %s: %v", artifactLabel(artifact), err))
			continue
		}
		if found {
			published[artifact.Path] = cached.URL
			continue
		}
		pending = append(pending, pendingFile{artifact: artifact, digest: digest, name: pagesFileName(digest, artifact.Path)})
	}
	if len(pending) == 0 {
		return published
	}

	site, err := os.MkdirTemp("", "nm-pages-site-")
	if err != nil {
		sctx.Log(fmt.Sprintf("Cloudflare Pages evidence not published, falling back to GitHub media attachments: %v", err))
		return published
	}
	defer os.RemoveAll(site)
	staged := pending[:0]
	for _, item := range pending {
		if err := copyRegularFile(item.artifact.Path, filepath.Join(site, filepath.FromSlash(item.name))); err != nil {
			sctx.Log(fmt.Sprintf("Cloudflare Pages evidence failed for %s: %v", artifactLabel(item.artifact), err))
			continue
		}
		staged = append(staged, item)
	}
	if len(staged) == 0 {
		return published
	}
	if err := os.WriteFile(filepath.Join(site, "_headers"), []byte(noIndexPagesHeaders), 0o644); err != nil {
		sctx.Log(fmt.Sprintf("Cloudflare Pages evidence not published, falling back to GitHub media attachments: %v", err))
		return published
	}
	origin, err := deployPages(sctx, cloudflare.DeployRequest{
		Project:       project,
		Branch:        pagesEvidenceBranch,
		CommitHash:    sctx.Run.HeadSHA,
		CommitMessage: "no-mistakes evidence for run " + sctx.Run.ID,
		Dir:           site,
	})
	if err != nil {
		sctx.Log(fmt.Sprintf("Cloudflare Pages evidence not published, falling back to GitHub media attachments: %v", err))
		return published
	}
	for _, item := range staged {
		link := origin + "/" + joinURLPath("", item.name)
		if err := sctx.DB.UpsertRunMediaAttachment(sctx.Run.ID, db.RunMediaAttachment{Path: item.artifact.Path, Digest: item.digest, Publisher: publisher, URL: link}); err != nil {
			sctx.Log(fmt.Sprintf("warning: failed to cache Cloudflare Pages evidence for %s: %v", artifactLabel(item.artifact), err))
		}
		published[item.artifact.Path] = link
	}
	sctx.Log(fmt.Sprintf("published %d evidence file(s) to Cloudflare Pages project %s (%s)", len(staged), project, origin))
	return published
}

// The digest prefix keeps two evidence files with one base name apart.
func pagesFileName(digest, localPath string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 16 {
		hex = hex[:16]
	}
	return path.Join(hex, filepath.Base(localPath))
}

func copyRegularFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, maxPagesFileBytes+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > maxPagesFileBytes {
		err = errors.New("file grew past 25 MiB while it was copied")
	}
	return err
}
