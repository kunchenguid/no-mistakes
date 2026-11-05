package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGitlabEvidenceSectionUploadsOnceAndRewritesOneSection(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "01RUN")
	if err := os.MkdirAll(filepath.Join(runDir, "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "images-default.png"), []byte("png-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "notes.txt"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "review", "secret.png"), []byte("png-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	uploader := &countingGitlabUploader{markdown: "![images-default](/uploads/abc/images-default.png)", url: "/uploads/abc/images-default.png"}
	step := &PRStep{gitlabUploader: uploader}
	sctx := &pipeline.StepContext{
		Ctx:         context.Background(),
		EvidenceDir: runDir,
		Repo:        &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"},
		DB:          database,
		Config:      &config.Config{Test: config.Test{Evidence: config.Evidence{GitLabAttachMedia: true}}},
		Log:         func(string) {},
	}

	first := step.gitlabEvidenceSection(sctx, scm.ProviderGitLab)
	if uploader.calls != 1 {
		t.Fatalf("uploads = %d, want 1", uploader.calls)
	}
	if strings.Count(first, "## Evidence") != 1 || !strings.Contains(first, "`01RUN/images-default.png`") || !strings.Contains(first, uploader.markdown) {
		t.Fatalf("section = %q", first)
	}
	if strings.Contains(first, "secret.png") || strings.Contains(first, "notes.txt") {
		t.Fatalf("section included a non-image or the review conversation: %s", first)
	}

	second := step.gitlabEvidenceSection(sctx, scm.ProviderGitLab)
	if uploader.calls != 1 {
		t.Fatalf("second round uploads = %d, want the cached markdown", uploader.calls)
	}
	if strings.Count(second, "## Evidence") != 1 || second != first {
		t.Fatalf("second section = %q, want one unchanged section", second)
	}
	rewritten := stripGeneratedSections(first + "\n\n## Evidence\n\nold\n\n" + second)
	if strings.Contains(rewritten, "## Evidence") || strings.Contains(rewritten, "images-default") {
		t.Fatalf("strip left an Evidence section:\n%s", rewritten)
	}

	sctx.Repo.UpstreamURL = "https://another.example.com/group/project.git"
	step.gitlabEvidenceSection(sctx, scm.ProviderGitLab)
	if uploader.calls != 2 {
		t.Fatalf("uploads across hosts = %d, want 2", uploader.calls)
	}

	if got := step.gitlabEvidenceSection(sctx, scm.ProviderGitHub); got != "" || uploader.calls != 2 {
		t.Fatalf("GitHub section = %q uploads %d, want none", got, uploader.calls)
	}
}

func TestAppendGeneratedSectionsKeepsOneEvidenceSection(t *testing.T) {
	body := "## What Changed\n\n- updated\n\n## Evidence\n\nold image\n"
	testing := "## Testing\n\n- ok\n\n## Evidence\n\n`01RUN/shot.png`\n\n![shot](/uploads/abc/shot.png)"
	got := appendGeneratedSections(body, "low", testing, "## Pipeline\n\nrecorded", scm.ProviderGitLab)
	if strings.Count(got, "## Evidence") != 1 || strings.Contains(got, "old image") {
		t.Fatalf("rewritten body =\n%s", got)
	}
}

type countingGitlabUploader struct {
	calls    int
	markdown string
	url      string
	err      error
}

func (u *countingGitlabUploader) UploadProjectFile(context.Context, string) (gitlab.ProjectUpload, error) {
	u.calls++
	return gitlab.ProjectUpload{Markdown: u.markdown, URL: u.url}, u.err
}

func TestGitlabEvidenceAttachMediaGatesUploads(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "shot.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(filepath.Join(t.TempDir(), "missing-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	attach := true
	detach := false
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want int
	}{
		{name: "configuration unavailable"},
		{name: "missing configuration", cfg: config.Merge(global, &config.RepoConfig{})},
		{name: "empty configuration", cfg: config.Merge(&config.GlobalConfig{}, &config.RepoConfig{})},
		{name: "global disabled", cfg: config.Merge(&config.GlobalConfig{Test: config.TestRaw{Evidence: config.EvidenceRaw{AttachMedia: &detach}}}, &config.RepoConfig{})},
		{name: "repository disabled", cfg: config.Merge(&config.GlobalConfig{}, &config.RepoConfig{Test: config.TestRaw{Evidence: config.EvidenceRaw{AttachMedia: &detach}}})},
		{name: "global enabled", cfg: config.Merge(&config.GlobalConfig{Test: config.TestRaw{Evidence: config.EvidenceRaw{AttachMedia: &attach}}}, &config.RepoConfig{}), want: 1},
		{name: "repository disables global enable", cfg: config.Merge(&config.GlobalConfig{Test: config.TestRaw{Evidence: config.EvidenceRaw{AttachMedia: &attach}}}, &config.RepoConfig{Test: config.TestRaw{Evidence: config.EvidenceRaw{AttachMedia: &detach}}})},
		{name: "explicitly enabled", cfg: config.Merge(&config.GlobalConfig{}, &config.RepoConfig{Test: config.TestRaw{Evidence: config.EvidenceRaw{AttachMedia: &attach}}}), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploader := &countingGitlabUploader{markdown: "![shot](/uploads/abc/shot.png)", url: "/uploads/abc/shot.png"}
			step := &PRStep{gitlabUploader: uploader}
			got := step.gitlabEvidenceSection(&pipeline.StepContext{
				Ctx:         context.Background(),
				EvidenceDir: runDir,
				Repo:        &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"},
				Config:      tc.cfg,
				Log:         func(string) {},
			}, scm.ProviderGitLab)
			if uploader.calls != tc.want {
				t.Fatalf("uploads = %d, want %d", uploader.calls, tc.want)
			}
			if tc.want == 0 && got != "" {
				t.Fatalf("disabled section = %q, want empty", got)
			}
			if tc.want == 1 && !strings.Contains(got, uploader.markdown) {
				t.Fatalf("enabled section = %q, want uploaded image", got)
			}
		})
	}
}

