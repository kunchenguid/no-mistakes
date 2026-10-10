package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeProjectAsset(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("media-bytes\x00\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadMediaSendsProjectMultipartAndReturnsValidatedURL(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"shot.png", "clip.MP4", "still.avif", "clip.m4v", "clip.ogv", "screen shot.bmp"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			path := writeProjectAsset(t, root, name)
			assetURL := "/uploads/abc123/" + strings.ReplaceAll(name, " ", "%20")
			response, err := json.Marshal(map[string]string{"url": assetURL})
			if err != nil {
				t.Fatal(err)
			}
			host := New(func(ctx context.Context, binary string, args ...string) *exec.Cmd {
				if binary != "glab" || len(args) != 10 || strings.Join(args[:7], " ") != "api --hostname gitlab.example --method POST --input -" || args[7] != "--header" || args[9] != "projects/group%2Fsub%2Fproject/uploads" {
					t.Fatalf("wrong upload destination or request: %s %v", binary, args)
				}
				_, parameters, err := mime.ParseMediaType(strings.TrimPrefix(args[8], "Content-Type: "))
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProjectUploadHelperProcess$")
				cmd.Env = append(os.Environ(), "GITLAB_UPLOAD_HELPER=1", "GITLAB_UPLOAD_BOUNDARY="+parameters["boundary"], "GITLAB_UPLOAD_NAME="+name, "GITLAB_UPLOAD_RESPONSE="+string(response))
				return cmd
			}, nil, "gitlab.example", "group/sub/project")
			got, err := host.UploadMedia(context.Background(), path, root)
			if err != nil {
				t.Fatal(err)
			}
			if got != assetURL {
				t.Fatalf("upload URL = %q, want %q", got, assetURL)
			}
		})
	}
}

func TestProjectUploadHelperProcess(t *testing.T) {
	if os.Getenv("GITLAB_UPLOAD_HELPER") != "1" {
		return
	}
	reader := multipart.NewReader(os.Stdin, os.Getenv("GITLAB_UPLOAD_BOUNDARY"))
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() != os.Getenv("GITLAB_UPLOAD_NAME") {
		os.Exit(2)
	}
	body, err := io.ReadAll(part)
	if err != nil || string(body) != "media-bytes\x00\xff" {
		os.Exit(3)
	}
	if _, err := reader.NextPart(); err != io.EOF {
		os.Exit(4)
	}
	fmt.Fprint(os.Stdout, os.Getenv("GITLAB_UPLOAD_RESPONSE"))
	os.Exit(0)
}

func TestUploadMediaRefusesUnsafeEvidenceBeforeLaunchingGlab(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := writeProjectAsset(t, t.TempDir(), "private.png")
	inside := writeProjectAsset(t, root, "inside.png")
	empty := filepath.Join(root, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory.png")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path, root string }{
		{"outside", outside, root},
		{"unknown root", inside, ""},
		{"relative", "inside.png", root},
		{"traversal", root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root) + string(filepath.Separator) + "inside.png", root},
		{"missing", filepath.Join(root, "missing.png"), root},
		{"empty", empty, root},
		{"directory", directory, root},
		{"text", writeProjectAsset(t, root, "notes.txt"), root},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				t.Fatalf("unsafe evidence launched %s %v", name, args)
				return nil
			}, nil, "gitlab.example", "group/project")
			if reference, err := host.UploadMedia(context.Background(), tc.path, tc.root); err == nil || reference != "" {
				t.Fatalf("reference=%q error=%v, want refusal", reference, err)
			}
		})
	}
	for _, tc := range []struct{ name, target string }{{"inside link", inside}, {"outside link", outside}, {"directory link", root}} {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(root, strings.ReplaceAll(tc.name, " ", "-")+".png")
			if err := os.Symlink(tc.target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if tc.name == "directory link" {
				link = filepath.Join(link, "inside.png")
			}
			if err := ValidateProjectAsset(link, root); err == nil || !strings.Contains(err.Error(), "symbolic links") {
				t.Fatalf("error=%v, want symlink refusal", err)
			}
		})
	}
}

func TestUploadMediaRejectsUnusableResponsesWithoutDisclosingCredentials(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := writeProjectAsset(t, root, "shot.png")
	const secret = "glpat-fixture-secret"
	cases := []struct {
		name, response string
		exit           int
	}{
		{"auth failure", secret, 1},
		{"malformed", secret, 0},
		{"missing", `{}`, 0},
		{"absolute", `{"url":"https://other.example/uploads/abc/shot.png"}`, 0},
		{"traversal", `{"url":"/uploads/%2e%2e/shot.png"}`, 0},
		{"query", `{"url":"/uploads/abc/shot.png?token=secret"}`, 0},
		{"text", `{"url":"/uploads/abc/notes.txt"}`, 0},
		{"markup", `{"url":"![shot](/uploads/abc/shot.png)"}`, 0},
		{"newline", `{"url":"/uploads/abc/shot.png\nextra"}`, 0},
		{"encoded slash", `{"url":"/uploads/abc%2fdef/shot.png"}`, 0},
		{"empty query", `{"url":"/uploads/abc/shot.png?"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if strings.Contains(strings.Join(args, " "), secret) {
					t.Fatal("credential in command arguments")
				}
				key := strings.TrimSpace(name + " " + strings.Join(args, " "))
				return gitlabTestCmdFactory(map[string]gitlabTestResponse{key: {stdout: tc.response, stderr: secret, code: tc.exit}})(ctx, name, args...)
			}, nil, "gitlab.example", "group/project")
			reference, err := host.UploadMedia(context.Background(), path, root)
			if err == nil || reference != "" {
				t.Fatalf("reference=%q error=%v, want refusal", reference, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("credential disclosed: %v", err)
			}
		})
	}
}
