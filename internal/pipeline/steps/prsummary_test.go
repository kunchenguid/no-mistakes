package steps

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testPipelineHeadSHA = "0123456789abcdef0123456789abcdef01234567"

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
	md, risk := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

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

func TestBuildPipelineSummary_BitbucketCloudOmitsHTMLAndAttestation(t *testing.T) {
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

	md, risk := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket)

	if !strings.Contains(md, "## Pipeline") {
		t.Fatal("missing Pipeline heading")
	}
	if !strings.Contains(md, noMistakesPRSignature) {
		t.Fatalf("expected human signature, got:\n%s", md)
	}
	for _, want := range []string{
		"✅ **Review** - passed",
		"✅ **Test** - passed",
		"✅ **Lint** - passed",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %q in Bitbucket pipeline summary, got:\n%s", want, md)
		}
	}
	for _, leak := range []string{
		"<details>", "</details>", "<summary>", "</summary>",
		"<!--", "-->", "<code>", "<video", "No issues found.",
		pipelineAttestationCommentPrefix,
	} {
		if strings.Contains(md, leak) {
			t.Errorf("Bitbucket pipeline leaked %q:\n%s", leak, md)
		}
	}
	if risk != "" {
		t.Errorf("expected empty risk for clean run, got: %q", risk)
	}
}

func TestBuildPipelineSummary_FindingCannotSpoofFoldBoundary(t *testing.T) {
	t.Parallel()
	for _, field := range []struct {
		name        string
		file        string
		description string
		spoof       string
	}{
		{"description", "foo.py", "Update the header:\n### Configuration\nadd the missing key", "\n### Configuration"},
		{"file", "src/app.py\n### Fake Heading", "add the missing key", "\n### Fake Heading"},
	} {
		t.Run(field.name, func(t *testing.T) {
			t.Parallel()
			findings := types.Findings{
				Items: []types.Finding{{
					ID:          "lint-1",
					Severity:    "warning",
					File:        field.file,
					Line:        12,
					Description: field.description,
					Action:      types.ActionAutoFix,
				}},
			}
			raw, err := types.MarshalFindingsJSON(findings)
			if err != nil {
				t.Fatal(err)
			}
			steps := []*db.StepResult{
				{ID: "s1", StepName: types.StepLint, Status: types.StepStatusCompleted, FindingsJSON: &raw},
			}
			rounds := map[string][]*db.StepRound{
				"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &raw, DurationMS: 200}},
			}

			for _, tc := range []struct {
				name     string
				provider scm.Provider
			}{
				{"github", scm.ProviderGitHub},
				{"bitbucket", scm.ProviderBitbucket},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					md, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, tc.provider)
					if strings.Contains(md, field.spoof) {
						t.Fatalf("embedded finding %s was not escaped, spoofs the PR-body fold-boundary marker:\n%s", field.name, md)
					}
					if !strings.Contains(md, "add the missing key") {
						t.Fatalf("expected the finding bullet to stay intact:\n%s", md)
					}

					_, updates := splitPipelineSectionHeader(md)
					groups := parsePipelineUpdateGroups(updates)
					if len(groups) != 1 {
						t.Fatalf("expected exactly 1 step group, got %d - embedded finding %s was mistaken for a fold boundary:\n%s", len(groups), field.name, md)
					}
				})
			}
		})
	}
}

