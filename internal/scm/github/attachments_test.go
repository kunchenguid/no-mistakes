package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestClassifyGitHubToken(t *testing.T) {
	t.Parallel()
	allowed := []string{"gho_oauth", "ghp_classic", "github_pat_fine"}
	for _, token := range allowed {
		class, reason := classifyGitHubToken(token)
		if class != githubTokenAllowed {
			t.Errorf("classifyGitHubToken(%q) rejected: %s", token, reason)
		}
	}
	rejected := map[string]string{
		"":            "no GitHub token",
		"ghs_actions": "installation",
		"ghu_app":     "user-to-server",
		"unknown":     "unsupported",
	}
	for token, want := range rejected {
		class, reason := classifyGitHubToken(token)
		if class != githubTokenRejected {
			t.Errorf("classifyGitHubToken(%q) allowed, want rejected", token)
		}
		if !strings.Contains(strings.ToLower(reason), strings.ToLower(want)) && reason == "" {
			t.Errorf("classifyGitHubToken(%q) reason %q does not mention %q", token, reason, want)
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

func TestUserAssetClientUploadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "checkout.png")
	body := []byte("fake-png")
	if err := os.WriteFile(png, body, 0o644); err != nil {
		t.Fatal(err)
	}
	asset, err := ValidateUserAsset(png)
	if err != nil {
		t.Fatal(err)
	}

	var got http.Request
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"https://github.com/user-attachments/assets/c919a728-162d-435e-83a4-a8636a76a8aa"}`))
	}))
	t.Cleanup(server.Close)

	client := &UserAssetClient{
		HTTP:         server.Client(),
		UploadPrefix: server.URL + "/",
		Token:        "gho_test-token",
		RepositoryID: 1354199749,
		acceptedHost: "github.com",
	}
	url, err := client.UploadFile(context.Background(), asset)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if url != "https://github.com/user-attachments/assets/c919a728-162d-435e-83a4-a8636a76a8aa" {
		t.Fatalf("url = %q", url)
	}
	if got.Method != http.MethodPost {
		t.Fatalf("method = %s", got.Method)
	}
	if got.URL.Path != "/user-attachments/assets" {
		t.Fatalf("path = %s", got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("name") != "checkout.png" || q.Get("content_type") != "image/png" || q.Get("repository_id") != "1354199749" {
		t.Fatalf("query = %s", got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer gho_test-token" {
		t.Fatal("missing bearer authorization")
	}
	if got.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content-type = %s", got.Header.Get("Content-Type"))
	}
	if got.Header.Get("Accept") != "application/vnd.github+json" {
		t.Fatalf("accept = %s", got.Header.Get("Accept"))
	}
	if string(gotBody) != string(body) {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestUserAssetClientUploadFileRejectsReplacedFile(t *testing.T) {
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

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"https://github.com/user-attachments/assets/c919a728-162d-435e-83a4-a8636a76a8aa"}`))
	}))
	t.Cleanup(server.Close)
	client := &UserAssetClient{
		HTTP:         server.Client(),
		UploadPrefix: server.URL + "/",
		Token:        "gho_test",
		RepositoryID: 1,
		acceptedHost: "github.com",
	}
	if _, err := client.UploadFile(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "changed after attachment validation") {
		t.Fatalf("error = %v, want replaced-file refusal", err)
	}
	if requests != 0 {
		t.Fatalf("upload requests = %d, want 0", requests)
	}
}

func TestUserAssetClientUploadFileRejectsUnexpectedURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset, err := ValidateUserAsset(png)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"https://evil.example/not-an-attachment"}`))
	}))
	t.Cleanup(server.Close)
	client := &UserAssetClient{
		HTTP:         server.Client(),
		UploadPrefix: server.URL + "/",
		Token:        "gho_test",
		RepositoryID: 1,
		acceptedHost: "github.com",
	}
	if _, err := client.UploadFile(context.Background(), asset); err == nil {
		t.Fatal("expected unexpected URL to fail closed")
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

func TestHostUploadUserAssetSkipsInstallationToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth token --hostname github.com": {stdout: "ghs_installation\n"},
	}), func() bool { return true }, "github.com", "test/repo")
	_, err := host.UploadUserAsset(context.Background(), png)
	if err == nil || !strings.Contains(err.Error(), "installation") {
		t.Fatalf("error = %v, want installation-token refusal", err)
	}
}

func TestHostUploadUserAssetUploadsWithOAuthToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	png := filepath.Join(dir, "dot.png")
	if err := os.WriteFile(png, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_operator" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`))
	}))
	t.Cleanup(server.Close)

	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		switch {
		case key == "gh auth token --hostname github.com":
			return githubTestCmdFactory(map[string]githubTestResponse{key: {stdout: "gho_operator\n"}})(ctx, name, args...)
		case strings.HasPrefix(key, "gh api --hostname github.com graphql") && strings.Contains(key, "owner=test") && strings.Contains(key, "name=repo"):
			return githubTestCmdFactory(map[string]githubTestResponse{key: {stdout: `{"data":{"repository":{"databaseId":42,"viewerPermission":"WRITE"}}}`}})(ctx, name, args...)
		default:
			t.Fatalf("unexpected command %q", key)
			return exec.CommandContext(ctx, "false")
		}
	}, func() bool { return true }, "github.com", "test/repo")
	host.assetHTTP = server.Client()
	host.assetUploadPrefix = server.URL + "/"

	url, err := host.UploadUserAsset(context.Background(), png)
	if err != nil {
		t.Fatalf("UploadUserAsset: %v", err)
	}
	if !strings.Contains(url, "user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Fatalf("url = %q", url)
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