func TestGitlabEvidenceReportsUnreadableImages(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ok.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(runDir, "blocked.png")
	if err := os.WriteFile(blocked, []byte("png"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o644) })
	if _, err := os.ReadFile(blocked); err == nil {
		t.Skip("file permissions do not deny reads on this platform")
	} else if !os.IsPermission(err) {
		t.Fatal(err)
	}
	hidden := filepath.Join(runDir, "hidden")
	if err := os.Mkdir(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "inside.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hidden, 0o755) })
	if _, err := os.ReadDir(hidden); err == nil {
		t.Skip("directory permissions do not deny reads on this platform")
	} else if !os.IsPermission(err) {
		t.Fatal(err)
	}
	var logs []string
	uploader := &countingGitlabUploader{markdown: "![ok](/uploads/abc/ok.png)", url: "/uploads/abc/ok.png"}
	step := &PRStep{gitlabUploader: uploader}
	got := step.gitlabEvidenceSection(&pipeline.StepContext{
		Ctx:         context.Background(),
		EvidenceDir: runDir,
		Repo:        &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"},
		Config:      &config.Config{Test: config.Test{Evidence: config.Evidence{GitLabAttachMedia: true}}},
		Log:         func(line string) { logs = append(logs, line) },
	}, scm.ProviderGitLab)
	if uploader.calls != 1 {
		t.Fatalf("uploads = %d, want the readable image only", uploader.calls)
	}
	if !strings.Contains(got, "run/blocked.png") || !strings.Contains(got, "not uploaded:") || !strings.Contains(got, "run/hidden") {
		t.Fatalf("section dropped an unreadable path:\n%s", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "unreadable") {
		t.Fatalf("logs = %q, want the directory or file read failure", logs)
	}
}

func TestGitlabEvidenceAttemptsEveryImageAndReportsRejections(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 51; i++ {
		if err := os.WriteFile(filepath.Join(runDir, fmt.Sprintf("%02d.png", i)), []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	large := filepath.Join(runDir, "large.png")
	if err := os.WriteFile(large, make([]byte, 11*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	uploader := &countingGitlabUploader{err: errors.New("attachment rejected by server")}
	step := &PRStep{gitlabUploader: uploader}
	sctx := &pipeline.StepContext{Ctx: context.Background(), EvidenceDir: runDir, Repo: &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"}, Config: &config.Config{Test: config.Test{Evidence: config.Evidence{GitLabAttachMedia: true}}}, Log: func(string) {}}
	got := step.gitlabEvidenceSection(sctx, scm.ProviderGitLab)
	if uploader.calls != 52 {
		t.Fatalf("upload attempts = %d, want every one of 52 images", uploader.calls)
	}
	if strings.Count(got, "not uploaded: attachment rejected by server") != 52 || !strings.Contains(got, "run/large.png") {
		t.Fatalf("missing rejected evidence entries: %s", got)
	}
}

func TestGitlabEvidenceIndependentOfTestingInEveryAppendixMode(t *testing.T) {
	sctx, _ := newEvidencePublishContext(t, "feature/gitlab-evidence")
	sctx.Repo.UpstreamURL = "https://gitlab.example.com/group/project.git"
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	sctx.Config.Test.Evidence.StoreInRepo = false
	sctx.Config.Test.Evidence.GitLabAttachMedia = true
	sctx.EvidenceDir = filepath.Join(t.TempDir(), sctx.Run.ID)
	if err := os.Mkdir(sctx.EvidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sctx.EvidenceDir, "shot.png"), []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploader := &countingGitlabUploader{markdown: "![shot](/uploads/abc/shot.png)", url: "/uploads/abc/shot.png"}
	step := &PRStep{gitlabUploader: uploader}
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		t.Run(mode, func(t *testing.T) {
			sctx.Config.PR.Appendix = mode
			pipelineMD, risk, testing, evidence := step.buildPipelineSection(sctx, scm.ProviderGitLab)
			ordinary := assembleDraftPRBody(sctx, "## What Changed\n\nUpdated", risk, testing, pipelineMD, evidence, 0, scm.ProviderGitLab)
			limited := assembleDraftPRBody(sctx, "## What Changed\n\nUpdated", risk, "## Testing\n\n"+strings.Repeat("verbose test log\n", 200), pipelineMD, evidence, 1200, scm.ProviderGitLab)
			if scm.PRBodyLen(limited) > 1200 {
				t.Fatal("evidence reservation exceeded body budget")
			}
			fallback, err := step.fallbackPRContent(sctx, "A\tfile.go", risk, testing, pipelineMD, evidence, 0, scm.ProviderGitLab)
			if err != nil {
				t.Fatal(err)
			}
			appendix, err := step.buildPRAppendix(sctx, scm.ProviderGitLab)
			if err != nil {
				t.Fatal(err)
			}
			owned, err := composeOwnedPRContent(prOwnedBody{before: "Author narrative"}, "Title", appendix, 0, sctx.WorkDir, "")
			if err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"ordinary": ordinary, "limited": limited, "fallback": fallback.Body, "owned": owned.Body} {
				if strings.Count(body, "## Evidence") != 1 || !strings.Contains(body, uploader.markdown) || !strings.Contains(body, sctx.Run.ID+"/shot.png") {
					t.Fatalf("%s omitted evidence: %s", name, body)
				}
			}
		})
	}
}

// TestGitlabEvidenceLinkBaseListsImagesAsLinksWithoutUploading: with uploads
// off and test.evidence.link_base_url set, the Evidence section is still
// written, every image is a link under the base, and nothing is uploaded.
func TestGitlabEvidenceLinkBaseListsImagesAsLinksWithoutUploading(t *testing.T) {
	t.Parallel()
	runDir := filepath.Join(t.TempDir(), "01RUN")
	if err := os.MkdirAll(filepath.Join(runDir, "browser"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shot.png", filepath.Join("browser", "step 2.png"), "notes.txt"} {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var logs []string
	uploader := &countingGitlabUploader{markdown: "![shot](/uploads/abc/shot.png)", url: "/uploads/abc/shot.png"}
	step := &PRStep{gitlabUploader: uploader}
	got := step.gitlabEvidenceSection(&pipeline.StepContext{
		Ctx:         context.Background(),
		EvidenceDir: runDir,
		Repo:        &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"},
		Config:      &config.Config{Test: config.Test{Evidence: config.Evidence{LinkBaseURL: evidenceLinkTestBase}}},
		Log:         func(line string) { logs = append(logs, line) },
	}, scm.ProviderGitLab)
	if uploader.calls != 0 {
		t.Fatalf("uploads = %d, want none", uploader.calls)
	}
	want := "## Evidence\n\n[01RUN/browser/step 2.png](" + evidenceLinkTestBase + "/01RUN/browser/step%202.png)\n\n[01RUN/shot.png](" + evidenceLinkTestBase + "/01RUN/shot.png)"
	if got != want {
		t.Fatalf("section = %q, want %q", got, want)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "without uploading") {
		t.Fatalf("logs = %q, want the link-mode line", logs)
	}
}

// TestGitlabEvidenceLinkBaseWithUploadsLinksThePathMention: with uploads on,
// the base does not change what is uploaded; it turns each entry's path
// mention into the link, including an entry whose upload was rejected.
func TestGitlabEvidenceLinkBaseWithUploadsLinksThePathMention(t *testing.T) {
	t.Parallel()
	runDir := filepath.Join(t.TempDir(), "01RUN")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "shot.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := func() *pipeline.StepContext {
		return &pipeline.StepContext{
			Ctx:         context.Background(),
			EvidenceDir: runDir,
			Repo:        &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"},
			Config:      &config.Config{Test: config.Test{Evidence: config.Evidence{GitLabAttachMedia: true, LinkBaseURL: evidenceLinkTestBase}}},
			Log:         func(string) {},
		}
	}
	mention := "[01RUN/shot.png](" + evidenceLinkTestBase + "/01RUN/shot.png)"

	uploader := &countingGitlabUploader{markdown: "![shot](/uploads/abc/shot.png)", url: "/uploads/abc/shot.png"}
	got := (&PRStep{gitlabUploader: uploader}).gitlabEvidenceSection(sctx(), scm.ProviderGitLab)
	if uploader.calls != 1 {
		t.Fatalf("uploads = %d, want 1", uploader.calls)
	}
	if want := "## Evidence\n\n" + mention + "\n\n" + uploader.markdown; got != want {
		t.Fatalf("section = %q, want %q", got, want)
	}

	rejecting := &countingGitlabUploader{err: errors.New("413 attachment too large")}
	got = (&PRStep{gitlabUploader: rejecting}).gitlabEvidenceSection(sctx(), scm.ProviderGitLab)
	if want := "## Evidence\n\n" + mention + " - not uploaded: 413 attachment too large"; got != want {
		t.Fatalf("rejected section = %q, want %q", got, want)
	}
}

// TestGitlabEvidenceLinkOnlyModeReadsNoImageBytes: listing links needs file
// names only, so an image the step cannot read is still listed as a link and
// the walk records no digest; the upload walk still reports the same file.
func TestGitlabEvidenceLinkOnlyModeReadsNoImageBytes(t *testing.T) {
	t.Parallel()
	runDir := filepath.Join(t.TempDir(), "01RUN")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(runDir, "blocked.png")
	if err := os.WriteFile(blocked, []byte("png"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o644) })
	if _, err := os.ReadFile(blocked); err == nil {
		t.Skip("file permissions do not deny reads on this platform")
	} else if !os.IsPermission(err) {
		t.Fatal(err)
	}

	listed, problems, err := gitlabEvidenceImages(runDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 || len(listed) != 1 || listed[0].digest != "" {
		t.Fatalf("link-only walk = %+v, problems %+v; want one undigested image and no read", listed, problems)
	}
	hashed, problems, err := gitlabEvidenceImages(runDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashed) != 0 || len(problems) != 1 {
		t.Fatalf("upload walk = %+v, problems %+v; want the unreadable file reported", hashed, problems)
	}

	uploader := &countingGitlabUploader{markdown: "![blocked](/uploads/abc/blocked.png)", url: "/uploads/abc/blocked.png"}
	got := (&PRStep{gitlabUploader: uploader}).gitlabEvidenceSection(&pipeline.StepContext{
		Ctx:         context.Background(),
		EvidenceDir: runDir,
		Repo:        &db.Repo{UpstreamURL: "https://gitlab.example.com/group/project.git"},
		Config:      &config.Config{Test: config.Test{Evidence: config.Evidence{LinkBaseURL: evidenceLinkTestBase}}},
		Log:         func(string) {},
	}, scm.ProviderGitLab)
	if want := "## Evidence\n\n[01RUN/blocked.png](" + evidenceLinkTestBase + "/01RUN/blocked.png)"; got != want || uploader.calls != 0 {
		t.Fatalf("section = %q (uploads %d), want %q with no upload", got, uploader.calls, want)
	}
}