func TestBuildPipelineSummary_EmitsStructuredStepAttestation(t *testing.T) {
	t.Parallel()

	steps := []*db.StepResult{
		{ID: "ci", StepName: types.StepCI, Status: types.StepStatusPending},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusSkipped},
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusFailed},
		{ID: "rebase", StepName: types.StepRebase, Status: types.StepStatusCompleted},
		{ID: "lint", StepName: types.StepLint, Status: types.StepStatusAwaitingApproval},
		{ID: "push", StepName: types.StepPush, Status: types.StepStatusCompleted},
		{ID: "pr", StepName: types.StepPR, Status: types.StepStatusRunning},
		{ID: "intent", StepName: types.StepIntent, Status: types.StepStatusSkipped},
	}

	got, _ := BuildPipelineSummary(steps, nil, testPipelineHeadSHA)
	repeated, _ := BuildPipelineSummary(steps, nil, testPipelineHeadSHA)
	if got != repeated {
		t.Fatalf("attestation must be deterministic:\nfirst:\n%s\nsecond:\n%s", got, repeated)
	}

	const prefix = "<!-- no-mistakes-pipeline-attestation:v1 "
	start := strings.Index(got, prefix)
	if start < 0 {
		t.Fatalf("pipeline summary missing attestation comment:\n%s", got)
	}
	end := strings.Index(got[start:], " -->")
	if end < 0 {
		t.Fatalf("attestation comment is not closed:\n%s", got)
	}
	var attestation struct {
		HeadSHA string `json:"head_sha"`
		Steps   []struct {
			Step   types.StepName   `json:"step"`
			Status types.StepStatus `json:"status"`
		} `json:"steps"`
	}
	payload := got[start+len(prefix) : start+end]
	if err := json.Unmarshal([]byte(payload), &attestation); err != nil {
		t.Fatalf("attestation payload must be JSON: %v\n%s", err, payload)
	}
	if attestation.HeadSHA != testPipelineHeadSHA {
		t.Fatalf("attested head = %q, want %q", attestation.HeadSHA, testPipelineHeadSHA)
	}

	want := []struct {
		step   types.StepName
		status types.StepStatus
	}{
		{types.StepIntent, types.StepStatusSkipped},
		{types.StepRebase, types.StepStatusCompleted},
		{types.StepReview, types.StepStatusCompleted},
		{types.StepTest, types.StepStatusFailed},
		{types.StepDocument, types.StepStatusSkipped},
		{types.StepLint, types.StepStatusAwaitingApproval},
		{types.StepPush, types.StepStatusCompleted},
		{types.StepPR, types.StepStatusRunning},
		{types.StepCI, types.StepStatusPending},
	}
	if len(attestation.Steps) != len(want) {
		t.Fatalf("attested %d steps, want %d: %+v", len(attestation.Steps), len(want), attestation.Steps)
	}
	for i, wantStep := range want {
		if gotStep := attestation.Steps[i]; gotStep.Step != wantStep.step || gotStep.Status != wantStep.status {
			t.Errorf("attested step %d = (%q, %q), want (%q, %q)", i, gotStep.Step, gotStep.Status, wantStep.step, wantStep.status)
		}
	}

	if signatureAt, attestationAt := strings.Index(got, noMistakesPRSignature), strings.Index(got, prefix); signatureAt < 0 || attestationAt < signatureAt {
		t.Fatalf("attestation must follow the existing signature:\n%s", got)
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

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

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
	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

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
	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

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
	md, risk := BuildPipelineSummary(nil, nil, testPipelineHeadSHA)
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
	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)
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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)
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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)
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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)
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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)

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

func TestBuildTestingSummary_ArtifactContentCannotSpoofFoldBoundary(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Server log","content":"line one\n### Fake Heading\nline two"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummary(steps, rounds)
	t.Logf("rendered testing markdown:\n%s", md)

	if strings.Contains(md, "\n### Fake Heading") {
		t.Fatalf("embedded artifact content was not escaped, spoofs the PR-body fold-boundary marker:\n%s", md)
	}
	if !strings.Contains(md, "line one") || !strings.Contains(md, "line two") {
		t.Fatalf("expected the artifact content to stay intact:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_ArtifactContentCannotSpoofFoldBoundary(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Server log","content":"line one\n### Fake Heading\nline two"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	for _, tc := range []struct {
		name     string
		provider scm.Provider
	}{
		{"github", scm.ProviderGitHub},
		{"bitbucket", scm.ProviderBitbucket},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := BuildTestingSummaryForPRWithProvider(steps, rounds, "https://github.com/example/widgets.git", "abc123", t.TempDir(), "", nil, tc.provider)
			t.Logf("rendered PR testing markdown:\n%s", md)

			if strings.Contains(md, "\n### Fake Heading") {
				t.Fatalf("embedded artifact content was not escaped, spoofs the PR-body fold-boundary marker:\n%s", md)
			}
			if !strings.Contains(md, "line one") || !strings.Contains(md, "line two") {
				t.Fatalf("expected the artifact content to stay intact:\n%s", md)
			}
		})
	}
}

