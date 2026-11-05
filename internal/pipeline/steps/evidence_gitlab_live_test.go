package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// TestLiveGitLabEvidenceSectionOnThrowawayMR uploads a real image into the
// project named by NM_LIVE_GITLAB_PROJECT and checks a throwaway merge request.
// CI does not set the variable.
func TestLiveGitLabEvidenceSectionOnThrowawayMR(t *testing.T) {
	project := strings.TrimSpace(os.Getenv("NM_LIVE_GITLAB_PROJECT"))
	if project == "" {
		t.Skip("set NM_LIVE_GITLAB_PROJECT to run the live GitLab evidence check")
	}
	hostName := strings.TrimSpace(os.Getenv("NM_LIVE_GITLAB_HOST"))
	if hostName == "" {
		hostName = "gitlab.com"
	}
	ctx := context.Background()
	before := liveGitlabUploadCount(t, ctx, hostName, project)

	root := t.TempDir()
	runID := fmt.Sprintf("01LIVE%d", time.Now().Unix())
	runDir := filepath.Join(root, runID)
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "images-default.png"), tinyLivePNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "notes.txt"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	uploader := &countingLiveUploader{host: gitlab.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, name, args...)
	}, nil, hostName, project)}
	step := &PRStep{gitlabUploader: uploader}
	sctx := &pipeline.StepContext{
		Ctx:         ctx,
		EvidenceDir: runDir,
		Repo:        &db.Repo{UpstreamURL: "https://" + hostName + "/" + project + ".git"},
		DB:          database,
		Config:      &config.Config{Test: config.Test{Evidence: config.Evidence{GitLabAttachMedia: true}}},
		Log:         func(line string) { t.Log(line) },
	}

	first := step.gitlabEvidenceSection(sctx, scm.ProviderGitLab)
	if uploader.calls != 1 {
		t.Fatalf("first round uploads = %d, want 1", uploader.calls)
	}
	if strings.Count(first, "## Evidence") != 1 || !strings.Contains(first, runID+"/images-default.png") {
		t.Fatalf("first section = %q", first)
	}
	after := liveGitlabUploadCount(t, ctx, hostName, project)
	if after != before+1 {
		t.Fatalf("upload count = %d, want %d", after, before+1)
	}

	second := step.gitlabEvidenceSection(sctx, scm.ProviderGitLab)
	if uploader.calls != 1 {
		t.Fatalf("second round uploads = %d, want the cached upload", uploader.calls)
	}
	if strings.Count(second, "## Evidence") != 1 {
		t.Fatalf("second section = %q", second)
	}
	if again := liveGitlabUploadCount(t, ctx, hostName, project); again != after {
		t.Fatalf("upload count after the second round = %d, want %d", again, after)
	}

	stale := first + "\n\n## Evidence\n\nstale copy\n"
	rewritten := appendGeneratedSections("## What Changed\n\n- live check", "low", second, "## Pipeline\n\nrecorded", scm.ProviderGitLab)
	if strings.Contains(stripGeneratedSections(stale), "## Evidence") {
		t.Fatal("strip left the previous Evidence section")
	}
	mrURL := liveCreateEvidenceMR(t, ctx, hostName, project, rewritten)
	t.Logf("merge request %s", mrURL)
	liveUpdateEvidenceMR(t, ctx, hostName, project, mrURL, stale+"\n\n"+rewritten)
	// The product rewrites before it publishes. Send that rewrite, not the stale copy.
	published := appendGeneratedSections(stale, "low", second, "## Pipeline\n\nrecorded", scm.ProviderGitLab)
	liveUpdateEvidenceMR(t, ctx, hostName, project, mrURL, published)
	got := liveEvidenceMRDescription(t, ctx, hostName, project, mrURL)
	if strings.Count(got, "## Evidence") != 1 {
		t.Fatalf("description has %d Evidence sections:\n%s", strings.Count(got, "## Evidence"), got)
	}
	if !strings.Contains(got, runID+"/images-default.png") {
		t.Fatalf("description missing the relative path:\n%s", got)
	}
}

type countingLiveUploader struct {
	host  *gitlab.Host
	calls int
}

func (u *countingLiveUploader) UploadProjectFile(ctx context.Context, path string) (gitlab.ProjectUpload, error) {
	u.calls++
	return u.host.UploadProjectFile(ctx, path)
}

var tinyLivePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
	0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x05, 0xfe, 0x02, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func liveGitlab(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(ctx, "glab", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("glab %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr)
	}
	return out
}

func liveGitlabUploadCount(t *testing.T, ctx context.Context, hostName, project string) int {
	t.Helper()
	raw := liveGitlab(t, ctx, "api", "--hostname", hostName, "--paginate", "projects/"+gitlabEncode(project)+"/uploads")
	var page []json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &page); err != nil {
		t.Fatalf("decode uploads: %v\n%s", err, raw)
	}
	return len(page)
}

func liveCreateEvidenceMR(t *testing.T, ctx context.Context, hostName, project, body string) string {
	t.Helper()
	branch := fmt.Sprintf("nm-evidence-live-%d", time.Now().Unix())
	commitPath := filepath.Join(t.TempDir(), "commit.json")
	writeLiveJSON(t, commitPath, map[string]any{
		"branch":         branch,
		"start_branch":   "main",
		"commit_message": "throwaway evidence upload check",
		"actions": []map[string]string{{
			"action":    "create",
			"file_path": branch + ".txt",
			"content":   "throwaway",
		}},
	})
	liveGitlab(t, ctx, "api", "--hostname", hostName, "--method", "POST", "--header", "Content-Type: application/json", "--input", commitPath, "projects/"+gitlabEncode(project)+"/repository/commits")
	mrPath := filepath.Join(t.TempDir(), "mr.json")
	writeLiveJSON(t, mrPath, map[string]string{
		"source_branch": branch,
		"target_branch": "main",
		"title":         "throwaway evidence upload check",
		"description":   body,
	})
	raw := liveGitlab(t, ctx, "api", "--hostname", hostName, "--method", "POST", "--header", "Content-Type: application/json", "--input", mrPath, "projects/"+gitlabEncode(project)+"/merge_requests")
	var created struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &created); err != nil || created.WebURL == "" {
		t.Fatalf("create merge request: %v\n%s", err, raw)
	}
	return created.WebURL
}

func liveUpdateEvidenceMR(t *testing.T, ctx context.Context, hostName, project, mrURL, body string) {
	t.Helper()
	iid := mrURL[strings.LastIndex(mrURL, "/")+1:]
	path := filepath.Join(t.TempDir(), "update-"+iid+".json")
	writeLiveJSON(t, path, map[string]string{"description": body})
	liveGitlab(t, ctx, "api", "--hostname", hostName, "--method", "PUT", "--header", "Content-Type: application/json", "--input", path, "projects/"+gitlabEncode(project)+"/merge_requests/"+iid)
}

func liveEvidenceMRDescription(t *testing.T, ctx context.Context, hostName, project, mrURL string) string {
	t.Helper()
	iid := mrURL[strings.LastIndex(mrURL, "/")+1:]
	raw := liveGitlab(t, ctx, "api", "--hostname", hostName, "projects/"+gitlabEncode(project)+"/merge_requests/"+iid)
	var mr struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &mr); err != nil {
		t.Fatalf("read merge request: %v\n%s", err, raw)
	}
	return mr.Description
}

func writeLiveJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitlabEncode(project string) string {
	return strings.ReplaceAll(project, "/", "%2F")
}
