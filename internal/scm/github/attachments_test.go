package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSupportsUserAttachments(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":                  true,
		"github.com":        true,
		"GitHub.COM":        true,
		"github.localhost":  true,
		"contoso.ghe.com":   true,
		"ghe.com":           true,
		"ghe.example.com":   false,
		"github.enterprise": false,
		"gitlab.com":        false,
	}
	for host, want := range cases {
		if got := SupportsUserAttachments(host); got != want {
			t.Errorf("SupportsUserAttachments(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestValidateUserAsset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset, err := ValidateUserAsset(png)
	if err != nil {
		t.Fatalf("png: %v", err)
	}
	if asset.ContentType != "image/png" || asset.Video {
		t.Fatalf("png asset = %+v", asset)
	}

	txt := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txt, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateUserAsset(txt); err == nil || !strings.Contains(err.Error(), "not a supported file type") {
		t.Fatalf("txt error = %v, want unsupported type", err)
	}

	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateUserAsset(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty error = %v, want empty", err)
	}

	oversize := filepath.Join(dir, "oversize.png")
	if err := os.WriteFile(oversize, make([]byte, maxUserAssetImageBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateUserAsset(oversize); err == nil || !strings.Contains(err.Error(), "images must be at most") {
		t.Fatalf("oversize error = %v, want image size limit", err)
	}

	mp4 := filepath.Join(dir, "clip.MP4")
	if err := os.WriteFile(mp4, []byte("ftyp"), 0o644); err != nil {
		t.Fatal(err)
	}
	video, err := ValidateUserAsset(mp4)
	if err != nil {
		t.Fatalf("mp4: %v", err)
	}
	if !video.Video || video.ContentType != "video/mp4" {
		t.Fatalf("mp4 asset = %+v", video)
	}
}

func TestHostUploadUserAssetKeepsCredentialInsideGHCLI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	const uploadURL = "https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	uploadEndpoint := "https://uploads.github.com/user-attachments/assets?content_type=image%2Fpng&name=dot.png&repository_id=42"
	uploadCommand := "gh api --hostname github.com --method POST --header Accept: application/vnd.github+json --header Content-Type: application/octet-stream --header Content-Length: 3 --input - " + uploadEndpoint
	commands := map[string]githubTestResponse{
		"gh auth token --hostname github.com": {
			stderr: "Automic Vault: Secret Disclosure is not permitted",
			code:   1,
		},
		"gh api --hostname github.com graphql -f query=" + userAssetRepoQuery + " -F owner=test -F name=repo": {
			stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`,
		},
		uploadCommand: {stdout: `{"url":"` + uploadURL + `"}`, wantStdin: "png"},
	}
	host := New(githubTestCmdFactory(commands), func() bool { return true }, "github.com", "test/repo")

	got, err := host.UploadUserAsset(context.Background(), png)
	if err != nil {
		t.Fatalf("UploadUserAsset: %v", err)
	}
	if got != uploadURL {
		t.Fatalf("URL = %q, want %q", got, uploadURL)
	}
}

func TestHostUploadUserAssetSkipsGHES(t *testing.T) {
	t.Parallel()
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("GHES must not call %s %s", name, strings.Join(args, " "))
		return exec.CommandContext(ctx, "false")
	}, func() bool { return true }, "ghe.example.com", "ghe.example.com/test/repo")
	if _, err := host.UploadUserAsset(context.Background(), "dot.png"); err == nil || !strings.Contains(err.Error(), "Enterprise Server") {
		t.Fatalf("error = %v, want GHES refusal", err)
	}
}

func TestHostUploadUserAssetScopesCLIToGHECHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	const hostName = "acme.ghe.com"
	const uploadURL = "https://acme.ghe.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	uploadEndpoint := "https://uploads.acme.ghe.com/user-attachments/assets?content_type=image%2Fpng&name=dot.png&repository_id=42"
	commands := map[string]githubTestResponse{
		"gh api --hostname " + hostName + " graphql -f query=" + userAssetRepoQuery + " -F owner=test -F name=repo": {
			stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`,
		},
		"gh api --hostname " + hostName + " --method POST --header Accept: application/vnd.github+json --header Content-Type: application/octet-stream --header Content-Length: 3 --input - " + uploadEndpoint: {
			stdout: `{"url":"` + uploadURL + `"}`, wantStdin: "png",
		},
	}
	host := New(githubTestCmdFactory(commands), func() bool { return true }, hostName, hostName+"/test/repo")
	got, err := host.UploadUserAsset(context.Background(), png)
	if err != nil {
		t.Fatalf("UploadUserAsset: %v", err)
	}
	if got != uploadURL {
		t.Fatalf("URL = %q, want %q", got, uploadURL)
	}
}

func TestHostUploadUserAssetDistinguishesCLIAndResponseFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	lookup := "gh api --hostname github.com graphql -f query=" + userAssetRepoQuery + " -F owner=test -F name=repo"
	upload := "gh api --hostname github.com --method POST --header Accept: application/vnd.github+json --header Content-Type: application/octet-stream --header Content-Length: 3 --input - https://uploads.github.com/user-attachments/assets?content_type=image%2Fpng&name=dot.png&repository_id=42"

	t.Run("repository authentication failure", func(t *testing.T) {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			lookup: {stderr: "authentication required", code: 1},
		}), func() bool { return true }, "github.com", "test/repo")
		_, err := host.UploadUserAsset(context.Background(), png)
		if err == nil || !strings.Contains(err.Error(), "gh api repository for user-attachments: authentication required") {
			t.Fatalf("error = %v, want repository authentication failure", err)
		}
	})

	t.Run("repository malformed response", func(t *testing.T) {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			lookup: {stdout: "Paste your token: "},
		}), func() bool { return true }, "github.com", "test/repo")
		_, err := host.UploadUserAsset(context.Background(), png)
		if err == nil || !strings.Contains(err.Error(), "parse repository for user-attachments") {
			t.Fatalf("error = %v, want malformed repository response", err)
		}
	})

	t.Run("upload authentication failure", func(t *testing.T) {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			lookup: {stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`},
			upload: {stderr: "authentication required", code: 1, wantStdin: "png"},
		}), func() bool { return true }, "github.com", "test/repo")
		_, err := host.UploadUserAsset(context.Background(), png)
		if err == nil || !strings.Contains(err.Error(), "gh api user-attachments upload: authentication required") {
			t.Fatalf("error = %v, want upload authentication failure", err)
		}
	})

	t.Run("ambiguous upload rejection", func(t *testing.T) {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			lookup: {stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`},
			upload: {stderr: "gh: Not Found (HTTP 404)", code: 1, wantStdin: "png"},
		}), func() bool { return true }, "github.com", "test/repo")
		_, err := host.UploadUserAsset(context.Background(), png)
		if err == nil || !strings.Contains(err.Error(), "repository write access") || !strings.Contains(err.Error(), "credential type supported") {
			t.Fatalf("error = %v, want both possible HTTP 404 causes", err)
		}
	})

	t.Run("unexpected prompt text", func(t *testing.T) {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			lookup: {stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`},
			upload: {stdout: "Paste your token: ", wantStdin: "png"},
		}), func() bool { return true }, "github.com", "test/repo")
		_, err := host.UploadUserAsset(context.Background(), png)
		if err == nil || !strings.Contains(err.Error(), "parse gh api user-attachments response") {
			t.Fatalf("error = %v, want malformed response failure", err)
		}
	})

	t.Run("unexpected response URL", func(t *testing.T) {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			lookup: {stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`},
			upload: {stdout: `{"url":"https://evil.example/not-an-attachment"}`, wantStdin: "png"},
		}), func() bool { return true }, "github.com", "test/repo")
		_, err := host.UploadUserAsset(context.Background(), png)
		if err == nil || !strings.Contains(err.Error(), "not a GitHub attachments host") {
			t.Fatalf("error = %v, want unexpected URL failure", err)
		}
	})
}

