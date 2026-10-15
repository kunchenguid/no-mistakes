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
	"github.com/kunchenguid/no-mistakes/internal/testartifact"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testPipelineHeadSHA = "0123456789abcdef0123456789abcdef01234567"

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
	t.Parallel()
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
	t.Parallel()
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
	findings := `{"findings":[],"summary":"","artifacts":[{"kind":"text-quote","label":"Rendered PR markdown","content":"## Testing\n\n- Evidence captured"}]}`
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
	findings := `{"findings":[],"summary":"","testing_summary":"Checkout success was verified visually.","tested":["manual checkout flow"],"artifacts":[{"kind":"screenshot","shows":"ui","label":"Checkout success screenshot","path":"artifacts/checkout-success.png"},{"kind":"text-quote","label":"Checkout server log","content":"POST /checkout 200\nreceipt=ok"}]}`
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
	failedRound := `{"findings":[{"id":"test-1","severity":"warning","description":"checkout failed","action":"auto-fix"}],"summary":"checkout failed","testing_summary":"Checkout failed before fix.","tested":["broken checkout flow"],"artifacts":[{"kind":"screenshot","shows":"ui","label":"Broken checkout screenshot","path":"artifacts/broken-checkout.png"}]}`
	passedRound := `{"findings":[],"summary":"","testing_summary":"Checkout passed after fix.","tested":["fixed checkout flow"],"artifacts":[{"kind":"screenshot","shows":"ui","label":"Fixed checkout screenshot","path":"artifacts/fixed-checkout.png"}]}`
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
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","shows":"ui","label":"Absolute path","path":"/Users/alice/project/artifacts/leak.png"},{"kind":"screenshot","shows":"ui","label":"Parent path","path":"../secret.png"},{"kind":"screenshot","shows":"ui","label":"Markdown injection","url":"https://example.com/evidence.png)\n![leak](file:///tmp/secret"},{"kind":"screenshot","shows":"ui","label":"Safe path","path":"artifacts/safe.png"},{"kind":"image","shows":"chart","label":"Safe URL","url":"https://example.com/chart.png"},{"kind":"log","label":"Log URL","url":"https://example.com/log.txt"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummary(steps, rounds)

	for _, unsafe := range []string{"/Users/alice", "../secret.png", "Markdown injection", "file:///tmp/secret", "Log URL", "example.com/log.txt"} {
		if strings.Contains(md, unsafe) {
			t.Fatalf("did not expect unsafe target content %q, got:\n%s", unsafe, md)
		}
	}
	if !strings.Contains(md, "![Safe path](artifacts/safe.png)") || !strings.Contains(md, "![Safe URL](https://example.com/chart.png)") {
		t.Fatalf("expected safe artifact targets to render, got:\n%s", md)
	}
}

func TestRenderTestingArtifact_DefensivelyEscapesFoldBoundary(t *testing.T) {
	t.Parallel()
	md := renderTestingArtifact(testartifact.Publication{
		Kind:  testartifact.KindTextQuote,
		Label: "Quote",
		Quote: "line one\n### Fake Heading\nline two",
	}, testingSummaryOptions{})

	if strings.Contains(md, "\n### Fake Heading") || !strings.Contains(md, "\n\\### Fake Heading") {
		t.Fatalf("direct publication was not defensively escaped:\n%s", md)
	}
}

func TestRenderTestingArtifact_PreservesTerminalNewline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		quote string
		want  string
	}{
		{name: "without terminal newline", quote: "line", want: "```text\nline\n```\n"},
		{name: "with terminal newline", quote: "line\n", want: "```text\nline\n```\n"},
		{name: "with two terminal newlines", quote: "line\n\n", want: "```text\nline\n\n```\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderTestingArtifact(testartifact.Publication{Kind: testartifact.KindTextQuote, Label: "Quote", Quote: tc.quote}, testingSummaryOptions{})
			if !strings.HasSuffix(got, tc.want) {
				t.Fatalf("rendered quote = %q, want suffix %q", got, tc.want)
			}
		})
	}
}

