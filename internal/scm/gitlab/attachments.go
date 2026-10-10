package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const projectAssetUploadTimeout = 60 * time.Second

var _ scm.MediaUploader = (*Host)(nil)

// UploadMedia posts one evidence file to GitLab's project uploads API and
// returns its validated project-relative URL. glab handles authentication,
// api_host/api_protocol, and the run's forge profile, so no token appears in
// our arguments or errors. The multipart body goes to glab on stdin, which
// works with versions that predate --form and keeps the checked file open.
func (h *Host) UploadMedia(ctx context.Context, path, evidenceRoot string) (string, error) {
	if h == nil || h.host == "" || h.projectPath == "" {
		return "", errors.New("could not resolve GitLab host and project for media upload")
	}
	file, err := openProjectAsset(path, evidenceRoot)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Only the multipart framing is buffered. The file itself streams from the
	// descriptor openProjectAsset opened after checking containment and identity.
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	if _, err := writer.CreateFormFile("file", filepath.Base(path)); err != nil {
		return "", fmt.Errorf("create GitLab upload form: %w", err)
	}
	headerLen := framing.Len()
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close GitLab upload form: %w", err)
	}
	body := io.MultiReader(bytes.NewReader(framing.Bytes()[:headerLen]), file, bytes.NewReader(framing.Bytes()[headerLen:]))

	ctx, cancel := context.WithTimeout(ctx, projectAssetUploadTimeout)
	defer cancel()
	args := []string{"api", "--hostname", h.host, "--method", "POST", "--input", "-",
		"--header", "Content-Type: " + writer.FormDataContentType(),
		"projects/" + url.PathEscape(h.projectPath) + "/uploads"}
	cmd := h.cmd(ctx, "glab", args...)
	cmd.Stdin = body
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// Discard stderr: glab's debug HTTP output or an auth error could include a
	// credential.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("GitLab project upload: %w", ctx.Err())
		}
		return "", errors.New("GitLab project upload failed; check glab authentication and project upload permissions")
	}
	var response struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return "", errors.New("GitLab project upload returned invalid JSON")
	}
	if !validProjectAssetURL(response.URL) {
		return "", errors.New("GitLab project upload returned no usable upload URL")
	}
	return response.URL, nil
}

// ValidateProjectAsset refuses anything that is not an image or video, any path
// outside this run's evidence root, and symbolic links anywhere on the path,
// including directory links inside the root. UploadMedia repeats the check
// before it sends any bytes.
func ValidateProjectAsset(path, evidenceRoot string) error {
	file, err := openProjectAsset(path, evidenceRoot)
	if err != nil {
		return err
	}
	return file.Close()
}

func openProjectAsset(path, evidenceRoot string) (*os.File, error) {
	if !projectAssetMediaType(path) {
		return nil, errors.New("not a supported image or video file type")
	}
	if !filepath.IsAbs(path) || evidenceRoot == "" || filepath.Clean(path) != path {
		return nil, errors.New("path is not an absolute file under the evidence root")
	}
	rootPath, err := filepath.Abs(evidenceRoot)
	if err != nil {
		return nil, errors.New("cannot resolve evidence root")
	}
	rel, err := filepath.Rel(rootPath, path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return nil, errors.New("path is outside the evidence root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errors.New("cannot open evidence root")
	}
	defer root.Close()
	var info os.FileInfo
	component := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		component = filepath.Join(component, part)
		info, err = root.Lstat(component)
		if err != nil {
			return nil, errors.New("evidence file is missing or unreadable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symbolic links are not uploaded")
		}
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("evidence must be a non-empty regular file")
	}
	// Root.Open prevents a concurrent symlink replacement from escaping the
	// evidence root. SameFile rejects a file replaced after the Lstat check.
	file, err := root.Open(rel)
	if err != nil {
		return nil, errors.New("cannot open evidence file")
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		file.Close()
		return nil, errors.New("evidence file changed during upload validation")
	}
	return file, nil
}

func projectAssetMediaType(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".avif", ".mp4", ".webm", ".mov", ".m4v", ".ogv":
		return true
	}
	return false
}

func validProjectAssetURL(raw string) bool {
	if len(raw) > 4096 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n<>[]()\"'`\\#") {
		return false
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	parts := strings.Split(parsed.Path, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "uploads" || parts[2] == "" || parts[2] == "." || parts[2] == ".." || parts[3] == "" || parts[3] == "." || parts[3] == ".." {
		return false
	}
	return projectAssetMediaType(parts[3])
}
