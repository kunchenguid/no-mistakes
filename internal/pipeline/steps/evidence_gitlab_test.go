package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
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

	if got := step.gitlabEvidenceSection(sctx, scm.ProviderGitHub); got != "" || uploader.calls != 1 {
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
}

func (u *countingGitlabUploader) UploadProjectFile(context.Context, string) (gitlab.ProjectUpload, error) {
	u.calls++
	return gitlab.ProjectUpload{Markdown: u.markdown, URL: u.url}, nil
}
