package steps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testPNGBytes() []byte {
	var encoded bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 0x2a, G: 0x6f, B: 0xd6, A: 0xff})
	if err := png.Encode(&encoded, img); err != nil {
		panic(err)
	}
	return encoded.Bytes()
}

func writePublishedImageFixture(t *testing.T, repoRoot string, dirs ...string) (types.TestArtifact, string) {
	t.Helper()
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	name := fmt.Sprintf("%x.png", sum[:16])
	parts := append(append([]string{}, dirs...), name)
	rel := filepath.Join(parts...)
	target := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return types.TestArtifact{
		Kind:      "screenshot",
		Label:     "Screenshot",
		Path:      filepath.ToSlash(rel),
		SHA256:    fmt.Sprintf("%x", sum[:]),
		Size:      int64(len(data)),
		Published: true,
	}, target
}

func commitPRFixture(t *testing.T, repoRoot string) string {
	t.Helper()
	gitCmd(t, repoRoot, "init")
	gitCmd(t, repoRoot, "config", "user.name", "test")
	gitCmd(t, repoRoot, "config", "user.email", "test@example.com")
	gitCmd(t, repoRoot, "add", "-A")
	gitCmd(t, repoRoot, "commit", "-m", "evidence")
	return gitCmd(t, repoRoot, "rev-parse", "HEAD")
}

func findingsWithArtifacts(t *testing.T, artifacts ...types.TestArtifact) string {
	t.Helper()
	raw, err := types.MarshalFindingsJSON(types.Findings{Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNoMistakesRequiredWorkflowChecksPipelineSignature(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "no-mistakes-required.yml"))
	if err != nil {
		t.Fatalf("read required workflow: %v", err)
	}
	if !strings.Contains(string(workflow), "marker='"+noMistakesPRSignature+"'") {
		t.Fatalf("required workflow does not check the generated PR signature %q", noMistakesPRSignature)
	}
}

func TestBuildPipelineSummary_AllClean(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "s2", StepName: types.StepTest, Status: types.StepStatusCompleted},
		{ID: "s3", StepName: types.StepLint, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", DurationMS: 500}},
		"s2": {{Round: 1, Trigger: "initial", DurationMS: 300}},
		"s3": {{Round: 1, Trigger: "initial", DurationMS: 200}},
	}
	md, risk := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "## Pipeline") {
		t.Error("missing Pipeline heading")
	}
	if !strings.Contains(md, "[git push no-mistakes](https://github.com/kunchenguid/no-mistakes)") {
		t.Errorf("expected linked tagline, got:\n%s", md)
	}
	if strings.Count(md, "<details>") != len(steps) {
		t.Fatalf("expected one collapsible per step, got:\n%s", md)
	}
	for _, want := range []string{
		"<summary>✅ **Review** - passed</summary>",
		"<summary>✅ **Test** - passed</summary>",
		"<summary>✅ **Lint** - passed</summary>",
		"✅ No issues found.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %q in pipeline summary, got:\n%s", want, md)
		}
	}
	if risk != "" {
		t.Errorf("expected empty risk for clean run, got: %q", risk)
	}
}

func TestBuildPipelineSummary_IncludesAllPipelineSteps(t *testing.T) {
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepRebase, Status: types.StepStatusCompleted},
		{ID: "s2", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "s3", StepName: types.StepTest, Status: types.StepStatusCompleted},
		{ID: "s4", StepName: types.StepDocument, Status: types.StepStatusCompleted},
		{ID: "s5", StepName: types.StepLint, Status: types.StepStatusCompleted},
		{ID: "s6", StepName: types.StepPush, Status: types.StepStatusCompleted},
		{ID: "s7", StepName: types.StepPR, Status: types.StepStatusRunning},
		{ID: "s8", StepName: types.StepCI, Status: types.StepStatusPending},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", DurationMS: 200}},
		"s2": {{Round: 1, Trigger: "initial", DurationMS: 300}},
		"s3": {{Round: 1, Trigger: "initial", DurationMS: 400}},
		"s4": {{Round: 1, Trigger: "initial", DurationMS: 500}},
		"s5": {{Round: 1, Trigger: "initial", DurationMS: 600}},
		"s6": {{Round: 1, Trigger: "initial", DurationMS: 700}},
	}

	md, _ := BuildPipelineSummary(steps, rounds)

	for _, want := range []string{
		"<summary>✅ **Rebase** - passed</summary>",
		"<summary>✅ **Review** - passed</summary>",
		"<summary>✅ **Test** - passed</summary>",
		"<summary>✅ **Document** - passed</summary>",
		"<summary>✅ **Lint** - passed</summary>",
		"<summary>✅ **Push** - passed</summary>",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %q in pipeline summary, got:\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"<summary>⏳ **PR** - running</summary>", "<summary>⏳ **CI** - pending</summary>"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("did not expect %q in pipeline summary, got:\n%s", unwanted, md)
		}
	}
	if strings.Count(md, "<details>") != len(steps)-2 {
		t.Fatalf("expected one collapsible per pipeline step, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_SkippedStep(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusSkipped},
		{ID: "s2", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {},
		"s2": {{Round: 1, Trigger: "initial", DurationMS: 300}},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "⏭️") {
		t.Errorf("expected skip emoji for skipped step, got:\n%s", md)
	}
	if !strings.Contains(md, "skipped") {
		t.Errorf("expected 'skipped' text for skipped step, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ExcludesPushPRCI(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "s2", StepName: types.StepPush, Status: types.StepStatusCompleted},
		{ID: "s3", StepName: types.StepPR, Status: types.StepStatusCompleted},
		{ID: "s4", StepName: types.StepCI, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", DurationMS: 500}},
		"s2": {{Round: 1, Trigger: "initial", DurationMS: 100}},
		"s3": {{Round: 1, Trigger: "initial", DurationMS: 200}},
		"s4": {{Round: 1, Trigger: "initial", DurationMS: 300}},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	for _, want := range []string{"**Push**"} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %s in pipeline summary, got:\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"**PR**", "**CI**"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("did not expect %s in pipeline summary, got:\n%s", unwanted, md)
		}
	}
}

func TestBuildPipelineSummary_EmptySteps(t *testing.T) {
	t.Parallel()
	md, risk := BuildPipelineSummary(nil, nil)
	if md != "" {
		t.Errorf("expected empty string for nil steps, got: %q", md)
	}
	if risk != "" {
		t.Errorf("expected empty risk for nil steps, got: %q", risk)
	}
}

func TestBuildPipelineSummary_RebaseWithConflicts(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"rebase-1","severity":"warning","file":"pkg/foo.go","description":"merge conflict resolved by agent"}],"summary":"1 conflict resolved"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepRebase, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 2000}},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "**Rebase**") {
		t.Errorf("expected Rebase in output, got:\n%s", md)
	}
	if !strings.Contains(md, "conflict") {
		t.Errorf("expected conflict mention in output, got:\n%s", md)
	}
}

