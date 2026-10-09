package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Project markdown uploads contract (POST /projects/:id/uploads).
//
// The upload goes through glab so it reuses the operator's existing glab login
// for this host, the same credential every other GitLab call here uses:
//
//	glab api --method POST projects/<url-encoded project path>/uploads --form file=@<path> [--hostname <host>]
//
// The response is {"id":5,"alt":"shot","url":"/uploads/<32 hex>/shot.png",
// "full_path":"/-/project/<id>/uploads/<32 hex>/shot.png","markdown":"![shot](/uploads/...)"}.
//
// UploadUserAsset returns the relative url. It is the form GitLab's own editor
// and the response's markdown field write, and GitLab expands it against the
// project the description belongs to (into its stable /-/project/<id>/uploads
// link), so no web URL has to be derived from the remote or an SSH alias. The
// merge request always lives in that same project because fork merge requests
// are not routed on GitLab.
//
// An image-syntax link renders as an image; one whose target has a video
// extension (mp4, m4v, mov, webm, ogv) renders as an inline player. A bare
// upload URL on its own line is left as plain text, so videos must use image
// syntax here (GitHub's convention is the opposite).
//
// Client-side file rules: extension only, case-insensitive; regular non-empty
// files only; at most 100 MiB, the default maximum attachment size on
// GitLab.com and self-managed instances (an instance may set a lower limit and
// the server still enforces it). SVG is left out: GitLab treats it as an
// unsafe image type, so an uploaded SVG is not guaranteed to display inline.
// Images and videos with any other extension keep their local rendering.

const (
	maxUploadBytes int64 = 100 * 1024 * 1024
	uploadTimeout        = 5 * time.Minute
)

var (
	uploadExtensions = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".mp4", ".m4v", ".mov", ".webm", ".ogv"}
	// uploadURLPattern accepts only the relative /uploads/<secret>/<filename>
	// shape GitLab documents. GitLab sanitizes the stored filename, so a
	// response carrying whitespace or Markdown punctuation is not one this
	// adapter understands and must not be embedded into the description.
	uploadURLPattern = regexp.MustCompile(`(?i)^/uploads/[0-9a-f]{32}/[^/\s()\[\]<>"'\\]+$`)
)

// ValidateUserAsset applies GitLab's client-side upload rules. A failure here
// must not produce an upload.
func (h *Host) ValidateUserAsset(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("empty attachment path")
	}
	info, err := os.Stat(path)
	if err != nil {
		if pathErr, ok := err.(*fs.PathError); ok {
			return fmt.Errorf("%s: %w", path, pathErr.Err)
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	ext := strings.ToLower(filepath.Ext(path))
	supported := false
	for _, candidate := range uploadExtensions {
		if candidate == ext {
			supported = true
			break
		}
	}
	if !supported {
		names := make([]string, len(uploadExtensions))
		for i, candidate := range uploadExtensions {
			names[i] = strings.TrimPrefix(candidate, ".")
		}
		return fmt.Errorf("%s is not a supported file type (supported: %s)", path, strings.Join(names, ", "))
	}
	if info.Size() > maxUploadBytes {
		return fmt.Errorf("%s: attachments must be at most %.1f MB", path, float64(maxUploadBytes)/(1024*1024))
	}
	return nil
}

// UploadUserAsset validates path and uploads it to this Host's project as a
// Markdown upload, returning the relative URL to embed. Callers must treat any
// error as fail-closed: keep today's PR rendering rather than inventing a URL.
func (h *Host) UploadUserAsset(ctx context.Context, path string) (string, error) {
	if h == nil {
		return "", errors.New("GitLab host is not configured")
	}
	project := strings.Trim(strings.TrimSpace(h.projectPath), "/")
	if project == "" {
		return "", errors.New("cannot determine the GitLab project path to upload attachments to")
	}
	path = strings.TrimSpace(path)
	if err := h.ValidateUserAsset(path); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	args := []string{
		"api", "--method", "POST",
		fmt.Sprintf("projects/%s/uploads", encodeProjectPath(project)),
		"--form", "file=@" + path,
	}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	// stdout is read on its own: a glab notice on stderr (an update banner)
	// must not land after the JSON document and make it unreadable.
	var stdout, stderr bytes.Buffer
	cmd := h.cmd(ctx, "glab", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("glab api project upload: %s: %w", strings.TrimSpace(stderr.String()+" "+stdout.String()), err)
	}
	var upload struct {
		URL string `json:"url"`
	}
	trimmed := bytesTrimToJSON(stdout.Bytes())
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &upload) != nil {
		return "", fmt.Errorf("glab api project upload: invalid JSON output: %s", strings.TrimSpace(stdout.String()))
	}
	url := strings.TrimSpace(upload.URL)
	if !uploadURLPattern.MatchString(url) {
		return "", fmt.Errorf("glab api project upload: unexpected upload URL %q", url)
	}
	return url, nil
}