func TestBuildTestingSummaryForPR_RendersEvidenceArtifactsCompactly(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":"artifacts/checkout.png"},{"kind":"log","label":"Server log","path":"artifacts/server.log"},{"kind":"log","label":"Placement rectangle evidence","content":"{\"button\":{\"top\":169,\"left\":248,\"right\":272,\"bottom\":193}}"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "- Evidence: [Checkout screenshot](https://github.com/example/widgets/blob/abc123/artifacts/checkout.png)") {
		t.Fatalf("expected screenshot path to render as compact GitHub blob link, got:\n%s", md)
	}
	if !strings.Contains(md, "[Server log](https://github.com/example/widgets/blob/abc123/artifacts/server.log)") {
		t.Fatalf("expected log path to render as GitHub blob URL, got:\n%s", md)
	}
	if !strings.Contains(md, "<details>\n<summary>Evidence: Placement rectangle evidence</summary>") || !strings.Contains(md, "```text\n{\"button\":{\"top\":169,\"left\":248,\"right\":272,\"bottom\":193}}\n```") {
		t.Fatalf("expected content artifact to render in collapsible details, got:\n%s", md)
	}
	for _, broken := range []string{"![Checkout screenshot]", "raw.githubusercontent.com", "](artifacts/checkout.png)", "](artifacts/server.log)"} {
		if strings.Contains(md, broken) {
			t.Fatalf("did not expect broken or noisy artifact rendering %q, got:\n%s", broken, md)
		}
	}
}