func TestBuildTestingSummary_DoesNotClaimPassedWithoutRounds(t *testing.T) {
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}

	md := BuildTestingSummary(steps, map[string][]*db.StepRound{})

	if md == "" {
		t.Fatal("expected testing summary for completed test step")
	}
	if strings.Contains(md, "passed") {
		t.Errorf("did not expect passed status without recorded rounds, got:\n%s", md)
	}
	if !strings.Contains(md, "findings unavailable") {
		t.Errorf("expected unavailable status without recorded rounds, got:\n%s", md)
	}
}

func TestBuildTestingSummary_IncludesRecordedTestDetails(t *testing.T) {
	t.Parallel()
	findings := "{\"findings\":[],\"summary\":\"\",\"testing_summary\":\"Validated the CLI doctor path and config loading; both passed.\",\"tested\":[\"`go test ./internal/cli -run '^TestDoctorBasic$' -count=1`\"]}"
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummary(steps, rounds)

	if !strings.Contains(md, "- Summary: Validated the CLI doctor path and config loading; both passed.") {
		t.Fatalf("expected natural-language testing summary, got:\n%s", md)
	}
	if !strings.Contains(md, "- `go test ./internal/cli -run '^TestDoctorBasic$' -count=1`") {
		t.Fatalf("expected recorded test command in testing summary, got:\n%s", md)
	}
	if !strings.Contains(md, "- Outcome: ✅ passed across 1 run (300ms)") {
		t.Fatalf("expected outcome line with run count and duration, got:\n%s", md)
	}
	if strings.Index(md, "Summary:") > strings.Index(md, "`go test ./internal/cli -run '^TestDoctorBasic$' -count=1`") {
		t.Fatalf("expected testing summary before raw test details, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_OmitsRecordedTestDetails(t *testing.T) {
	t.Parallel()
	findings := "{\"findings\":[],\"summary\":\"\",\"testing_summary\":\"Validated the CLI doctor path and config loading; both passed.\",\"tested\":[\"`go test ./internal/cli -run '^TestDoctorBasic$' -count=1`\",\"`make e2e`\"]}"
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "## Testing\n\nValidated the CLI doctor path and config loading; both passed.") {
		t.Fatalf("expected natural-language testing summary as a paragraph, got:\n%s", md)
	}
	if strings.Contains(md, "- Summary:") {
		t.Fatalf("did not expect PR testing summary to render as a Summary bullet, got:\n%s", md)
	}
	for _, command := range []string{"go test ./internal/cli", "make e2e"} {
		if strings.Contains(md, command) {
			t.Fatalf("did not expect raw recorded command %q in PR testing summary, got:\n%s", command, md)
		}
	}
	if strings.Contains(md, "Outcome:") {
		t.Fatalf("did not expect outcome row in PR testing summary, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_SummarizesBaselineOnlyTests(t *testing.T) {
	t.Parallel()
	findings := "{\"findings\":[],\"summary\":\"\",\"tested\":[\"`go test ./...`\"]}"
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "## Testing\n\nCompleted 1 recorded test check.") {
		t.Fatalf("expected compact baseline test summary as a paragraph, got:\n%s", md)
	}
	if strings.Contains(md, "- Summary:") {
		t.Fatalf("did not expect compact baseline summary to render as a Summary bullet, got:\n%s", md)
	}
	if strings.Contains(md, "go test ./...") {
		t.Fatalf("did not expect raw recorded command in PR testing summary, got:\n%s", md)
	}
	if strings.Contains(md, "Outcome:") {
		t.Fatalf("did not expect outcome row in PR testing summary, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_KeepsFailedOutcomeForCompactTestedSummary(t *testing.T) {
	t.Parallel()
	findings := "{\"findings\":[],\"summary\":\"\",\"tested\":[\"`go test ./...`\"]}"
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusFailed, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "Completed 1 recorded test check.") {
		t.Fatalf("expected compact baseline test summary as a paragraph, got:\n%s", md)
	}
	if !strings.Contains(md, "Outcome: ❌ failed across 1 run (300ms)") {
		t.Fatalf("expected failed outcome to remain visible, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_KeepsOutcomeForArtifactOnlyEvidence(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","artifacts":[{"kind":"log","label":"Rendered PR markdown","content":"## Testing\n\n- Evidence captured"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "Outcome:") {
		t.Fatalf("expected artifact-only evidence to keep outcome fallback, got:\n%s", md)
	}
	if !strings.Contains(md, "Evidence: Rendered PR markdown") {
		t.Fatalf("expected artifact evidence to render, got:\n%s", md)
	}
}

func TestBuildTestingSummary_EscapesMarkdownInTestingSummary(t *testing.T) {
	t.Parallel()
	findings := "{\"findings\":[],\"summary\":\"\",\"testing_summary\":\"Validated `go test ./...`\\nand noted <details> output\",\"tested\":[]}"
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummary(steps, rounds)

	if !strings.Contains(md, "- Summary: <code>Validated `go test ./...`&#10;and noted &lt;details&gt; output</code>") {
		t.Fatalf("expected escaped testing summary, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_KeepsInlineCodeProseAsPlainText(t *testing.T) {
	t.Parallel()
	summary := "The shutdown-focused tests passed, including explicit `/shutdown`, idle timeout, and `stop` command logic."
	findings := fmt.Sprintf("{\"findings\":[],\"summary\":\"\",\"testing_summary\":%q,\"tested\":[]}", summary)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())

	if strings.Contains(md, "<code>") {
		t.Fatalf("prose summary with inline code spans should not be wrapped in <code>, got:\n%s", md)
	}
	if !strings.Contains(md, summary) {
		t.Fatalf("expected prose summary rendered verbatim, got:\n%s", md)
	}
}

func TestBuildTestingSummary_RendersEvidenceArtifacts(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Checkout success was verified visually.","tested":["manual checkout flow"],"artifacts":[{"kind":"screenshot","label":"Checkout success screenshot","path":"artifacts/checkout-success.png"},{"kind":"log","label":"Checkout server log","content":"POST /checkout 200\nreceipt=ok"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummary(steps, rounds)
	t.Logf("rendered testing markdown:\n%s", md)

	if !strings.Contains(md, "![Validation screenshot 1](artifacts/checkout-success.png)") {
		t.Fatalf("expected screenshot artifact to render inline, got:\n%s", md)
	}
	if !strings.Contains(md, "**Checkout server log**") || !strings.Contains(md, "```text\nPOST /checkout 200\nreceipt=ok\n```") {
		t.Fatalf("expected log artifact content to render inline, got:\n%s", md)
	}
	if strings.Index(md, "Summary:") > strings.Index(md, "![Validation screenshot 1]") {
		t.Fatalf("expected summary before artifacts, got:\n%s", md)
	}
}

func TestBuildTestingSummary_UsesFinalSuccessfulRoundArtifacts(t *testing.T) {
	t.Parallel()
	failedRound := `{"findings":[{"id":"test-1","severity":"warning","description":"checkout failed","action":"auto-fix"}],"summary":"checkout failed","testing_summary":"Checkout failed before fix.","tested":["broken checkout flow"],"artifacts":[{"kind":"screenshot","label":"Broken checkout screenshot","path":"artifacts/broken-checkout.png"}]}`
	passedRound := `{"findings":[],"summary":"","testing_summary":"Checkout passed after fix.","tested":["fixed checkout flow"],"artifacts":[{"kind":"screenshot","label":"Fixed checkout screenshot","path":"artifacts/fixed-checkout.png"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &failedRound, DurationMS: 300},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &passedRound, DurationMS: 400},
		},
	}

	md := BuildTestingSummary(steps, rounds)

	if !strings.Contains(md, "Checkout passed after fix.") || !strings.Contains(md, "![Validation screenshot 1](artifacts/fixed-checkout.png)") {
		t.Fatalf("expected final successful evidence, got:\n%s", md)
	}
	for _, stale := range []string{"Checkout failed before fix.", "broken checkout flow", "Broken checkout screenshot", "artifacts/broken-checkout.png"} {
		if strings.Contains(md, stale) {
			t.Fatalf("did not expect stale failed-round evidence %q, got:\n%s", stale, md)
		}
	}
}

func TestBuildTestingSummary_RejectsUnsafeArtifactTargets(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Absolute path","path":"/Users/alice/project/artifacts/leak.png"},{"kind":"screenshot","label":"Parent path","path":"../secret.png"},{"kind":"screenshot","label":"Markdown injection","url":"https://example.com/evidence.png)\n![leak](file:///tmp/secret"},{"kind":"screenshot","label":"Safe path","path":"artifacts/safe.png"},{"kind":"log","label":"Safe URL","url":"https://example.com/log.txt"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummary(steps, rounds)

	for _, unsafe := range []string{"/Users/alice", "../secret.png", "Markdown injection", "file:///tmp/secret"} {
		if strings.Contains(md, unsafe) {
			t.Fatalf("did not expect unsafe target content %q, got:\n%s", unsafe, md)
		}
	}
	if !strings.Contains(md, "![Validation screenshot 1](artifacts/safe.png)") || !strings.Contains(md, "[Safe URL](https://example.com/log.txt)") {
		t.Fatalf("expected safe artifact targets to render, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_RendersEvidenceArtifactsCompactly(t *testing.T) {
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "artifacts")
	image.Label = "Checkout screenshot"
	findingsValue := types.Findings{
		TestingSummary: "Evidence was collected.",
		Artifacts: []types.TestArtifact{
			image,
			{Kind: "log", Label: "Server log", Path: "artifacts/server.log"},
			{Kind: "log", Label: "Placement rectangle evidence", Content: `{"button":{"top":169,"left":248,"right":272,"bottom":193}}`},
		},
	}
	findings, err := types.MarshalFindingsJSON(findingsValue)
	if err != nil {
		t.Fatal(err)
	}
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", ref, repoRoot)
	t.Logf("rendered PR testing markdown:\n%s", md)

	wantImage := "![Validation screenshot 1](https://github.com/example/widgets/blob/" + ref + "/" + image.Path + "?raw=1)"
	if !strings.Contains(md, wantImage) {
		t.Fatalf("expected screenshot path to render inline from GitHub, got:\n%s", md)
	}
	if !strings.Contains(md, "[Server log](https://github.com/example/widgets/blob/"+ref+"/artifacts/server.log)") {
		t.Fatalf("expected log path to render as GitHub blob URL, got:\n%s", md)
	}
	if !strings.Contains(md, "<details>\n<summary>Evidence: Placement rectangle evidence</summary>") || !strings.Contains(md, "```text\n{\"button\":{\"top\":169,\"left\":248,\"right\":272,\"bottom\":193}}\n```") {
		t.Fatalf("expected content artifact to render in collapsible details, got:\n%s", md)
	}
	for _, broken := range []string{"](" + image.Path + ")", "](artifacts/server.log)"} {
		if strings.Contains(md, broken) {
			t.Fatalf("did not expect broken or noisy artifact rendering %q, got:\n%s", broken, md)
		}
	}
}

func TestBuildTestingSummaryForPR_RendersForkHostedImageInline(t *testing.T) {
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "evidence")
	image.Label = "Checkout screenshot"
	findings := findingsWithArtifacts(t, image)
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "https://github.com/fork-owner/widgets.git", ref, repoRoot)

	want := "![Validation screenshot 1](https://github.com/fork-owner/widgets/blob/" + ref + "/" + image.Path + "?raw=1)"
	if !strings.Contains(md, want) {
		t.Fatalf("expected fork-hosted image markdown %q, got:\n%s", want, md)
	}
}

func TestBuildTestingSummaryForPR_UsesAuthenticatedImmutableImageURL(t *testing.T) {
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "evidence")
	image.Label = "Private screenshot"
	findings := findingsWithArtifacts(t, image)
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://github.com/private-owner/widgets.git", ref, repoRoot)

	want := "![Validation screenshot 1](https://github.com/private-owner/widgets/blob/" + ref + "/" + image.Path + "?raw=1)"
	if !strings.Contains(md, want) {
		t.Fatalf("expected authenticated immutable image URL %q, got:\n%s", want, md)
	}
	if strings.Contains(md, image.Label) {
		t.Fatalf("published image leaked agent-controlled label %q:\n%s", image.Label, md)
	}
}

func TestBuildTestingSummaryForPR_AcceptsPersistedRedactedGitHubRemote(t *testing.T) {
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "evidence")
	image.Label = "Checkout screenshot"
	findings := findingsWithArtifacts(t, image)
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://redacted@github.com/example/widgets.git", ref, repoRoot)

	want := "![Validation screenshot 1](https://github.com/example/widgets/blob/" + ref + "/" + image.Path + "?raw=1)"
	if !strings.Contains(md, want) {
		t.Fatalf("expected redacted persisted remote to render %q, got:\n%s", want, md)
	}
}

func TestGitHubRepositoryForRemote_AcceptsResolvedSSHAlias(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "ssh")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "ssh-resolve-github")
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	repo, ok := githubRepositoryForRemote(context.Background(), "git@github_work:example/widgets.git")

	if !ok {
		t.Fatal("verified SSH alias was rejected")
	}
	if repo.host != "github.com" || repo.owner != "example" || repo.name != "widgets" {
		t.Fatalf("resolved repository = %#v", repo)
	}
}

func TestGitHubRepositoryForRemote_PreservesGitHubWebHostForSSHOver443(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "ssh")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "ssh-resolve-github-over-443")
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	repo, ok := githubRepositoryForRemote(context.Background(), "git@github.com:example/widgets.git")

	if !ok {
		t.Fatal("GitHub SSH-over-443 remote was rejected")
	}
	if repo.host != "github.com" || repo.owner != "example" || repo.name != "widgets" {
		t.Fatalf("resolved repository = %#v", repo)
	}
}

