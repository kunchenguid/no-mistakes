package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testAttachmentURL = "https://github.com/user-attachments/assets/c919a728-162d-435e-83a4-a8636a76a8aa"

type stubMediaUploader struct {
	t     *testing.T
	urls  map[string]string
	err   error
	calls []string
}

func (s *stubMediaUploader) UploadUserAsset(_ context.Context, path string) (string, error) {
	s.calls = append(s.calls, path)
	if s.err != nil {
		return "", s.err
	}
	if url, ok := s.urls[filepath.Base(path)]; ok {
		return url, nil
	}
	if s.t != nil {
		s.t.Fatalf("unexpected upload of %s", path)
	}
	return "", fmt.Errorf("unexpected upload of %s", path)
}

func prDraftAgent() *mockAgent {
	return &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"feat: demo change","body":"## What Changed\n\n- demo"}`)}, nil
		},
	}
}

func enableDefaultEvidence(sctx *pipeline.StepContext) {
	sctx.Config.Test = config.Merge(&config.GlobalConfig{}, &config.RepoConfig{}).Test
}

func writeEvidenceFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func screenshotFindings(path string) string {
	return fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q}]}`, path)
}

func renderPRWithScreenshot(t *testing.T, uploader userAssetUploader, configure func(sctx *testPRAttachCtx)) (body, logs string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	png := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png-bytes"))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(png), "")
	var logLines []string
	sctx.Log = func(line string) { logLines = append(logLines, line) }
	ctx := &testPRAttachCtx{StepContext: sctx, png: png}
	if configure != nil {
		configure(ctx)
	}
	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", baseSHA, ctx.provider, 0)
	if err != nil {
		t.Fatal(err)
	}
	return content.Body, strings.Join(logLines, "\n")
}

type testPRAttachCtx struct {
	*pipeline.StepContext
	png      string
	provider scm.Provider
}

func TestPRStep_DefaultConfigEmbedsGitHubScreenshotAttachment(t *testing.T) {
	t.Parallel()
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
	body, _ := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		ctx.provider = scm.ProviderGitHub
	})
	if !strings.Contains(body, "![Checkout screenshot]("+testAttachmentURL+")") {
		t.Fatalf("expected GitHub user-attachments image, got:\n%s", body)
	}
	if strings.Contains(body, "local file:") {
		t.Fatalf("default-config screenshot must not cite a local path, got:\n%s", body)
	}
	if len(uploader.calls) != 1 {
		t.Fatalf("uploads = %d, want 1", len(uploader.calls))
	}
}

func TestPRStep_ReusesScreenshotAttachmentAcrossPRRenders(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	png := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png-bytes"))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(png), "")
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
	step := &PRStep{mediaUploader: uploader}

	first, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 1 {
		t.Fatalf("uploads across two renders = %d, want 1", len(uploader.calls))
	}
	for i, body := range []string{first.Body, second.Body} {
		if !strings.Contains(body, "![Checkout screenshot]("+testAttachmentURL+")") {
			t.Fatalf("render %d did not contain cached attachment:\n%s", i+1, body)
		}
	}
}

func TestPRStep_DeduplicatesScreenshotUploadsByPath(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	png := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png-bytes"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Desktop checkout","path":%q},{"kind":"screenshot","label":"Mobile checkout","path":%q}]}`, png, png)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}

	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 1 {
		t.Fatalf("uploads = %d, want 1", len(uploader.calls))
	}
	for _, label := range []string{"Desktop checkout", "Mobile checkout"} {
		if !strings.Contains(content.Body, "!["+label+"]("+testAttachmentURL+")") {
			t.Fatalf("expected attachment for %s, got:\n%s", label, content.Body)
		}
	}
}

func TestPRStep_StoreInRepoKeepsCommitLinkAndAttachment(t *testing.T) {
	t.Parallel()
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Agent = prDraftAgent()
	sctx.Config.Test.Evidence.AttachMedia = true
	png := writeEvidenceFile(t, testEvidenceDir(sctx), "checkout.png", []byte("\x89PNG binary"))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(png), "")
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}

	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/add-login", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	wantLink := "https://github.com/example/widgets/blob/" + tip + "/.no-mistakes/evidence/feature/add-login/checkout.png"
	if !strings.Contains(content.Body, "![Checkout screenshot]("+testAttachmentURL+")") {
		t.Fatalf("expected attachment embed, got:\n%s", content.Body)
	}
	if !strings.Contains(content.Body, "- Evidence: [Checkout screenshot]("+wantLink+")") {
		t.Fatalf("expected commit-pinned evidence link %q, got:\n%s", wantLink, content.Body)
	}
	if strings.Contains(content.Body, "local file:") {
		t.Fatalf("published screenshot must not cite a local path, got:\n%s", content.Body)
	}
}

func TestPRStep_UploadFailureKeepsTodaysRendering(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	pngDir := ""
	makeCtx := func(ag agent.Agent) *pipeline.StepContext {
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		enableDefaultEvidence(sctx)
		if pngDir == "" {
			pngDir = writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png-bytes"))
		} else {
			sctx.EvidenceDir = filepath.Dir(pngDir)
		}
		insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(pngDir), "")
		return sctx
	}

	disabled := makeCtx(prDraftAgent())
	disabled.Config.Test.Evidence.AttachMedia = false
	today, err := (&PRStep{}).buildPRContent(disabled, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}

	var logs []string
	failing := makeCtx(prDraftAgent())
	failing.Log = func(line string) { logs = append(logs, line) }
	failed, err := (&PRStep{mediaUploader: &stubMediaUploader{err: errors.New("upload endpoint 500")}}).buildPRContent(failing, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Body != today.Body {
		t.Fatalf("upload failure must keep today's body.\ntoday:\n%s\nfailed:\n%s", today.Body, failed.Body)
	}
	if !strings.Contains(today.Body, "local file:") {
		t.Fatalf("today's rendering should cite the local screenshot, got:\n%s", today.Body)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "upload endpoint 500") {
		t.Fatalf("expected logged upload reason, got %q", joined)
	}
}

func TestPRStep_NonGitHubForgeLeavesScreenshotUnchanged(t *testing.T) {
	t.Parallel()
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
	body, logs := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		ctx.provider = scm.ProviderGitLab
		ctx.Repo.UpstreamURL = "https://gitlab.com/example/widgets.git"
	})
	if strings.Contains(body, "user-attachments") {
		t.Fatalf("GitLab PR must not embed GitHub attachments, got:\n%s", body)
	}
	if !strings.Contains(body, "local file:") {
		t.Fatalf("GitLab screenshot should keep local rendering, got:\n%s", body)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("GitLab must not upload, calls=%v", uploader.calls)
	}
	if !strings.Contains(logs, "GitHub-only") {
		t.Fatalf("expected GitHub-only skip reason, got %q", logs)
	}
}

func TestPRStep_TextArtifactIsNotUploaded(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	logPath := writeEvidenceFile(t, sctx.EvidenceDir, "cli-run.txt", []byte("it works\n"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"CLI run","path":%q}]}`, logPath)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	uploader := &stubMediaUploader{t: t}
	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("text artifacts must not upload, calls=%v", uploader.calls)
	}
	if strings.Contains(content.Body, "user-attachments") {
		t.Fatalf("text artifact must not become a user-attachment, got:\n%s", content.Body)
	}
	if !strings.Contains(content.Body, "it works") {
		t.Fatalf("expected inlined text evidence, got:\n%s", content.Body)
	}
}