func TestUploadUserAssetRejectsFileReplacedAfterValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "checkout.png")
	if err := os.WriteFile(png, []byte("safe-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset, err := ValidateUserAsset(png)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.png")
	if err := os.WriteFile(other, []byte("private!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(png); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, png); err != nil {
		t.Fatal(err)
	}
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("replaced file must not invoke %s %s", name, strings.Join(args, " "))
		return exec.CommandContext(ctx, "false")
	}, func() bool { return true }, "github.com", "test/repo")
	if _, err := host.uploadUserAsset(context.Background(), asset, 42); err == nil || !strings.Contains(err.Error(), "changed after attachment validation") {
		t.Fatalf("error = %v, want replaced-file refusal", err)
	}
}

func TestGH2990IssueCreateHelpDocumentsAttach(t *testing.T) {
	gh := localGH2990()
	if gh == "" {
		t.Skip("worktree-local gh 2.99.0 is not present")
	}
	out, err := exec.Command(gh, "issue", "create", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("gh issue create --help: %v\n%s", err, out)
	}
	help := string(out)
	for _, want := range []string{"--attach", "image or video", "You can attach up to 50 files"} {
		if !strings.Contains(help, want) {
			t.Errorf("gh 2.99.0 issue create help missing %q", want)
		}
	}
	if strings.Contains(help, "GitHub Enterprise Server is not supported") {
		// Help for issue create does not need to repeat the GHES trap; the
		// upload client owns that. This assertion documents that we do not
		// treat help prose as the GHES contract.
	}
	version, err := exec.Command(gh, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("gh --version: %v", err)
	}
	if !strings.Contains(string(version), "gh version 2.99.0") {
		t.Fatalf("local gh is %s, want 2.99.0", version)
	}
}

func localGH2990() string {
	if p := strings.TrimSpace(os.Getenv("NM_TEST_GH_2_99_0")); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	p := filepath.Join(root, ".scratch-gh", "gh_2.99.0_macOS_arm64", "bin", "gh")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}