func TestGitHubRepositoryForRemote_PreservesGitHubWebHostForAliasOver443(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "ssh")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "ssh-resolve-github-alias-over-443")
	ghConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghConfig, "hosts.yml"), []byte("github.com:\n  user: test\n  oauth_token: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", ghConfig)
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	repo, ok := githubRepositoryForRemote(context.Background(), "git@github_work:example/widgets.git")

	if !ok {
		t.Fatal("GitHub SSH alias over port 443 was rejected")
	}
	if repo.host != "github.com" || repo.owner != "example" || repo.name != "widgets" {
		t.Fatalf("resolved repository = %#v", repo)
	}
}

func TestGitHubRepositoryForRemote_AcceptsExplicitGitHubSSHOver443(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	repo, ok := githubRepositoryForRemote(context.Background(), "ssh://git@ssh.github.com:443/example/widgets.git")

	if !ok {
		t.Fatal("explicit GitHub SSH-over-443 remote was rejected")
	}
	if repo.host != "github.com" || repo.owner != "example" || repo.name != "widgets" {
		t.Fatalf("resolved repository = %#v", repo)
	}
}

func TestGitHubRepositoryForRemote_AcceptsGitHubSSHPort22(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	repo, ok := githubRepositoryForRemote(context.Background(), "ssh://git@github.com:22/example/widgets.git")

	if !ok {
		t.Fatal("GitHub SSH port 22 remote was rejected")
	}
	if repo.host != "github.com" || repo.owner != "example" || repo.name != "widgets" {
		t.Fatalf("resolved repository = %#v", repo)
	}
}