func TestBuildTestingSummaryForPR_ClosingReferencesAreWithheldBeforeAssembly(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"text-quote","label":"Issue result","content":"Closes #42"}]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab} {
		testing := BuildTestingSummaryForPRWithProvider(steps, rounds, "", "", t.TempDir(), "", nil, provider)
		body := appendGeneratedSectionsToCleanBody("", "", testing, "", provider)
		if strings.Contains(body, "Closes #42") || strings.Contains(body, "Closes `#42`") || strings.Contains(body, "Closes&#32;#42") {
			t.Fatalf("provider %q rewrote rather than withheld the quote:\n%s", provider, body)
		}
		if !strings.Contains(body, "quote refused by the publication check") {
			t.Fatalf("provider %q did not report the withheld quote:\n%s", provider, body)
		}
	}
}

func TestBuildTestingSummaryForPR_ArtifactContentCannotSpoofFoldBoundary(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"text-quote","label":"Server log","content":"line one\n### Fake Heading\nline two"}]}`
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

			if strings.Contains(md, "Fake Heading") || strings.Contains(md, "line one") || strings.Contains(md, "line two") {
				t.Fatalf("structural stored quote reached the PR body:\n%s", md)
			}
			if !strings.Contains(md, "quote refused by the publication check") {
				t.Fatalf("expected the structural stored quote to be withheld:\n%s", md)
			}
		})
	}
}

func TestBuildTestingSummaryForPR_RendersEvidenceArtifactsCompactly(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","shows":"ui","label":"Checkout screenshot","path":"artifacts/checkout.png"},{"kind":"log","label":"Server log","path":"artifacts/server.log"},{"kind":"text-quote","label":"Placement rectangle evidence","content":"{\"button\":{\"top\":169,\"left\":248,\"right\":272,\"bottom\":193}}"}]}`
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
	if strings.Contains(md, "Server log") || strings.Contains(md, "server.log") {
		t.Fatalf("a log is never linked or named in the PR body; with no readable file it publishes nothing, got:\n%s", md)
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

func TestBuildTestingSummaryForPR_SeparatesMixedEvidenceBlocks(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"text-quote","label":"Inline first","content":"first output"},{"kind":"image","shows":"ui","label":"Linked first","url":"https://example.com/first.png"},{"kind":"text-quote","label":"Inline second","content":"second output"},{"kind":"image","shows":"ui","label":"Linked second","url":"https://example.com/second.png"},{"kind":"image","shows":"ui","label":"Linked third","url":"https://example.com/third.png"}]}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil)

	for _, want := range []string{
		"</details>\n\n- Evidence: [Linked first]",
		"- Evidence: [Linked first](https://example.com/first.png)\n\n<details>",
		"</details>\n\n- Evidence: [Linked second]",
		"- Evidence: [Linked second](https://example.com/second.png)\n- Evidence: [Linked third]",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected mixed evidence boundary %q, got:\n%s", want, md)
		}
	}
}

func TestBuildTestingSummaryForPR_BitbucketCloudOmitsHTMLAndKeepsEvidence(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := writeTempEvidenceFile(t, evidenceRoot, "server.log", []byte("POST /checkout 200"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"text-quote","label":"Placement rectangle evidence","content":"{\"button\":{\"top\":169}}"},{"kind":"video","shows":"ui","label":"Checkout recording","url":"https://example.com/checkout.mp4"},{"kind":"log","label":"Server log","path":%q}]}`, localPath)
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
		"- Evidence: Log 1 (not shown in this PR): 1 line, 18 bytes, complete; phases: ready 0, error 0, warning 0, shutdown 0",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %q in Bitbucket testing summary, got:\n%s", want, md)
		}
	}
	for _, leak := range []string{"<details>", "<summary>", "<code>", "<video", "![Checkout recording]", "POST /checkout", "Server log"} {
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
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","shows":"ui","label":"Checkout screenshot","path":%q}]}`, localPath)
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

