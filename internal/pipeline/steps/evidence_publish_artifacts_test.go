package steps

import (
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEvidencePublishing_WritesReviewerVisibleArtifacts exercises the end-user
// PR evidence surface and, when NO_MISTAKES_EVIDENCE_DIR is set, writes the
// generated PR markdown, published image bytes, and an HTML preview that
// reviewers can open to inspect durable GitHub blob URLs.
func TestEvidencePublishing_WritesReviewerVisibleArtifacts(t *testing.T) {
	evidenceDir := strings.TrimSpace(os.Getenv("NO_MISTAKES_EVIDENCE_DIR"))
	if evidenceDir == "" {
		t.Skip("set NO_MISTAKES_EVIDENCE_DIR to capture reviewer-visible evidence artifacts")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	repoRoot := t.TempDir()
	runID := "artifact-demo"
	location := resolveTestEvidenceLocation(repoRoot, "fm/demo-branch", runID, config.Evidence{StoreInRepo: true})
	if err := os.MkdirAll(location.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(location.Dir) })

	sourcePNG := filepath.Join(location.Dir, "checkout-ui.png")
	writeDemoPNG(t, sourcePNG, 640, 360)
	sourceLog := filepath.Join(location.Dir, "server.log")
	if err := os.WriteFile(sourceLog, []byte("POST /checkout 200\nreceipt=ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := types.Findings{
		TestingSummary: "Validated checkout UI and kept temporary text evidence.",
		Tested:         []string{"manual screenshot review", "PR body rendering"},
		Artifacts: []types.TestArtifact{
			{Kind: "screenshot", Label: "Checkout screenshot", Path: sourcePNG},
			{Kind: "log", Label: "Server log", Path: sourceLog, Content: "POST /checkout 200"},
			{Kind: "screenshot", Label: "Local temp leak candidate", Path: "/tmp/no-mistakes-evidence/secret-leak.png"},
			{Kind: "screenshot", Label: "Missing published image", Path: ".no-mistakes/evidence/.generated/demo/missing.png"},
		},
	}

	findings.Artifacts = prepareTestEvidenceArtifacts(repoRoot, location, findings.Artifacts)

	var published *types.TestArtifact
	for i := range findings.Artifacts {
		art := &findings.Artifacts[i]
		if art.Path != "" && art.SHA256 != "" && strings.HasSuffix(art.Path, ".png") && !filepath.IsAbs(art.Path) {
			// Push marks verified staged images published; simulate that gate here.
			art.Published = true
			published = art
			break
		}
	}
	if published == nil {
		t.Fatalf("expected a prepared image artifact, got %#v", findings.Artifacts)
	}

	// Stage prepared image bytes into the fixture commit so blob verification
	// can mint an immutable GitHub URL.
	publishedAbs := filepath.Join(repoRoot, filepath.FromSlash(published.Path))
	if _, err := os.Stat(publishedAbs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := commitPRFixture(t, repoRoot)

	raw, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatal(err)
	}
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &raw}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &raw}}}

	parentMD := BuildTestingSummaryForPR(steps, rounds, "https://github.com/parent-owner/widgets.git", ref, repoRoot)
	forkMD := BuildTestingSummaryForPR(steps, rounds, "https://github.com/fork-owner/widgets.git", ref, repoRoot)

	wantParent := fmt.Sprintf("![Validation screenshot 1](https://github.com/parent-owner/widgets/blob/%s/%s?raw=1)", ref, published.Path)
	wantFork := fmt.Sprintf("![Validation screenshot 1](https://github.com/fork-owner/widgets/blob/%s/%s?raw=1)", ref, published.Path)
	if !strings.Contains(parentMD, wantParent) {
		t.Fatalf("parent PR markdown missing durable URL %q:\n%s", wantParent, parentMD)
	}
	if !strings.Contains(forkMD, wantFork) {
		t.Fatalf("fork PR markdown missing durable URL %q:\n%s", wantFork, forkMD)
	}
	for _, leak := range []string{sourcePNG, "/tmp/no-mistakes-evidence", "secret-leak.png", "missing.png"} {
		if strings.Contains(parentMD, leak) {
			t.Fatalf("PR markdown leaked filesystem or unpublished path %q:\n%s", leak, parentMD)
		}
	}
	if !strings.Contains(parentMD, "Image evidence unavailable") && !strings.Contains(parentMD, unpublishedImageExplanation) {
		t.Fatalf("expected path-free degradation for unpublished images, got:\n%s", parentMD)
	}
	if !strings.Contains(parentMD, "POST /checkout 200") {
		t.Fatalf("expected temporary text evidence to remain embedded, got:\n%s", parentMD)
	}

	imageCopy := filepath.Join(evidenceDir, "published-checkout.png")
	data, err := os.ReadFile(publishedAbs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageCopy, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "pr-testing-summary-parent.md"), []byte(parentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "pr-testing-summary-fork.md"), []byte(forkMD), 0o644); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(data)
	meta := fmt.Sprintf("published_path=%s\ncontent_addressed_name=%s\nsha256=%x\nsize=%d\nparent_url=%s\nfork_url=%s\n",
		published.Path, filepath.Base(published.Path), sum[:], len(data), wantParent, wantFork)
	if err := os.WriteFile(filepath.Join(evidenceDir, "publication-meta.txt"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	html := buildEvidencePreviewHTML(parentMD, forkMD, wantParent, wantFork, filepath.Base(imageCopy))
	if err := os.WriteFile(filepath.Join(evidenceDir, "pr-evidence-preview.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeDemoPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 24, G: 32, B: 48, A: 255})
		}
	}
	for y := 40; y < 100; y++ {
		for x := 40; x < width-40; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 42, G: 111, B: 214, A: 255})
		}
	}
	for y := 140; y < 200; y++ {
		for x := 40; x < width/2; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 230, G: 236, B: 245, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func buildEvidencePreviewHTML(parentMD, forkMD, parentURL, forkURL, imageFile string) string {
	escape := func(s string) string {
		replacer := strings.NewReplacer(
			"&", "&amp;",
			"<", "&lt;",
			">", "&gt;",
			`"`, "&quot;",
		)
		return replacer.Replace(s)
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>PR evidence publishing preview</title>
<style>
  :root { color-scheme: light; font-family: "IBM Plex Sans", "Segoe UI", sans-serif; }
  body { margin: 0; background: #f6f8fa; color: #1f2328; }
  header { padding: 28px 40px 12px; background: #ffffff; border-bottom: 1px solid #d0d7de; }
  h1 { margin: 0 0 8px; font-size: 28px; }
  .sub { color: #656d76; max-width: 760px; line-height: 1.45; }
  main { display: grid; gap: 24px; padding: 28px 40px 48px; }
  section { background: #fff; border: 1px solid #d0d7de; border-radius: 12px; padding: 20px 24px; }
  h2 { margin: 0 0 12px; font-size: 18px; }
  pre { white-space: pre-wrap; word-break: break-word; background: #0d1117; color: #e6edf3; padding: 16px; border-radius: 8px; font-size: 13px; }
  .pr-card { border: 1px solid #d0d7de; border-radius: 8px; overflow: hidden; }
  .pr-card header { background: #f6f8fa; padding: 12px 16px; border-bottom: 1px solid #d0d7de; font-weight: 600; }
  .pr-body { padding: 16px; }
  .pr-body img { display: block; max-width: 100%%; border: 1px solid #d0d7de; border-radius: 6px; }
  .url { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; color: #0969da; word-break: break-all; margin-top: 10px; }
  .bad { color: #cf222e; text-decoration: line-through; }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
  @media (max-width: 900px) { .grid { grid-template-columns: 1fr; } main, header { padding-left: 16px; padding-right: 16px; } }
</style>
</head>
<body>
<header>
  <h1>no-mistakes</h1>
  <p class="sub">Reviewer-visible PR evidence preview: published screenshots render from durable immutable GitHub <code>blob/&lt;sha&gt;/...?raw=1</code> URLs, not validation-machine filesystem paths. Text evidence stays embedded; missing or unpublished images degrade to path-free explanations.</p>
</header>
<main>
  <section>
    <h2>As rendered in a PR body</h2>
    <div class="pr-card">
      <header>## Testing</header>
      <div class="pr-body">
        <p>Validated checkout UI and kept temporary text evidence.</p>
        <p><img src="%s" alt="Validation screenshot 1"></p>
        <p class="url">%s</p>
      </div>
    </div>
  </section>
  <section class="grid">
    <div>
      <h2>Parent-hosted durable URL</h2>
      <pre>%s</pre>
    </div>
    <div>
      <h2>Fork-hosted durable URL</h2>
      <pre>%s</pre>
      <p class="url">%s</p>
    </div>
  </section>
  <section>
    <h2>Generated Testing markdown (parent remote)</h2>
    <pre>%s</pre>
  </section>
  <section>
    <h2>Generated Testing markdown (fork remote)</h2>
    <pre>%s</pre>
  </section>
  <section>
    <h2>Forbidden leakage check</h2>
    <p class="bad">/tmp/no-mistakes-evidence/.../checkout-ui.png</p>
    <p>Local temp paths and unpublished repo paths must not appear in the PR body. The generated markdown above uses only GitHub blob URLs or path-free explanations.</p>
  </section>
</main>
</body>
</html>`, imageFile, escape(parentURL), escape(parentURL), escape(forkURL), escape(forkURL), escape(parentMD), escape(forkMD))
}