func TestGitHubRepositoryForRemote_AcceptsAuthenticatedGHESSHCustomPort(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "ssh")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "ssh-resolve-ghes-custom-port")
	ghConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghConfig, "hosts.yml"), []byte("github.corp.example:\n  user: test\n  oauth_token: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", ghConfig)
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	repo, ok := githubRepositoryForRemote(context.Background(), "ssh://git@github.corp.example:2222/team/widgets.git")

	if !ok {
		t.Fatal("authenticated GHES SSH custom-port remote was rejected")
	}
	if repo.host != "github.corp.example" || repo.owner != "team" || repo.name != "widgets" {
		t.Fatalf("resolved repository = %#v", repo)
	}
}

func TestBuildTestingSummaryForPR_RejectsUnverifiedNonGitHubHost(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	repoRoot := t.TempDir()
	imagePath := filepath.Join(repoRoot, "evidence", "checkout.png")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":"evidence/checkout.png"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://gitlab.example.com/example/widgets.git", "abc123", repoRoot)

	if !strings.Contains(md, "Image evidence unavailable.") {
		t.Fatalf("expected unverified host to degrade safely, got:\n%s", md)
	}
	if strings.Contains(md, "gitlab.example.com/example/widgets/raw/") {
		t.Fatalf("unverified host received a GitHub raw route:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_EscapesEveryImageURLPathSegment(t *testing.T) {
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "evidence #?", "snow 雪", "100%")
	image.Label = "Escaped screenshot"
	findings := findingsWithArtifacts(t, image)
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", ref, repoRoot)

	want := "https://github.com/example/widgets/blob/" + ref + "/evidence%20%23%3F/snow%20%E9%9B%AA/100%25/" + filepath.Base(image.Path) + "?raw=1"
	if !strings.Contains(md, want) {
		t.Fatalf("expected escaped immutable image URL %q, got:\n%s", want, md)
	}
}