func TestBuildTestingSummaryForPR_MediaCaptionIsNotPublished(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := filepath.Join(evidenceRoot, "checkout.png")
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","shows":"ui","label":"Checkout screenshot","path":%q,"content":"Checkout completed visually."}]}`, localPath)

	md := testingSummaryFor(t, raw, repoRoot, evidenceRoot, scm.ProviderGitHub)

	want := "- Evidence: Checkout screenshot (local file: <code>" + html.EscapeString(localPath) + "</code>)"
	if !strings.Contains(md, want) {
		t.Fatalf("expected the declared screenshot's local reference, got:\n%s", md)
	}
	assertNoneOf(t, md, "Checkout completed visually.", "```", "github.com/example/widgets/blob/abc123/")
}
func TestBuildTestingSummaryForPR_PrefersArtifactURLOverLocalPath(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	localPath := filepath.Join(evidenceRoot, "checkout.png")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"screenshot","shows":"ui","label":"Checkout screenshot","url":"https://example.com/checkout.png","path":%q}]}`, localPath)
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

// testingSummaryFor renders the PR Testing section for one recorded test
// step whose findings are raw.
func testingSummaryFor(t *testing.T, raw, repoRoot, evidenceRoot string, provider scm.Provider) string {
	t.Helper()
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &raw},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &raw, DurationMS: 300}},
	}
	md := BuildTestingSummaryForPRWithProvider(steps, rounds, "git@github.com:example/widgets.git", "abc123", repoRoot, evidenceRoot, nil, provider)
	t.Logf("rendered PR testing markdown:\n%s", md)
	return md
}

func assertNoneOf(t *testing.T, md string, leaks ...string) {
	t.Helper()
	for _, leak := range leaks {
		if strings.Contains(md, leak) {
			t.Fatalf("PR body carries %q:\n%s", leak, md)
		}
	}
}

// syntheticServerLog is a boot log shaped like the one that leaked a
// credential through an inlined log artifact. Every value is synthetic.
const syntheticServerLog = "Migrating database\n" +
	"Database: /home/example-user/project/data/live.db\n" +
	"Server ready at http://localhost:43210\n" +
	"bootstrap password: Synth3tic-Not-A-Real-Secret-0000\n" +
	"worker pid=48213 started\n" +
	"run 01ARZ3NDEKTSV4RRFFQ69G5FAV session 123e4567-e89b-42d3-a456-426614174000\n" +
	"Shutdown complete\n"

var syntheticServerLogValues = []string{
	"Synth3tic", "bootstrap password", "example-user", "live.db", "localhost", "43210", "48213",
	"01ARZ3NDEKTSV4RRFFQ69G5FAV", "123e4567-e89b-42d3-a456-426614174000", "Migrating", "Shutdown complete",
}

// The leak this contract exists for: a log artifact's bytes must never reach
// the PR body, on any provider flavor, whatever else the record carries.
func TestBuildTestingSummaryForPR_LogPublishesOnlyItsSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		provider scm.Provider
	}{
		{"github", scm.ProviderGitHub},
		{"bitbucket", scm.ProviderBitbucket},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
			localPath := writeTempEvidenceFile(t, evidenceRoot, "server.log", []byte(syntheticServerLog))
			raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Live boot for Patient Example","path":%q,"url":"https://example.com/server.log","content":%q}]}`, localPath, syntheticServerLog)

			md := testingSummaryFor(t, raw, t.TempDir(), evidenceRoot, tc.provider)

			want := "- Evidence: Log 1 (not shown in this PR): 7 lines, " + fmt.Sprint(len(syntheticServerLog)) + " bytes, complete; phases: ready 2, error 0, warning 0, shutdown 1"
			if !strings.Contains(md, want) {
				t.Fatalf("expected the log's machine summary %q, got:\n%s", want, md)
			}
			assertNoneOf(t, md, syntheticServerLogValues...)
			assertNoneOf(t, md, localPath, "server.log", "Patient Example", "example.com", "```")
		})
	}
}

