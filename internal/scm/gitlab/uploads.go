package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

// Project-file upload for merge request descriptions.
//
// GitLab's markdown upload is POST /projects/:id/uploads with a multipart
// file field, not a JSON body. glab api -F file=@path JSON-encodes the file
// bytes, which this endpoint rejects. glab 1.74+ added --form for real
// multipart; the 1.73 binary still in use here has no --form. Probe `glab api
// --help` once per Host and use --form when it is listed. Otherwise build the
// multipart body and pass it with --input so the request still goes through
// glab and uses glab's host-scoped credential. Do not read the token out of
// glab and post it from Go.

const gitlabUploadFormFlag = "--form"

// ProjectUpload is the markdown GitLab returned for one uploaded file.
type ProjectUpload struct {
	Markdown string
	URL      string
}

type gitlabUploadResponse struct {
	Markdown string `json:"markdown"`
	URL      string `json:"url"`
	Alt      string `json:"alt"`
}

// UploadProjectFile posts path to this host's project and returns the
// markdown GitLab says to embed. The filename sent to GitLab is the base
// name only, so the response cannot carry a local directory.
func (h *Host) UploadProjectFile(ctx context.Context, path string) (ProjectUpload, error) {
	if h == nil || h.cmd == nil {
		return ProjectUpload{}, errors.New("gitlab upload: host is not configured")
	}
	if strings.TrimSpace(h.projectPath) == "" {
		return ProjectUpload{}, errors.New("gitlab upload: project path is unknown")
	}
	path = strings.TrimSpace(path)
	info, err := os.Stat(path)
	if err != nil {
		return ProjectUpload{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return ProjectUpload{}, fmt.Errorf("gitlab upload: %s is not a non-empty file", path)
	}
	stdout, err := h.postProjectUpload(ctx, path)
	if err != nil {
		return ProjectUpload{}, err
	}
	upload, err := parseProjectUpload(stdout)
	if err != nil {
		return ProjectUpload{}, err
	}
	return upload, nil
}

func (h *Host) postProjectUpload(ctx context.Context, path string) ([]byte, error) {
	endpoint := fmt.Sprintf("projects/%s/uploads", encodeProjectPath(h.projectPath))
	args := []string{"api", "--method", "POST"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	var cleanup func()
	if h.glabSupportsUploadForm(ctx) {
		args = append(args, gitlabUploadFormFlag, "file=@"+path)
	} else {
		body, err := os.CreateTemp("", "nm-gitlab-upload-*")
		if err != nil {
			return nil, err
		}
		bodyPath := body.Name()
		cleanup = func() { os.Remove(bodyPath) }
		header, err := writeUploadBody(body, path)
		closeErr := body.Close()
		if err != nil {
			cleanup()
			return nil, err
		}
		if closeErr != nil {
			cleanup()
			return nil, closeErr
		}
		args = append(args, "--header", header, "--input", bodyPath)
	}
	if cleanup != nil {
		defer cleanup()
	}
	args = append(args, endpoint)
	stdout, combined, err := h.glabStdout(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("gitlab upload: %s: %w", strings.TrimSpace(string(combined)), err)
	}
	return stdout, nil
}

func (h *Host) glabSupportsUploadForm(ctx context.Context) bool {
	h.formProbeOnce.Do(func() {
		cmd := h.cmd(ctx, "glab", "api", "--help")
		if cmd == nil {
			return
		}
		out, err := cmd.CombinedOutput()
		h.formProbeOK = err == nil && bytes.Contains(out, []byte(gitlabUploadFormFlag))
	})
	return h.formProbeOK
}

func writeUploadBody(w io.Writer, path string) (contentHeader string, err error) {
	writer := multipart.NewWriter(w)
	contentHeader = "Content-Type: " + writer.FormDataContentType()
	filename := filepath.Base(path)
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		filename = "evidence" + filepath.Ext(path)
	}
	part, err := writer.CreatePart(uploadPartHeader(filename))
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return contentHeader, nil
}

func uploadPartHeader(filename string) textproto.MIMEHeader {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, strings.ReplaceAll(filename, `"`, "")))
	h.Set("Content-Type", uploadContentType(filename))
	return h
}

func uploadContentType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

func parseProjectUpload(raw []byte) (ProjectUpload, error) {
	var parsed gitlabUploadResponse
	if err := json.Unmarshal(bytes.TrimSpace(raw), &parsed); err != nil {
		return ProjectUpload{}, fmt.Errorf("gitlab upload: decode response: %w", err)
	}
	markdown := strings.TrimSpace(parsed.Markdown)
	url := strings.TrimSpace(parsed.URL)
	if markdown == "" || strings.ContainsAny(markdown, "\r\n") || !strings.HasPrefix(markdown, "![") || !strings.Contains(markdown, "](") {
		return ProjectUpload{}, errors.New("gitlab upload: response markdown is not a single image embed")
	}
	if url == "" || !strings.Contains(url, "/uploads/") || !strings.Contains(markdown, url) {
		return ProjectUpload{}, errors.New("gitlab upload: response URL is not a project upload")
	}
	return ProjectUpload{Markdown: markdown, URL: url}, nil
}