func TestBuildTestingSummaryForPR_RendersGitHubEnterpriseImageInline(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	ghConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghConfig, "hosts.yml"), []byte("github.corp.example:\n  user: test\n  oauth_token: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", ghConfig)
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "evidence #?", "snow 雪", "100%")
	image.Label = "Enterprise screenshot"
	findings := findingsWithArtifacts(t, image)
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://github.corp.example/team/widgets.git", ref, repoRoot)

	want := "https://github.corp.example/team/widgets/blob/" + ref + "/evidence%20%23%3F/snow%20%E9%9B%AA/100%25/" + filepath.Base(image.Path) + "?raw=1"
	if !strings.Contains(md, want) {
		t.Fatalf("expected escaped immutable GHES image URL %q, got:\n%s", want, md)
	}
}

func TestBuildTestingSummaryForPR_RendersAuthenticatedGHESCustomPort(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	ghConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghConfig, "hosts.yml"), []byte("github.corp.example:8443:\n  user: test\n  oauth_token: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", ghConfig)
	repoRoot := t.TempDir()
	image, _ := writePublishedImageFixture(t, repoRoot, "evidence")
	findings := findingsWithArtifacts(t, image)
	ref := commitPRFixture(t, repoRoot)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://github.corp.example:8443/team/widgets.git", ref, repoRoot)

	want := "https://github.corp.example:8443/team/widgets/blob/" + ref + "/" + image.Path + "?raw=1"
	if !strings.Contains(md, want) {
		t.Fatalf("expected authenticated GHES custom-port URL %q, got:\n%s", want, md)
	}
}

func TestBuildTestingSummaryForPR_RejectsUntrustedGitHubRemoteLayouts(t *testing.T) {
	repoRoot := t.TempDir()
	imagePath := filepath.Join(repoRoot, "evidence", "checkout.png")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":"evidence/checkout.png"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	for _, remote := range []string{
		"http://github.corp.example/team/widgets.git",
		"https://user@github.com/team/widgets.git",
		"https://user:secret@github.corp.example/team/widgets.git",
		"https://github.com:8443/team/widgets.git",
		"https://evilgithub.com/team/widgets.git",
		"https://github.corp.example/team/widgets/extra.git",
		"https://github.corp.example/team/widgets.git?download=1",
		"https://bad host/team/widgets.git",
	} {
		t.Run(remote, func(t *testing.T) {
			md := BuildTestingSummaryForPR(steps, rounds, remote, "abc123", repoRoot)
			if !strings.Contains(md, "Image evidence unavailable.") {
				t.Fatalf("expected safe fallback for %q, got:\n%s", remote, md)
			}
			for _, unsafe := range []string{"evidence/checkout.png", "raw.githubusercontent.com", "/raw/abc123/"} {
				if strings.Contains(md, unsafe) {
					t.Fatalf("remote %q exposed unsupported image target %q:\n%s", remote, unsafe, md)
				}
			}
		})
	}
}