// A record written before the typed contract (no kind, or kind log with an
// inline copy and a caption) renders exactly like a new log: summary only.
func TestBuildTestingSummaryForPR_LegacyTextEvidenceIsSummarizedNotEmbedded(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	fileBody := "RENDERED WIZARD SCREEN\n  > Claude\n  > Codex\nGitHub source selected"
	localPath := writeTempEvidenceFile(t, evidenceRoot, "init-wizard-rendered-screens.txt", []byte(fileBody))
	caption := "<img src=x onerror=alert(1)>\n[leak](file:///tmp/secret)"
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Rendered setup wizard screens","path":%q,"content":%q},{"kind":"log","label":"Wizard log","url":"https://example.com/artifacts/wizard.txt","path":%q}]}`, localPath, caption, localPath)

	md := testingSummaryFor(t, raw, t.TempDir(), evidenceRoot, scm.ProviderGitHub)

	if !strings.Contains(md, "- Evidence: Log 1 (not shown in this PR): 4 lines, ") || !strings.Contains(md, "- Evidence: Log 2 (not shown in this PR): 4 lines, ") {
		t.Fatalf("expected both legacy records summarized, got:\n%s", md)
	}
	assertNoneOf(t, md, "RENDERED WIZARD SCREEN", "Claude", "img src", "onerror", "file:///tmp/secret", "example.com", "Rendered setup wizard screens", "Wizard log", localPath, "```")
}

func TestBuildTestingSummaryForPR_RepoLogIsSummarizedNotLinked(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fileBody := "POST /checkout 200\nreceipt=ok"
	if err := os.WriteFile(filepath.Join(repoRoot, "artifacts", "server.log"), []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Server log","path":"artifacts/server.log"}]}`

	md := testingSummaryFor(t, raw, repoRoot, "", scm.ProviderGitHub)

	if !strings.Contains(md, "- Evidence: Log 1 (not shown in this PR): 2 lines, 29 bytes, complete") {
		t.Fatalf("expected the repository log summarized, got:\n%s", md)
	}
	assertNoneOf(t, md, "POST /checkout", "receipt=ok", "artifacts/server.log", "github.com/example/widgets/blob", "```")
}

func TestBuildTestingSummaryForPR_LargeLogSummarizesWithoutContent(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	fileBody := strings.Repeat("HEAD-LINE\n", 50) + strings.Repeat("X", 40*1024) + "\n" + strings.Repeat("TAIL-LINE error\n", 50)
	localPath := writeTempEvidenceFile(t, evidenceRoot, "big.txt", []byte(fileBody))
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Large log","path":%q}]}`, localPath)

	md := testingSummaryFor(t, raw, t.TempDir(), evidenceRoot, scm.ProviderGitHub)

	want := fmt.Sprintf("- Evidence: Log 1 (not shown in this PR): 101 lines, %d bytes, complete; phases: ready 0, error 50, warning 0, shutdown 0", len(fileBody))
	if !strings.Contains(md, want) {
		t.Fatalf("expected %q, got:\n%s", want, md)
	}
	assertNoneOf(t, md, "HEAD-LINE", "TAIL-LINE", "XXXX", "truncated")
	if len(md) > 1024 {
		t.Fatalf("a log summary must stay small whatever the file size, got %d bytes", len(md))
	}
}

func TestBuildTestingSummaryForPR_EveryLogSummarizesIndependently(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	var artifacts []string
	for _, name := range []string{"first", "second", "third"} {
		path := writeTempEvidenceFile(t, evidenceRoot, name+".txt", []byte(strings.Repeat(name+" evidence line\n", 700)))
		artifacts = append(artifacts, fmt.Sprintf(`{"kind":"log","label":"%s log","path":%q}`, name, path))
	}
	raw := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[` + strings.Join(artifacts, ",") + `]}`

	md := testingSummaryFor(t, raw, t.TempDir(), evidenceRoot, scm.ProviderGitHub)

	for _, n := range []string{"1", "2", "3"} {
		if !strings.Contains(md, "- Evidence: Log "+n+" (not shown in this PR): 700 lines, ") {
			t.Fatalf("expected Log %s summarized, got:\n%s", n, md)
		}
	}
	assertNoneOf(t, md, "evidence line", "local file:")
}

// A log the summary cannot read (binary, missing, or outside the worktree and
// the run's evidence directory) publishes nothing at all, not a reference.
func TestBuildTestingSummaryForPR_UnsummarizableLogPublishesNothing(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	binary := writeTempEvidenceFile(t, evidenceRoot, "capture.dat", []byte{0x00, 0x01, 0x02, 0xff, 0x00})
	outside := writeTempEvidenceFile(t, filepath.Join(t.TempDir(), "elsewhere"), "server.log", []byte(syntheticServerLog))
	missing := filepath.Join(evidenceRoot, "gone.log")
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"label":"Binary capture","path":%q},{"kind":"log","label":"Outside","path":%q},{"kind":"log","label":"Missing","path":%q},{"kind":"log","label":"Inline only","content":%q}]}`, binary, outside, missing, syntheticServerLog)

	md := testingSummaryFor(t, raw, t.TempDir(), evidenceRoot, scm.ProviderGitHub)

	assertNoneOf(t, md, "Evidence:", "Log 1", "capture.dat", "local file:", "```")
	assertNoneOf(t, md, syntheticServerLogValues...)
}

