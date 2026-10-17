package steps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_GitLabMediaRendersAreIdempotentAndTrackChangedContents(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	sctx.Repo.UpstreamURL = "https://gitlab.example/group/sub/widgets.git"
	image := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.avif", []byte("image-v1"))
	video := writeEvidenceFile(t, sctx.EvidenceDir, "walk.MP4", []byte("video"))
	text := writeEvidenceFile(t, sctx.EvidenceDir, "checks.txt", []byte("checks passed\n"))
	missing := filepath.Join(sctx.EvidenceDir, "gone.png")
	worktreeImage := writeEvidenceFile(t, sctx.WorkDir, "not-evidence.png", []byte("not-evidence"))
	findings := fmt.Sprintf(`{"findings":[],"testing_summary":"Evidence collected.","artifacts":[{"label":"Checkout","path":%q},{"label":"Again","path":%q},{"label":"Walkthrough","kind":"video","path":%q},{"label":"Checks","kind":"log","path":%q},{"label":"Missing","path":%q},{"label":"Worktree image","path":%q}]}`, image, image, video, text, missing, worktreeImage)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	imageMD := "![Checkout](/uploads/image-v1/checkout.avif)"
	videoMD := "![Walkthrough](/uploads/video/walk.MP4)"
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.avif": "/uploads/image-v1/checkout.avif", "walk.MP4": "/uploads/video/walk.MP4"}}
	step := &PRStep{mediaUploader: uploader}
	first, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitLab, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitLab, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Body != second.Body || len(uploader.calls) != 2 {
		t.Fatalf("unchanged evidence must produce the same body without reupload; calls=%v", uploader.calls)
	}
	for _, expected := range []string{imageMD, videoMD, "checks passed", "gone.png", "not-evidence.png"} {
		if !strings.Contains(second.Body, expected) {
			t.Fatalf("missing %q in body:\n%s", expected, second.Body)
		}
	}
	if strings.Contains(second.Body, "<code>"+image+"</code>") || strings.Contains(second.Body, "<code>"+video+"</code>") {
		t.Fatalf("uploaded media must not retain local references:\n%s", second.Body)
	}
	// Same path and size, different contents: neither path nor size alone may
	// authorize reuse of the old screenshot.
	writeEvidenceFile(t, sctx.EvidenceDir, "checkout.avif", []byte("image-v2"))
	newMD := "![Checkout](/uploads/image-v2/checkout.avif)"
	uploader.urls["checkout.avif"] = "/uploads/image-v2/checkout.avif"
	third, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitLab, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 3 || !strings.Contains(third.Body, newMD) || strings.Contains(third.Body, imageMD) {
		t.Fatalf("changed screenshot reused stale evidence; calls=%v body:\n%s", uploader.calls, third.Body)
	}
}

func TestPRStep_GitLabUploadFailureKeepsLocalReferenceAndReportsReason(t *testing.T) {
	t.Parallel()
	uploader := &stubMediaUploader{err: errors.New("GitLab project upload failed")}
	body, logs := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		ctx.provider = scm.ProviderGitLab
		ctx.Repo.UpstreamURL = "https://gitlab.example/group/widgets.git"
	})
	if !strings.Contains(body, "local file:") || strings.Contains(body, "/uploads/") {
		t.Fatalf("failed upload must retain local evidence, got:\n%s", body)
	}
	if !strings.Contains(logs, "GitLab project upload failed") {
		t.Fatalf("failure was not reported: %s", logs)
	}
}

func TestPRStep_GitLabMediaHonorsPublicationOptOutAndStoreInRepo(t *testing.T) {
	t.Parallel()
	for _, store := range []bool{false, true} {
		t.Run(fmt.Sprintf("store_in_repo=%v", store), func(t *testing.T) {
			markdown := "![Checkout screenshot](/uploads/abc/checkout.png)"
			uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": "/uploads/abc/checkout.png"}}
			body, _ := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
				ctx.provider = scm.ProviderGitLab
				ctx.Repo.UpstreamURL = "https://gitlab.example/group/widgets.git"
				ctx.Config.Test.Evidence.AttachMedia = false
				ctx.Config.Test.Evidence.StoreInRepo = store
			})
			if store && (!strings.Contains(body, markdown) || len(uploader.calls) != 1) {
				t.Fatalf("store_in_repo must imply attachment upload; calls=%v body:\n%s", uploader.calls, body)
			}
			if !store && (!strings.Contains(body, "local file:") || len(uploader.calls) != 0) {
				t.Fatalf("publication opt-out ignored; calls=%v body:\n%s", uploader.calls, body)
			}
		})
	}
}

func TestPRStep_GitLabNeverUploadsSymbolicLinks(t *testing.T) {
	t.Parallel()
	uploader := &stubMediaUploader{t: t}
	body, logs := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		ctx.provider = scm.ProviderGitLab
		ctx.Repo.UpstreamURL = "https://gitlab.example/group/widgets.git"
		target := writeEvidenceFile(t, ctx.EvidenceDir, "target.png", []byte("private"))
		if err := os.Remove(ctx.png); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, ctx.png); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})
	if len(uploader.calls) != 0 || !strings.Contains(body, "local file:") {
		t.Fatalf("symlink must retain local rendering without upload; calls=%v body:\n%s", uploader.calls, body)
	}
	if !strings.Contains(logs, "symbolic links") {
		t.Fatalf("symlink refusal not reported: %s", logs)
	}
}