func TestBuildTestingSummaryForPR_MissingRepoImageDegradesSafely(t *testing.T) {
	repoRoot := t.TempDir()
	findings := `{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Missing screenshot","path":"evidence/missing.png"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot)

	if !strings.Contains(md, "- Evidence: Image evidence unavailable.") {
		t.Fatalf("expected safe missing-file explanation, got:\n%s", md)
	}
	for _, unsafe := range []string{"evidence/missing.png", "raw.githubusercontent.com", "local file"} {
		if strings.Contains(md, unsafe) {
			t.Fatalf("missing image exposed unsafe target %q:\n%s", unsafe, md)
		}
	}
}

func TestBuildTestingSummaryForPR_RequiresManifestBlobAtPushedCommit(t *testing.T) {
	repoRoot := t.TempDir()
	image, target := writePublishedImageFixture(t, repoRoot, "evidence")
	image.Label = "Checkout screenshot"
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "tracked.txt"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := commitPRFixture(t, repoRoot)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := findingsWithArtifacts(t, image)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://github.com/example/widgets.git", ref, repoRoot)

	if !strings.Contains(md, "Image evidence unavailable.") {
		t.Fatalf("expected image missing from pushed commit to degrade safely, got:\n%s", md)
	}
	if strings.Contains(md, "raw.githubusercontent.com") {
		t.Fatalf("missing pushed blob rendered an immutable URL:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_CancellationStopsBlobVerification(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	logFile := filepath.Join(t.TempDir(), "git.log")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "record-success")
	t.Setenv("FAKE_CLI_LOG", logFile)

	data := testPNGBytes()
	sum := sha256.Sum256(data)
	image := types.TestArtifact{
		Kind:      "screenshot",
		Label:     "Checkout screenshot",
		Path:      "evidence/checkout.png",
		SHA256:    fmt.Sprintf("%x", sum[:]),
		Size:      int64(len(data)),
		Published: true,
	}
	findings := findingsWithArtifacts(t, image)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	md := buildTestingSummaryForPR(ctx, steps, rounds, "https://github.com/example/widgets.git", "abc123", t.TempDir())

	if !strings.Contains(md, "Image evidence unavailable.") {
		t.Fatalf("expected cancellation to degrade safely, got:\n%s", md)
	}
	if data, err := os.ReadFile(logFile); err == nil && len(data) > 0 {
		t.Fatalf("cancellation launched git blob verification:\n%s", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestBuildTestingSummaryForPR_RetryUsesPushedImageBlob(t *testing.T) {
	for _, mutation := range []struct {
		name string
		run  func(*testing.T, string)
	}{
		{
			name: "deleted worktree file",
			run: func(t *testing.T, target string) {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "changed worktree file",
			run: func(t *testing.T, target string) {
				if err := os.WriteFile(target, coloredPNGBytes(99), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked worktree file",
			run: func(t *testing.T, target string) {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside.png"), target); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			image, target := writePublishedImageFixture(t, repoRoot, "evidence")
			image.Label = "Checkout screenshot"
			ref := commitPRFixture(t, repoRoot)
			mutation.run(t, target)
			findings := findingsWithArtifacts(t, image)
			steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
			rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

			md := BuildTestingSummaryForPR(steps, rounds, "https://github.com/example/widgets.git", ref, repoRoot)

			want := "![Validation screenshot 1](https://github.com/example/widgets/blob/" + ref + "/" + image.Path + "?raw=1)"
			if !strings.Contains(md, want) {
				t.Fatalf("retry did not render pushed image blob %q:\n%s", want, md)
			}
		})
	}
}

func TestBuildTestingSummaryForPR_ScrubsTempPathsFromSummaryAndCommands(t *testing.T) {
	root := testEvidenceRoot()
	imagePath := filepath.Join(root, "run-secret", "checkout.png")
	fileURL := "file://" + filepath.ToSlash(imagePath)
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":%q,"tested":[%q]}`,
		"Captured "+imagePath+" and "+fileURL,
		"playwright screenshot "+imagePath)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	pipeline, _ := BuildPipelineSummary(steps, rounds)
	testing := BuildTestingSummaryForPR(steps, rounds, "https://github.com/example/widgets.git", "abc123", t.TempDir())

	for name, rendered := range map[string]string{"pipeline": pipeline, "testing": testing} {
		for _, leaked := range []string{root, "run-secret", "checkout.png", "file://"} {
			if strings.Contains(rendered, leaked) {
				t.Fatalf("%s output leaked %q:\n%s", name, leaked, rendered)
			}
		}
		if !strings.Contains(rendered, "[image evidence]") {
			t.Fatalf("%s output omitted generic placeholder:\n%s", name, rendered)
		}
	}
}

