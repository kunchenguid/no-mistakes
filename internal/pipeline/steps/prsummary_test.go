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
