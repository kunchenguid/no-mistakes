package steps

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineSummaryFor_ConcisePRChangeKeepsRepresentativeGoldens(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{{ID: "review-1", StepName: types.StepReview, Status: types.StepStatusCompleted}}
	rounds := map[string][]*db.StepRound{"review-1": {{Round: 1, Trigger: "initial"}}}

	htmlGot, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub)
	htmlWant := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" +
		`<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"` + testPipelineHeadSHA + `","steps":[{"step":"review","status":"completed"}]} -->` +
		"\n\n<details>\n<summary>✅ **Review** - passed</summary>\n\n✅ No issues found.\n</details>\n"
	if htmlGot != htmlWant {
		t.Fatalf("HTML pipeline golden changed\nwant:\n%s\n\ngot:\n%s", htmlWant, htmlGot)
	}

	markdownGot, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket)
	markdownWant := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n✅ **Review** - passed\n"
	if markdownGot != markdownWant {
		t.Fatalf("Bitbucket pipeline golden changed\nwant:\n%s\n\ngot:\n%s", markdownWant, markdownGot)
	}
}

func TestBuildTestingSummaryForPR_IsConciseAndInlinesOnlyTrustedVisuals(t *testing.T) {
	t.Parallel()
	findings := `{
		"findings":[],
		"summary":"",
		"testing_summary":"474 backend tests and 16 frontend metrics tests passed. A real PostgreSQL probe verified inclusive boundaries and bounded line-item access. This additional sentence belongs behind the testing-detail disclosure rather than in the visible summary.",
		"tested":["backend suite","frontend suite"],
		"artifacts":[
			{"kind":"screenshot","label":"Before","path":"artifacts/before.png"},
			{"kind":"screenshot","label":"After","path":"artifacts/after.png"},
			{"kind":"screenshot","label":"External tracker","url":"https://tracker.example/pixel.png"},
			{"kind":"log","label":"Probe output","content":"verbose probe output"}
		]
	}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	got := BuildTestingSummaryForPRWithProvider(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderGitHub)
	if !strings.Contains(got, "✅ **Test** - passed · 2 recorded checks · 4 evidence items") {
		t.Fatalf("testing summary missing exact compact status/count line:\n%s", got)
	}
	for _, want := range []string{
		"[![Before](https://raw.githubusercontent.com/example/widgets/abc123/artifacts/before.png)](https://github.com/example/widgets/blob/abc123/artifacts/before.png)",
		"[![After](https://raw.githubusercontent.com/example/widgets/abc123/artifacts/after.png)](https://github.com/example/widgets/blob/abc123/artifacts/after.png)",
		"<summary>More testing detail</summary>",
		"<summary>Additional evidence — 2 items</summary>",
		"[External tracker](https://tracker.example/pixel.png)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("testing summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "![External tracker]") {
		t.Fatalf("arbitrary external image URL was loaded inline:\n%s", got)
	}
	visible := strings.Split(got, "<details>")[0]
	if strings.Contains(visible, "additional sentence") || strings.Contains(visible, "verbose probe output") {
		t.Fatalf("visible testing summary contains verbose detail:\n%s", visible)
	}
}

func TestBuildTestingSummaryForPR_PreservesArtifactOrderAroundInlineVisuals(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence collected.","artifacts":[{"kind":"log","label":"Before log","url":"https://example.test/before.log"},{"kind":"screenshot","label":"Visual","path":"artifacts/visual.png"},{"kind":"log","label":"After log","url":"https://example.test/after.log"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	got := BuildTestingSummaryForPRWithProvider(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderGitHub)
	positions := []int{strings.Index(got, "Before log"), strings.Index(got, "![Visual]"), strings.Index(got, "After log")}
	if positions[0] < 0 || positions[1] <= positions[0] || positions[2] <= positions[1] {
		t.Fatalf("artifact order changed around inline visual: %v\n%s", positions, got)
	}
}

func TestBuildTestingSummaryForPR_LimitsInlineVisualsToTwo(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Visual evidence collected.","artifacts":[{"kind":"screenshot","label":"First","path":"artifacts/first.png"},{"kind":"screenshot","label":"Second","path":"artifacts/second.png"},{"kind":"screenshot","label":"Third","path":"artifacts/third.png"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	got := BuildTestingSummaryForPRWithProvider(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderGitHub)
	if strings.Count(got, "[![") != 2 {
		t.Fatalf("expected exactly two inline visuals:\n%s", got)
	}
	if strings.Contains(got, "[![Third]") || !strings.Contains(got, "[Third](https://github.com/example/widgets/blob/abc123/artifacts/third.png)") {
		t.Fatalf("third visual did not degrade to a safe link:\n%s", got)
	}
}

func TestBuildTestingSummaryForPR_EscapesArtifactLabelMarkdownInjection(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence collected.","artifacts":[{"kind":"log","label":"log ](https://evil.example) ![pixel](https://tracker.example/pixel)","url":"https://safe.example/log"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		got := BuildTestingSummaryForPRWithProvider(steps, rounds, "https://bitbucket.org/example/widgets.git", "abc123", t.TempDir(), "", nil, provider)
		if strings.Contains(got, "![pixel]") || strings.Contains(got, "log ](https://evil.example)") {
			t.Fatalf("provider %s rendered active Markdown from an evidence label:\n%s", provider, got)
		}
		if !strings.Contains(got, "https://safe.example/log") {
			t.Fatalf("provider %s lost the safe artifact target:\n%s", provider, got)
		}
	}
}

func TestRenderBitbucketConciseArtifact_EscapesLocalReferenceLabel(t *testing.T) {
	t.Parallel()
	opts := testingSummaryOptions{flavor: prBodyMarkdown, repoRoot: "/repo", evidenceRoot: "/evidence"}
	got := renderBitbucketConciseArtifact(types.TestArtifact{
		Label: "![pixel](https://tracker.example/pixel)",
		Path:  "/evidence/capture.png",
	}, opts, "![pixel](https://tracker.example/pixel)")
	if strings.Contains(got, "![pixel]") {
		t.Fatalf("Bitbucket local evidence label rendered an active image:\n%s", got)
	}
}

func TestBuildTestingSummaryForPR_BitbucketIsLinkOnlyAndBounded(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Focused tests passed. More detail should remain bounded.","tested":["focused suite"],"artifacts":[{"kind":"screenshot","label":"After","path":"artifacts/after.png"},{"kind":"log","label":"Inline log","content":"large raw output"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	got := BuildTestingSummaryForPRWithProvider(steps, rounds, "https://bitbucket.org/example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderBitbucket)
	if !strings.Contains(got, "✅ **Test** - passed · 1 recorded check · 1 evidence item") {
		t.Fatalf("Bitbucket summary missing exact count line:\n%s", got)
	}
	if strings.Contains(got, "<details>") || strings.Contains(got, "![") || strings.Contains(got, "large raw output") {
		t.Fatalf("Bitbucket concise summary exposed HTML, inline images, or raw evidence:\n%s", got)
	}
	if !strings.Contains(got, "[After]") {
		t.Fatalf("Bitbucket summary dropped safe visual link:\n%s", got)
	}
}

func TestBuildTestingSummaryForPR_EncodesArtifactLabelExactlyOnce(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence collected.","artifacts":[{"kind":"log","label":"timeout <5s","content":"captured probe output"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	got := BuildTestingSummaryForPRWithProvider(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderGitHub)
	if !strings.Contains(got, "Evidence: timeout &lt;5s") {
		t.Fatalf("GitHub evidence label did not render as its own text:\n%s", got)
	}
	if strings.Contains(got, `\&`) {
		t.Fatalf("GitHub evidence label was escaped twice, publishing the escape itself:\n%s", got)
	}
}

func TestRenderBitbucketConciseArtifact_EncodesLocalReferenceLabelExactlyOnce(t *testing.T) {
	t.Parallel()
	opts := testingSummaryOptions{flavor: prBodyMarkdown, repoRoot: "/repo", evidenceRoot: "/evidence"}
	got := renderBitbucketConciseArtifact(types.TestArtifact{
		Label: "timeout <5s",
		Path:  "/evidence/capture.png",
	}, opts, "timeout <5s")
	if !strings.Contains(got, `timeout \<5s`) {
		t.Fatalf("Bitbucket local evidence label did not render as its own text:\n%s", got)
	}
	if strings.Contains(got, "&lt;") {
		t.Fatalf("Bitbucket local evidence label was HTML-escaped on top of its Markdown escape:\n%s", got)
	}
}

func TestBuildTestingSummaryForPR_NonGitHubHostLinksArtifactInsteadOfInliningRelativeImage(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	shot := filepath.Join(repoRoot, "artifacts", "shot.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence collected.","artifacts":[{"kind":"screenshot","label":"After","path":%q}]}`, shot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, FindingsJSON: &findings}}}

	got := BuildTestingSummaryForPRWithProvider(steps, rounds, "https://gitlab.com/example/widgets.git", "abc123", repoRoot, "", nil, scm.ProviderGitLab)
	if strings.Contains(got, "[![") {
		t.Fatalf("relative repo path was promoted to an inline image that cannot resolve:\n%s", got)
	}
	if !strings.Contains(got, "- Evidence: [After](artifacts/shot.png)") {
		t.Fatalf("non-GitHub host lost the artifact link fallback:\n%s", got)
	}
}