func TestBuildTestingSummaryForPR_ScrubsLocalTempVisualArtifactPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	localPath := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123", "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot)
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "- Evidence: Image evidence unavailable.") {
		t.Fatalf("expected a safe publication explanation, got:\n%s", md)
	}
	for _, broken := range []string{"![Checkout screenshot]", "github.com/example/widgets/blob/abc123/", localPath, "/tmp/", "local file"} {
		if strings.Contains(md, broken) {
			t.Fatalf("did not expect local temp artifact to be rendered as a visual or GitHub link %q, got:\n%s", broken, md)
		}
	}
}

func TestBuildTestingSummaryForPR_ScrubsCaptionedLocalVisualArtifactPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	localPath := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123", "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q,"content":"Checkout completed visually."}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot)
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "Image evidence unavailable.") {
		t.Fatalf("expected captioned local temp screenshot to degrade safely, got:\n%s", md)
	}
	if strings.Contains(md, "Checkout completed visually.") {
		t.Fatalf("did not expect unpublished image metadata to be echoed, got:\n%s", md)
	}
	for _, unsafe := range []string{localPath, "/tmp/", "local file", "github.com/example/widgets/blob/abc123/"} {
		if strings.Contains(md, unsafe) {
			t.Fatalf("did not expect unsafe local source %q, got:\n%s", unsafe, md)
		}
	}
}

func TestRenderUnpublishedCompactImage_ExplainsDisabledPublication(t *testing.T) {
	got := renderUnpublishedCompactImage("Checkout screenshot", disabledImagePublicationExplanation)
	if got != "- Evidence: Image evidence unavailable because publication is disabled.\n" {
		t.Fatalf("disabled image publication rendering = %q", got)
	}
}