func TestBuildTestingSummaryForPR_BitbucketCloudOmitsHTMLAndKeepsEvidence(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := writeTempEvidenceFile(t, evidenceRoot, "server.log", []byte("POST /checkout 200"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Placement rectangle evidence","content":"{\"button\":{\"top\":169}}"},{"kind":"video","label":"Checkout recording","url":"https://example.com/checkout.mp4"},{"kind":"log","label":"Server log","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPRWithProvider(steps, rounds, "https://bitbucket.org/example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil, scm.ProviderBitbucket)

	for _, want := range []string{
		"## Testing",
		"Evidence was collected.",
		"### Evidence: Placement rectangle evidence",
		"```text\n{\"button\":{\"top\":169}}\n```",
		"[Checkout recording](https://example.com/checkout.mp4)",
		"### Evidence: Server log",
		"```text\nPOST /checkout 200\n```",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %q in Bitbucket testing summary, got:\n%s", want, md)
		}
	}
	for _, leak := range []string{"<details>", "<summary>", "<code>", "<video", "![Checkout recording]"} {
		if strings.Contains(md, leak) {
			t.Errorf("Bitbucket testing leaked %q:\n%s", leak, md)
		}
	}
}

func TestBuildTestingSummaryForPR_BitbucketKeepsHTMLMentioningSummaryAsProse(t *testing.T) {
	t.Parallel()
	summary := "Bitbucket render is clean of <details>, <summary>, <code>, <video>, and the attestation HTML comment."
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":%q}`, summary)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPRWithProvider(steps, rounds, "https://bitbucket.org/example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderBitbucket)

	if !strings.Contains(md, "## Testing\n\n"+summary) {
		t.Fatalf("expected Bitbucket testing summary to stay prose, got:\n%s", md)
	}
	if strings.Contains(md, "```") || strings.Contains(md, "`"+summary+"`") {
		t.Fatalf("Bitbucket testing summary must not be wrapped as code just because it mentions HTML tags, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_RendersLocalTempVisualArtifactPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := filepath.Join(evidenceRoot, "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot, evidenceRoot, nil)
	t.Logf("rendered PR testing markdown:\n%s", md)

	want := "- Evidence: Checkout screenshot (local file: <code>" + html.EscapeString(localPath) + "</code>)"
	if !strings.Contains(md, want) {
		t.Fatalf("expected local temp screenshot path to render as local evidence, got:\n%s", md)
	}
	for _, broken := range []string{"![Checkout screenshot]", "github.com/example/widgets/blob/abc123/"} {
		if strings.Contains(md, broken) {
			t.Fatalf("did not expect local temp artifact to be rendered as a visual or GitHub link %q, got:\n%s", broken, md)
		}
	}
}

func TestBuildTestingSummaryForPR_PreservesCaptionedLocalVisualArtifactPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := filepath.Join(evidenceRoot, "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q,"content":"Checkout completed visually."}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot, evidenceRoot, nil)
	t.Logf("rendered PR testing markdown:\n%s", md)

	wantSource := "Source: Checkout screenshot (local file: <code>" + html.EscapeString(localPath) + "</code>)"
	if !strings.Contains(md, wantSource) {
		t.Fatalf("expected captioned local temp screenshot path to be preserved, got:\n%s", md)
	}
	if !strings.Contains(md, "```text\nCheckout completed visually.\n```") {
		t.Fatalf("expected caption to render safely in text fence, got:\n%s", md)
	}
	if strings.Contains(md, "github.com/example/widgets/blob/abc123/") {
		t.Fatalf("did not expect local temp artifact to be rendered as a GitHub link, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_PrefersArtifactURLOverLocalPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := filepath.Join(evidenceRoot, "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","url":"https://example.com/checkout.png","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot, evidenceRoot, nil)

	if !strings.Contains(md, "- Evidence: [Checkout screenshot](https://example.com/checkout.png)") {
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

// writeTempEvidenceFile creates a file under the caller's evidence root, which
// is the only absolute location outside the repository that artifact paths may
// reference. The root is passed in rather than derived here so each test owns
// one directory and can hand the same value to BuildTestingSummaryForPR.
func writeTempEvidenceFile(t *testing.T, evidenceRoot, name string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(evidenceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(evidenceRoot, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildTestingSummaryForPR_EmbedsLocalTextEvidenceContent(t *testing.T) {
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	fileBody := "RENDERED WIZARD SCREEN\n  > Claude\n  > Codex\nGitHub source selected"
	localPath := writeTempEvidenceFile(t, evidenceRoot, "init-wizard-rendered-screens.txt", []byte(fileBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Rendered setup wizard screens","path":%q,"content":"Shows agent auto-detect with Claude and Codex listed individually."}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil)
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
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	fileBody := "rendered wizard evidence"
	localPath := writeTempEvidenceFile(t, evidenceRoot, "wizard.txt", []byte(fileBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Wizard log","url":"https://example.com/artifacts/wizard.txt","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil)

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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot, "", nil)
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

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot, "", nil)

	if strings.Contains(md, secret) || strings.Contains(md, "```text") {
		t.Fatalf("did not expect repo-relative secret content to be embedded, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_RendersFileCaptionAsText(t *testing.T) {
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	fileBody := "safe evidence body"
	localPath := writeTempEvidenceFile(t, evidenceRoot, "caption.txt", []byte(fileBody))
	caption := "<img src=x onerror=alert(1)>\n[leak](file:///tmp/secret)"
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Captioned log","path":%q,"content":%q}]}`, localPath, caption)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil)

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
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	head := strings.Repeat("HEAD-LINE\n", 50)
	tail := strings.Repeat("TAIL-LINE\n", 50)
	fileBody := head + strings.Repeat("X", 40*1024) + tail
	localPath := writeTempEvidenceFile(t, evidenceRoot, "big.txt", []byte(fileBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Large log","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil)

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
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	firstBody := strings.Repeat("first evidence line\n", 700)
	secondBody := strings.Repeat("second evidence line\n", 700)
	thirdBody := strings.Repeat("third evidence line\n", 700)
	firstPath := writeTempEvidenceFile(t, evidenceRoot, "first.txt", []byte(firstBody))
	secondPath := writeTempEvidenceFile(t, evidenceRoot, "second.txt", []byte(secondBody))
	thirdPath := writeTempEvidenceFile(t, evidenceRoot, "third.txt", []byte(thirdBody))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"First log","path":%q},{"label":"Second log","path":%q},{"label":"Third log","path":%q}]}`, firstPath, secondPath, thirdPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil)

	if !strings.Contains(md, firstBody) || !strings.Contains(md, secondBody) {
		t.Fatalf("expected earlier evidence to embed before budget is exhausted, got:\n%s", md[:min(len(md), 600)])
	}
	if strings.Contains(md, thirdBody) {
		t.Fatalf("did not expect evidence beyond the total budget to be embedded")
	}
	if !strings.Contains(md, "- Evidence: Third log (local file: <code>"+html.EscapeString(thirdPath)+"</code>)") {
		t.Fatalf("expected evidence beyond the total budget to fall back to a local source, got:\n%s", md)
	}
}

func TestBuildTestingSummaryForPR_FallsBackForBinaryEvidence(t *testing.T) {
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := writeTempEvidenceFile(t, evidenceRoot, "capture.dat", []byte{0x00, 0x01, 0x02, 0xff, 0x00})
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Binary capture","path":%q}]}`, localPath)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil)

	if !strings.Contains(md, "- Evidence: Binary capture (local file: <code>"+html.EscapeString(localPath)+"</code>)") {
		t.Fatalf("expected binary evidence to fall back to a local file reference, got:\n%s", md)
	}
	if strings.Contains(md, "```text") {
		t.Fatalf("did not expect binary content to be embedded as text, got:\n%s", md)
	}
}
