package gitlab

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testUploadURL = "/uploads/66dbcd21ec5d24ed6ea225176098d52b/checkout.png"

func writeUploadFile(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Truncate extends the file sparsely, so the size-limit cases stay cheap.
	if err := os.Truncate(path, int64(size)); err != nil {
		t.Fatal(err)
	}
	return path
}

func uploadCommandKey(project, path, hostname string) string {
	key := "glab api --method POST projects/" + project + "/uploads --form file=@" + path
	if hostname != "" {
		key += " --hostname " + hostname
	}
	return key
}

// TestUploadUserAssetReturnsTheRelativeUploadURL pins the upload call shape
// (nested project path encoded as one parameter, hostname scoping) and that the
// relative url is what gets embedded: GitLab expands it against the project
// the merge request description belongs to.
func TestUploadUserAssetReturnsTheRelativeUploadURL(t *testing.T) {
	t.Parallel()

	path := writeUploadFile(t, "checkout.png", 16)
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		uploadCommandKey("group%2Fsub%2Fproject", path, "gitlab.example.com"): {
			stdout: `{"id":5,"alt":"checkout","url":"` + testUploadURL + `","full_path":"/-/project/1234/uploads/66dbcd21ec5d24ed6ea225176098d52b/checkout.png","markdown":"![checkout](` + testUploadURL + `)"}` + "\n",
			stderr: "A new version of glab is available\n",
		},
	}), nil, "gitlab.example.com", "group/sub/project")

	got, err := host.UploadUserAsset(context.Background(), path)
	if err != nil {
		t.Fatalf("UploadUserAsset() error = %v", err)
	}
	if got != testUploadURL {
		t.Fatalf("UploadUserAsset() = %q, want %q", got, testUploadURL)
	}
}

func TestUploadUserAssetFailsClosed(t *testing.T) {
	t.Parallel()

	path := writeUploadFile(t, "checkout.webm", 16)
	cases := []struct {
		name     string
		response gitlabTestResponse
		want     string
	}{
		{name: "glab error", response: gitlabTestResponse{stderr: "403 Forbidden", code: 1}, want: "403 Forbidden"},
		{name: "not JSON", response: gitlabTestResponse{stdout: "uploaded\n"}, want: "invalid JSON"},
		{name: "no url", response: gitlabTestResponse{stdout: `{"id":5}`}, want: "unexpected upload URL"},
		{name: "absolute url", response: gitlabTestResponse{stdout: `{"url":"https://evil.example/uploads/66dbcd21ec5d24ed6ea225176098d52b/a.webm"}`}, want: "unexpected upload URL"},
		{name: "markdown breakout", response: gitlabTestResponse{stdout: `{"url":"/uploads/66dbcd21ec5d24ed6ea225176098d52b/a.webm) [x](https://evil.example"}`}, want: "unexpected upload URL"},
		{name: "short secret", response: gitlabTestResponse{stdout: `{"url":"/uploads/abc/a.webm"}`}, want: "unexpected upload URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
				uploadCommandKey("group%2Fproject", path, ""): tc.response,
			}), nil, "", "group/project")
			got, err := host.UploadUserAsset(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UploadUserAsset() = %q, %v; want error containing %q", got, err, tc.want)
			}
			if got != "" {
				t.Fatalf("UploadUserAsset() returned URL %q alongside an error", got)
			}
		})
	}
}

func TestUploadUserAssetWithoutProjectPathDoesNotRunGlab(t *testing.T) {
	t.Parallel()

	path := writeUploadFile(t, "checkout.png", 16)
	host := New(gitlabTestCmdFactory(nil), nil, "", "")
	if _, err := host.UploadUserAsset(context.Background(), path); err == nil || !strings.Contains(err.Error(), "project path") {
		t.Fatalf("UploadUserAsset() error = %v, want a missing project path error", err)
	}
}

// TestValidateUserAssetAppliesGitLabsRules covers the client-side rules that
// decide whether an artifact is uploaded at all: GitLab's 100 MiB attachment
// limit for images and videos alike, and only the extensions GitLab renders
// inline.
func TestValidateUserAssetAppliesGitLabsRules(t *testing.T) {
	t.Parallel()

	host := New(nil, nil, "", "group/project")
	dir := t.TempDir()
	for _, name := range []string{"shot.png", "shot.JPG", "shot.jpeg", "shot.gif", "shot.webp", "rec.mp4", "rec.m4v", "rec.mov", "rec.webm", "rec.ogv"} {
		if err := host.ValidateUserAsset(writeUploadFile(t, name, 8)); err != nil {
			t.Errorf("ValidateUserAsset(%s) = %v, want accepted", name, err)
		}
	}
	// An image above GitHub's 10 MiB image cap is still within GitLab's limit.
	if err := host.ValidateUserAsset(writeUploadFile(t, "large.png", 10*1024*1024+1)); err != nil {
		t.Errorf("ValidateUserAsset(11 MiB png) = %v, want accepted", err)
	}

	rejected := map[string]string{
		writeUploadFile(t, "huge.webm", int(maxUploadBytes)+1): "at most 100.0 MB",
		writeUploadFile(t, "vector.svg", 8):                    "not a supported file type",
		writeUploadFile(t, "bitmap.bmp", 8):                    "not a supported file type",
		writeUploadFile(t, "empty.png", 0):                     "is empty",
		dir:                                                    "is a directory",
	}
	if err := host.ValidateUserAsset(filepath.Join(dir, "missing.png")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ValidateUserAsset(missing.png) = %v, want a not-exist error", err)
	}
	for path, want := range rejected {
		if err := host.ValidateUserAsset(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateUserAsset(%s) = %v, want error containing %q", filepath.Base(path), err, want)
		}
	}
}