// Media the agent did not declare, or declared as a terminal capture, is
// listed under a machine label and never shown, linked, or attached.
func TestBuildTestingSummaryForPR_TerminalAndUndeclaredMediaAreWithheld(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	terminal := writeTempEvidenceFile(t, evidenceRoot, "boot.png", []byte("png"))
	legacy := writeTempEvidenceFile(t, evidenceRoot, "legacy.png", []byte("png"))
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"image","shows":"terminal","label":"Boot log at localhost","path":%q},{"kind":"screenshot","label":"Legacy shot","path":%q},{"kind":"video","shows":"terminal","label":"CLI session","url":"https://example.com/cli.mp4"}]}`, terminal, legacy)

	md := testingSummaryFor(t, raw, t.TempDir(), evidenceRoot, scm.ProviderGitHub)

	for _, want := range []string{
		"- Evidence: Image 1 (not shown in this PR: terminal capture)",
		"- Evidence: Image 2 (not shown in this PR: content not declared)",
		"- Evidence: Video 1 (not shown in this PR: terminal capture)",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("expected %q, got:\n%s", want, md)
		}
	}
	assertNoneOf(t, md, "boot.png", "legacy.png", "Boot log", "Legacy shot", "CLI session", "example.com", "localhost", "![")
}

func TestBuildTestingSummaryForPR_TextQuotesAreCheckedAtRender(t *testing.T) {
	t.Parallel()
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"text-quote","label":"Heading","content":"## Done"},{"kind":"text-quote","label":"Stored before the check","content":%q}]}`, syntheticServerLog)

	md := testingSummaryFor(t, raw, t.TempDir(), "", scm.ProviderGitHub)

	if !strings.Contains(md, "<summary>Evidence: Heading</summary>") || !strings.Contains(md, "```text\n## Done\n```") {
		t.Fatalf("expected the clean quote inline, got:\n%s", md)
	}
	if !strings.Contains(md, "- Evidence: Quote 2 (not shown in this PR: quote refused by the publication check)") {
		t.Fatalf("expected the unsafe stored quote withheld, got:\n%s", md)
	}
	assertNoneOf(t, md, syntheticServerLogValues...)
	assertNoneOf(t, md, "Stored before the check")
}