func TestPRStep_OversizedImageIsSkippedWithReason(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	png := writeEvidenceFile(t, sctx.EvidenceDir, "oversize.png", make([]byte, 10*1024*1024+1))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(png), "")
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	uploader := &stubMediaUploader{t: t}
	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("oversized image must not upload, calls=%v", uploader.calls)
	}
	if strings.Contains(content.Body, "user-attachments") {
		t.Fatalf("oversized image must not embed an attachment, got:\n%s", content.Body)
	}
	if !strings.Contains(content.Body, "local file:") {
		t.Fatalf("oversized image should keep local rendering, got:\n%s", content.Body)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "images must be at most") {
		t.Fatalf("expected size-limit skip reason, got %q", logs)
	}
}

func TestPRStep_GHESDoesNotUpload(t *testing.T) {
	t.Parallel()
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
	body, logs := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		ctx.provider = scm.ProviderGitHub
		ctx.Repo.UpstreamURL = "https://ghe.example.com/test/repo.git"
	})
	if strings.Contains(body, "user-attachments") {
		t.Fatalf("GHES must not embed user-attachments, got:\n%s", body)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("GHES must not upload, calls=%v", uploader.calls)
	}
	if !strings.Contains(logs, "Enterprise Server") {
		t.Fatalf("expected GHES skip reason, got %q", logs)
	}
}

func TestPRStep_OversizedImagePreservesCommittedPath(t *testing.T) {
	t.Parallel()
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Agent = prDraftAgent()
	png := writeEvidenceFile(t, testEvidenceDir(sctx), "oversize.png", make([]byte, 10*1024*1024+1))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(png), "")
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	uploader := &stubMediaUploader{t: t}

	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/add-login", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	wantLink := "https://github.com/example/widgets/blob/" + tip + "/.no-mistakes/evidence/feature/add-login/oversize.png"
	if !strings.Contains(content.Body, wantLink) {
		t.Fatalf("expected committed evidence path to be preserved, got:\n%s", content.Body)
	}
	if strings.Contains(content.Body, "user-attachments") {
		t.Fatalf("oversized image must not embed an attachment, got:\n%s", content.Body)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("oversized image must not upload, calls=%v", uploader.calls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "images must be at most") {
		t.Fatalf("expected size-limit skip reason, got %q", logs)
	}
}

func TestPRStep_UnsupportedImagePreservesCommittedPath(t *testing.T) {
	t.Parallel()
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Agent = prDraftAgent()
	image := writeEvidenceFile(t, testEvidenceDir(sctx), "checkout.bmp", []byte("bitmap"))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(image), "")
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	uploader := &stubMediaUploader{t: t}

	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/add-login", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	wantLink := "https://github.com/example/widgets/blob/" + tip + "/.no-mistakes/evidence/feature/add-login/checkout.bmp"
	if !strings.Contains(content.Body, wantLink) {
		t.Fatalf("expected committed evidence path to be preserved, got:\n%s", content.Body)
	}
	if strings.Contains(content.Body, "user-attachments") {
		t.Fatalf("unsupported image must not embed an attachment, got:\n%s", content.Body)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("unsupported image must not upload, calls=%v", uploader.calls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "not a supported file type") {
		t.Fatalf("expected unsupported-type skip reason, got %q", logs)
	}
}

func TestPRStep_VideoAttachmentIsBareURL(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	video := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.mp4", []byte("ftyp"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"video","label":"Checkout recording","path":%q}]}`, video)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.mp4": testAttachmentURL}}
	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Body, testAttachmentURL+"\n") {
		t.Fatalf("expected bare video URL, got:\n%s", content.Body)
	}
	if strings.Contains(content.Body, "![Checkout recording](") {
		t.Fatalf("video must not use image markdown, got:\n%s", content.Body)
	}
	if strings.Contains(content.Body, "local file:") {
		t.Fatalf("uploaded video must not cite a local path, got:\n%s", content.Body)
	}
}
