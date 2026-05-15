package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
		"**Round 1** - passed ✅",
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

	if !strings.Contains(md, "![Checkout success screenshot](artifacts/checkout-success.png)") {
		t.Fatalf("expected screenshot artifact to render inline, got:\n%s", md)
	}
	if !strings.Contains(md, "**Checkout server log**") || !strings.Contains(md, "```text\nPOST /checkout 200\nreceipt=ok\n```") {
		t.Fatalf("expected log artifact content to render inline, got:\n%s", md)
	}
	if strings.Index(md, "Summary:") > strings.Index(md, "![Checkout success screenshot]") {
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

	if !strings.Contains(md, "Checkout passed after fix.") || !strings.Contains(md, "![Fixed checkout screenshot](artifacts/fixed-checkout.png)") {
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
	if !strings.Contains(md, "![Safe path](artifacts/safe.png)") || !strings.Contains(md, "[Safe URL](https://example.com/log.txt)") {
		t.Fatalf("expected safe artifact targets to render, got:\n%s", md)
	}
}

func TestBuildTestingSummary_ConvertsArtifactPathsToGitHubURLs(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":"artifacts/checkout.png"},{"kind":"log","label":"Server log","path":"artifacts/server.log"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123")
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "![Checkout screenshot](https://raw.githubusercontent.com/example/widgets/abc123/artifacts/checkout.png)") {
		t.Fatalf("expected screenshot path to render as raw GitHub URL, got:\n%s", md)
	}
	if !strings.Contains(md, "[Server log](https://github.com/example/widgets/blob/abc123/artifacts/server.log)") {
		t.Fatalf("expected log path to render as GitHub blob URL, got:\n%s", md)
	}
	for _, broken := range []string{"](artifacts/checkout.png)", "](artifacts/server.log)"} {
		if strings.Contains(md, broken) {
			t.Fatalf("did not expect raw repository-relative artifact path %q, got:\n%s", broken, md)
		}
	}
}
