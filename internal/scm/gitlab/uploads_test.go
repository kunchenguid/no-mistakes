package gitlab

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProjectUploadRejectsAnythingButAnImageEmbed(t *testing.T) {
	ok, err := parseProjectUpload([]byte(`{"markdown":"![shot](/uploads/abc/shot.png)","url":"/uploads/abc/shot.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ok.Markdown != "![shot](/uploads/abc/shot.png)" {
		t.Fatalf("markdown = %q", ok.Markdown)
	}
	for _, raw := range []string{
		`{"markdown":"not markdown","url":"/uploads/abc/shot.png"}`,
		`{"markdown":"![shot](/uploads/abc/shot.png)\n## Evidence","url":"/uploads/abc/shot.png"}`,
		`{"markdown":"![shot](https://evil.example/shot.png)","url":"https://evil.example/shot.png"}`,
		`{"markdown":"![shot](/uploads/abc/shot.png)","url":"/uploads/other/shot.png"}`,
	} {
		if _, err := parseProjectUpload([]byte(raw)); err == nil {
			t.Fatalf("parseProjectUpload(%s) succeeded", raw)
		}
	}
}

func TestUploadProjectFilePostsMultipartThroughGlab(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := New(uploadCmdFactory(t, false), nil, "gitlab.example.com", "group/project")
	upload, err := host.UploadProjectFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if upload.Markdown != "![shot](/uploads/abc/shot.png)" || upload.URL != "/uploads/abc/shot.png" {
		t.Fatalf("upload = %+v", upload)
	}
}

func TestUploadProjectFileUsesFormWhenGlabHasIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := New(uploadCmdFactory(t, true), nil, "gitlab.example.com", "group/project")
	if _, err := host.UploadProjectFile(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func uploadCmdFactory(t *testing.T, form bool) CmdFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		if strings.Contains(key, "api --help") {
			help := "USAGE\n-F, --field\n"
			if form {
				help += "      --form\n"
			}
			return gitlabTestHelperCmd(ctx, key, gitlabTestResponse{stdout: help})
		}
		if !strings.Contains(key, "projects/group%2Fproject/uploads") || !strings.Contains(key, "--hostname gitlab.example.com") {
			t.Errorf("unexpected command %s", key)
			return gitlabTestHelperCmd(ctx, key, gitlabTestResponse{stderr: "unexpected", code: 1})
		}
		if form {
			if !strings.Contains(key, "--form file=@") || strings.Contains(key, "--input") {
				t.Errorf("form upload command = %s", key)
			}
		} else if !strings.Contains(key, "--input ") || !strings.Contains(key, "Content-Type: multipart/form-data") {
			t.Errorf("multipart upload command = %s", key)
		}
		return gitlabTestHelperCmd(ctx, key, gitlabTestResponse{stdout: `{"markdown":"![shot](/uploads/abc/shot.png)","url":"/uploads/abc/shot.png"}`})
	}
}