func TestBuildTestingSummaryForPR_DoesNotRenderImageLabel(t *testing.T) {
	malicious := "/tmp/no-mistakes-evidence/run-secret/capture.png"
	findings := fmt.Sprintf(`{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":%q,"content":"Image evidence was not published."}]}`, malicious)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	md := BuildTestingSummaryForPR(steps, rounds, "https://github.com/example/widgets.git", "abc123", t.TempDir())

	if strings.Contains(md, malicious) || strings.Contains(md, "/tmp/") {
		t.Fatalf("unpublished image leaked agent-controlled label:\n%s", md)
	}
	if !strings.Contains(md, "Image evidence unavailable") {
		t.Fatalf("missing generic image fallback:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_PrefersArtifactURLOverLocalPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	localPath := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123", "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","url":"https://example.com/checkout.png","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot)

	if !strings.Contains(md, "![Validation screenshot 1](https://example.com/checkout.png)") {
		t.Fatalf("expected artifact URL to take precedence, got:\n%s", md)
	}
	if strings.Contains(md, "local file:") || strings.Contains(md, localPath) {
		t.Fatalf("did not expect local path to replace URL, got:\n%s", md)
	}
}

func TestArtifactPathRelativeToRoot_AllowsSymlinkEquivalentPaths(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	root := filepath.Join(tempDir, "evidence")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(tempDir, "linked-evidence")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	target := filepath.Join(linkedRoot, "run-123", "checkout.png")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	rel, ok := artifactPathRelativeToRoot(target, root)

	if !ok {
		t.Fatalf("expected symlink-equivalent target to be within root")
	}
	if rel != filepath.Join("run-123", "checkout.png") {
		t.Fatalf("expected normalized relative path, got %q", rel)
	}
}

// writeTempEvidenceFile creates a uniquely-named file under the temp evidence
// root (the only absolute location outside the repo that artifact paths may
// reference) and registers cleanup of its run directory.
func writeTempEvidenceFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	runDir := filepath.Join(testEvidenceRoot(), "run-"+strings.ReplaceAll(t.Name(), "/", "_"))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	path := filepath.Join(runDir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildTestingSummaryForPR_EmbedsLocalTextEvidenceContent(t *testing.T) {
	fileBody := "RENDERED WIZARD SCREEN\n  > Claude\n  > Codex\nGitHub source selected"
	localPath := writeTempEvidenceFile(t, "init-wizard-rendered-screens.txt", []byte(fileBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Rendered setup wizard screens","path":%q,"content":"Shows agent auto-detect with Claude and Codex listed individually."}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "<summary>Evidence: Rendered setup wizard screens</summary>") {
		t.Fatalf("expected evidence summary, got:\n%s", md)
	}
	if !strings.Contains(md, "Shows agent auto-detect with Claude and Codex listed individually.") {
		t.Fatalf("expected caption to render as description text, got:\n%s", md)
	}
	if !strings.Contains(md, "```text\n"+fileBody+"\n```") {
		t.Fatalf("expected file content to be embedded in a fence, got:\n%s", md)
	}
	for _, broken := range []string{"Source: local file", localPath} {
		if strings.Contains(md, broken) {
			t.Fatalf("did not expect local file reference %q, got:\n%s", broken, md)
		}
	}
}

func TestBuildTestingSummaryForPR_PreservesPublicURLForEmbeddedTextEvidence(t *testing.T) {
	fileBody := "rendered wizard evidence"
	localPath := writeTempEvidenceFile(t, "wizard.txt", []byte(fileBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Wizard log","url":"https://example.com/artifacts/wizard.txt","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())

	if !strings.Contains(md, "Source: [Wizard log](https://example.com/artifacts/wizard.txt)") {
		t.Fatalf("expected public URL source to be preserved, got:\n%s", md)
	}
	if !strings.Contains(md, "```text\n"+fileBody+"\n```") {
		t.Fatalf("expected local text evidence to remain embedded, got:\n%s", md)
	}
	if strings.Contains(md, localPath) {
		t.Fatalf("did not expect local path to be exposed, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_EmbedsRepoTextEvidenceContent(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fileBody := "POST /checkout 200\nreceipt=ok"
	if err := os.WriteFile(filepath.Join(repoRoot, "artifacts", "server.log"), []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Server log","path":"artifacts/server.log"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot)
	t.Logf("rendered PR testing markdown:\n%s", md)

	if strings.Contains(md, fileBody) || strings.Contains(md, "```text") {
		t.Fatalf("did not expect repo-relative file content to be embedded, got:\n%s", md)
	}
	if !strings.Contains(md, "[Server log](https://github.com/example/widgets/blob/abc123/artifacts/server.log)") {
		t.Fatalf("expected repo-relative artifact to render as a blob link, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_DoesNotEmbedRepoRelativeSecrets(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	secret := "DATABASE_URL=postgres://secret"
	if err := os.WriteFile(filepath.Join(repoRoot, ".env"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Environment dump","path":".env"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot)

	if strings.Contains(md, secret) || strings.Contains(md, "```text") {
		t.Fatalf("did not expect repo-relative secret content to be embedded, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_RendersFileCaptionAsText(t *testing.T) {
	fileBody := "safe evidence body"
	localPath := writeTempEvidenceFile(t, "caption.txt", []byte(fileBody))
	caption := "<img src=x onerror=alert(1)>\n[leak](file:///tmp/secret)"
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Captioned log","path":%q,"content":%q}]}`, localPath, caption)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())

	if strings.Contains(md, caption) || strings.Contains(md, "<img src=x") {
		t.Fatalf("did not expect raw caption markdown/html, got:\n%s", md)
	}
	if !strings.Contains(md, "<code>&lt;img src=x onerror=alert(1)&gt;&#10;[leak](file:///tmp/secret)</code>") {
		t.Fatalf("expected escaped caption text, got:\n%s", md)
	}
	if !strings.Contains(md, "```text\n"+fileBody+"\n```") {
		t.Fatalf("expected file body to remain fenced, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_TruncatesLargeTextEvidenceFromMiddle(t *testing.T) {
	head := strings.Repeat("HEAD-LINE\n", 50)
	tail := strings.Repeat("TAIL-LINE\n", 50)
	fileBody := head + strings.Repeat("X", 40*1024) + tail
	localPath := writeTempEvidenceFile(t, "big.txt", []byte(fileBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Large log","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())

	if !strings.Contains(md, "HEAD-LINE") {
		t.Fatalf("expected truncated content to keep the head, got:\n%s", md[:min(len(md), 600)])
	}
	if !strings.Contains(md, "TAIL-LINE") {
		t.Fatalf("expected truncated content to keep the tail")
	}
	if !strings.Contains(md, "bytes truncated") {
		t.Fatalf("expected a middle-truncation marker")
	}
	if len(md) >= len(fileBody) {
		t.Fatalf("expected rendered output to be shorter than the full file (%d bytes), got %d", len(fileBody), len(md))
	}
}

func TestBuildTestingSummaryForPR_LimitsTotalEmbeddedTextEvidence(t *testing.T) {
	firstBody := strings.Repeat("first evidence line\n", 700)
	secondBody := strings.Repeat("second evidence line\n", 700)
	thirdBody := strings.Repeat("third evidence line\n", 700)
	firstPath := writeTempEvidenceFile(t, "first.txt", []byte(firstBody))
	secondPath := writeTempEvidenceFile(t, "second.txt", []byte(secondBody))
	thirdPath := writeTempEvidenceFile(t, "third.txt", []byte(thirdBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"First log","path":%q},{"label":"Second log","path":%q},{"label":"Third log","path":%q}]}`, firstPath, secondPath, thirdPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())

	if !strings.Contains(md, firstBody) || !strings.Contains(md, secondBody) {
		t.Fatalf("expected earlier evidence to embed before budget is exhausted, got:\n%s", md[:min(len(md), 600)])
	}
	if strings.Contains(md, thirdBody) {
		t.Fatalf("did not expect evidence beyond the total budget to be embedded")
	}
	if !strings.Contains(md, "- Evidence: Third log was not published.") {
		t.Fatalf("expected evidence beyond the total budget to degrade safely, got:\n%s", md)
	}
	if strings.Contains(md, thirdPath) || strings.Contains(md, "local file") {
		t.Fatalf("evidence beyond the total budget exposed a local path, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_FallsBackForBinaryEvidence(t *testing.T) {
	localPath := writeTempEvidenceFile(t, "capture.dat", []byte{0x00, 0x01, 0x02, 0xff, 0x00})
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Binary capture","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir())

	if !strings.Contains(md, "- Evidence: Binary capture was not published.") {
		t.Fatalf("expected binary evidence to degrade safely, got:\n%s", md)
	}
	if strings.Contains(md, "```text") {
		t.Fatalf("did not expect binary content to be embedded as text, got:\n%s", md)
	}
	if strings.Contains(md, localPath) || strings.Contains(md, "local file") {
		t.Fatalf("binary evidence exposed a local path, got:\n%s", md)
	}
}