func TestBuildPRTestingSummary_PutsEachAttachmentInItsOwnParagraph(t *testing.T) {
	t.Parallel()
	const (
		image   = `{"kind":"screenshot","shows":"ui","label":"Shot","path":"shot.png"}`
		image2  = `{"kind":"screenshot","shows":"ui","label":"Shot two","path":"shot2.png"}`
		video   = `{"kind":"video","shows":"ui","label":"Rec","path":"rec.mp4"}`
		details = `{"kind":"text-quote","label":"Inline","content":"output"}`
	)
	tests := []struct {
		name        string
		artifacts   []string
		attachments int
		upstreamURL string
		status      types.StepStatus
	}{
		{name: "image before outcome", artifacts: []string{image}, attachments: 1, status: types.StepStatusFailed},
		{name: "video before outcome", artifacts: []string{video}, attachments: 1, status: types.StepStatusFailed},
		{name: "image then video", artifacts: []string{image, video}, attachments: 2},
		{name: "video then image", artifacts: []string{video, image}, attachments: 2},
		{name: "image video image", artifacts: []string{image, video, image2}, attachments: 3},
		{name: "image and video next to details", artifacts: []string{details, image, video, details}, attachments: 2},
		{name: "evidence link then video", artifacts: []string{image, video}, attachments: 2, upstreamURL: "git@github.com:example/widgets.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[` + strings.Join(tt.artifacts, ",") + `]}`
			status := tt.status
			if status == "" {
				status = types.StepStatusCompleted
			}
			steps := []*db.StepResult{
				{ID: "s1", StepName: types.StepTest, Status: status, FindingsJSON: &findings},
			}
			rounds := map[string][]*db.StepRound{
				"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
			}
			attachments := map[string]string{
				"shot.png":  "https://github.com/user-attachments/assets/shot",
				"shot2.png": "https://github.com/user-attachments/assets/shot2",
				"rec.mp4":   "https://github.com/user-attachments/assets/rec",
			}

			md := buildPRTestingSummary(steps, rounds, tt.upstreamURL, "abc123", t.TempDir(), "", nil, scm.ProviderUnknown, attachments)

			lines := strings.Split(md, "\n")
			found := 0
			for i, line := range lines {
				if !strings.Contains(line, "https://github.com/user-attachments/assets/") {
					continue
				}
				found++
				if i == 0 || lines[i-1] != "" {
					t.Errorf("attachment line %q is not preceded by a blank line, got:\n%s", line, md)
				}
				// A list item interrupts a paragraph, so the attachment's own
				// evidence link may follow it directly.
				if i+1 < len(lines) && lines[i+1] != "" && !strings.HasPrefix(lines[i+1], "- Evidence: [") {
					t.Errorf("attachment line %q is not followed by a blank line, got:\n%s", line, md)
				}
			}
			if found != tt.attachments {
				t.Errorf("expected %d attachment lines, found %d, got:\n%s", tt.attachments, found, md)
			}
			if tt.status == types.StepStatusFailed && !strings.Contains(md, "\n\n- Outcome:") {
				t.Errorf("expected a blank line before the outcome, got:\n%s", md)
			}
		})
	}
}

func TestBuildPRTestingSummary_SeparatesDetailsFromAdjacentAttachments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		artifacts string
		status    types.StepStatus
		want      string
	}{
		{
			name:      "outcome after details",
			artifacts: `{"kind":"text-quote","label":"Inline","content":"output"}`,
			status:    types.StepStatusFailed,
			want:      "</details>\n\n- Outcome:",
		},
		{
			name:      "image after details",
			artifacts: `{"kind":"text-quote","label":"Inline","content":"output"},{"kind":"screenshot","shows":"ui","label":"Shot","path":"shot.png"}`,
			want:      "</details>\n\n![Shot](https://github.com/user-attachments/assets/shot)",
		},
		{
			name:      "video after details",
			artifacts: `{"kind":"text-quote","label":"Inline","content":"output"},{"kind":"video","shows":"ui","label":"Rec","path":"rec.mp4"}`,
			want:      "</details>\n\nhttps://github.com/user-attachments/assets/rec",
		},
		{
			name:      "image before details",
			artifacts: `{"kind":"screenshot","shows":"ui","label":"Shot","path":"shot.png"},{"kind":"text-quote","label":"Inline","content":"output"}`,
			want:      "![Shot](https://github.com/user-attachments/assets/shot)\n- Evidence: [Shot](https://github.com/example/widgets/blob/abc123/shot.png)\n\n<details>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := `{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[` + tt.artifacts + `]}`
			status := tt.status
			if status == "" {
				status = types.StepStatusCompleted
			}
			steps := []*db.StepResult{
				{ID: "s1", StepName: types.StepTest, Status: status, FindingsJSON: &findings},
			}
			rounds := map[string][]*db.StepRound{
				"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
			}
			attachments := map[string]string{
				"shot.png": "https://github.com/user-attachments/assets/shot",
				"rec.mp4":  "https://github.com/user-attachments/assets/rec",
			}

			md := buildPRTestingSummary(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), "", nil, scm.ProviderUnknown, attachments)

			if !strings.Contains(md, tt.want) {
				t.Errorf("expected %q, got:\n%s", tt.want, md)
			}
		})
	}
}
