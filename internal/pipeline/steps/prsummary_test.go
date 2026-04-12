package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineSummary_AllClean(t *testing.T) {
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
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "## Pipeline") {
		t.Error("missing Pipeline heading")
	}
	// Clean steps should show checkmark
	if !strings.Contains(md, "✅") {
		t.Error("expected checkmark for clean steps")
	}
	// Clean run should have no <details> blocks
	if strings.Contains(md, "<details>") {
		t.Error("clean run should not have details blocks")
	}
}

func TestBuildPipelineSummary_ReviewWithRisk(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"warning","file":"cmd/main.go","line":10,"description":"potential nil deref"}],"summary":"1 warning","risk_level":"low","risk_rationale":"straightforward refactor with no behavioral changes"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "low risk") {
		t.Errorf("expected 'low risk' in output, got:\n%s", md)
	}
	if !strings.Contains(md, "straightforward refactor") {
		t.Errorf("expected risk rationale in output, got:\n%s", md)
	}
	// Review with findings should have a details block
	if !strings.Contains(md, "<details>") {
		t.Errorf("expected details block for review findings, got:\n%s", md)
	}
	if !strings.Contains(md, "potential nil deref") {
		t.Errorf("expected finding description in details, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_AutoFix(t *testing.T) {
	findings1 := `{"findings":[{"id":"lint-1","severity":"error","file":"pkg/foo.go","line":18,"description":"unused import"},{"id":"lint-2","severity":"warning","file":"pkg/bar.go","line":35,"description":"missing error check"}],"summary":"2 issues"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepLint, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &findings1, DurationMS: 800},
			{Round: 2, Trigger: "auto_fix", DurationMS: 600},
		},
	}
	md := BuildPipelineSummary(steps, rounds)

	// Should show wrench emoji for auto-fixed
	if !strings.Contains(md, "🔧") {
		t.Errorf("expected wrench emoji for auto-fixed step, got:\n%s", md)
	}
	// Status line should mention auto-fixed
	if !strings.Contains(md, "auto-fixed") {
		t.Errorf("expected 'auto-fixed' in status line, got:\n%s", md)
	}
	// Should have details with round info
	if !strings.Contains(md, "Round 1") {
		t.Errorf("expected 'Round 1' in details, got:\n%s", md)
	}
	if !strings.Contains(md, "Round 2") {
		t.Errorf("expected 'Round 2' in details, got:\n%s", md)
	}
	if !strings.Contains(md, "unused import") {
		t.Errorf("expected finding description in round 1 details, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_MultiRoundWithUserFix(t *testing.T) {
	findings1 := `{"findings":[{"id":"test-1","severity":"error","file":"pkg/handler_test.go","line":42,"description":"expected 429 got 200"},{"id":"test-2","severity":"error","file":"pkg/handler_test.go","line":78,"description":"context deadline exceeded"}],"summary":"2 failures"}`
	findings2 := `{"findings":[{"id":"test-2","severity":"error","file":"pkg/handler_test.go","line":78,"description":"context deadline exceeded"}],"summary":"1 failure"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &findings1, DurationMS: 1000},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &findings2, DurationMS: 900},
			{Round: 3, Trigger: "user_fix", DurationMS: 700},
		},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "Round 3") {
		t.Errorf("expected 3 rounds in details, got:\n%s", md)
	}
	if !strings.Contains(md, "user-fix") || !strings.Contains(md, "auto-fix") {
		t.Errorf("expected both fix types mentioned, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_MultiRoundStillFailing(t *testing.T) {
	findings1 := `{"findings":[{"id":"lint-1","severity":"error","file":"pkg/foo.go","line":18,"description":"unused import"},{"id":"lint-2","severity":"warning","file":"pkg/bar.go","line":35,"description":"missing error check"}],"summary":"2 issues"}`
	findings2 := `{"findings":[{"id":"lint-2","severity":"warning","file":"pkg/bar.go","line":35,"description":"missing error check"}],"summary":"1 issue"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepLint, Status: types.StepStatusCompleted, FindingsJSON: &findings2},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &findings1, DurationMS: 800},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &findings2, DurationMS: 600},
		},
	}
	md := BuildPipelineSummary(steps, rounds)

	if strings.Contains(md, "auto-fixed") {
		t.Errorf("did not expect fixed status when final round still has findings, got:\n%s", md)
	}
	if !strings.Contains(md, "⚠️ **Lint** - 1 warning") {
		t.Errorf("expected final findings count in status line, got:\n%s", md)
	}
	if !strings.Contains(md, "Round 2") || !strings.Contains(md, "missing error check") {
		t.Errorf("expected final round details to remain visible, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_UsesFinalFindingsWithoutInitialRoundData(t *testing.T) {
	finalFindings := `{"findings":[{"id":"test-1","severity":"error","file":"pkg/handler_test.go","line":42,"description":"expected 429 got 200"}],"summary":"1 failure"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &finalFindings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", DurationMS: 1000},
		},
	}
	md := BuildPipelineSummary(steps, rounds)

	if strings.Contains(md, "passed") {
		t.Errorf("did not expect passed status when step result still has findings, got:\n%s", md)
	}
	if !strings.Contains(md, "⚠️ **Test** - 1 error") {
		t.Errorf("expected final findings count in status line, got:\n%s", md)
	}
	if strings.Contains(md, "<details>") {
		t.Errorf("did not expect details block without round findings data, got:\n%s", md)
	}
	if strings.Contains(md, "Round 1") {
		t.Errorf("did not expect round details without round findings data, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_SkippedStep(t *testing.T) {
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusSkipped},
		{ID: "s2", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {},
		"s2": {{Round: 1, Trigger: "initial", DurationMS: 300}},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "⏭️") {
		t.Errorf("expected skip emoji for skipped step, got:\n%s", md)
	}
	if !strings.Contains(md, "skipped") {
		t.Errorf("expected 'skipped' text for skipped step, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ExcludesPushPRBabysit(t *testing.T) {
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "s2", StepName: types.StepPush, Status: types.StepStatusCompleted},
		{ID: "s3", StepName: types.StepPR, Status: types.StepStatusCompleted},
		{ID: "s4", StepName: types.StepBabysit, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", DurationMS: 500}},
		"s2": {{Round: 1, Trigger: "initial", DurationMS: 100}},
		"s3": {{Round: 1, Trigger: "initial", DurationMS: 200}},
		"s4": {{Round: 1, Trigger: "initial", DurationMS: 300}},
	}
	md := BuildPipelineSummary(steps, rounds)

	// Push, PR, and Babysit should not appear in the summary
	if strings.Contains(md, "**Push**") || strings.Contains(md, "**PR**") || strings.Contains(md, "**Babysit**") {
		t.Errorf("should not include push/pr/babysit steps, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewApprovedWithWarnings(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"risky change"}],"summary":"1 warning","risk_level":"medium","risk_rationale":"changes error handling in critical path"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md := BuildPipelineSummary(steps, rounds)

	// Review with findings approved as-is should show warning emoji
	if !strings.Contains(md, "⚠️") {
		t.Errorf("expected warning emoji for review with findings, got:\n%s", md)
	}
	if !strings.Contains(md, "medium risk") {
		t.Errorf("expected 'medium risk' in output, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewUsesFinalCleanState(t *testing.T) {
	initialFindings := `{"findings":[{"id":"review-1","severity":"warning","description":"risky change"}],"summary":"1 warning","risk_level":"medium","risk_rationale":"initial risk rationale"}`
	finalFindings := `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"follow-up fixes reduced risk"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &finalFindings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &initialFindings, DurationMS: 1000},
			{Round: 2, Trigger: "user_fix", FindingsJSON: &finalFindings, DurationMS: 700},
		},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "✅ **Review** - low risk") {
		t.Errorf("expected clean final review status, got:\n%s", md)
	}
	if strings.Contains(md, "⚠️ **Review**") {
		t.Errorf("did not expect warning emoji after final clean review, got:\n%s", md)
	}
	if strings.Contains(md, "initial risk rationale") {
		t.Errorf("did not expect stale initial rationale, got:\n%s", md)
	}
	if !strings.Contains(md, "follow-up fixes reduced risk") {
		t.Errorf("expected final rationale in output, got:\n%s", md)
	}
	if !strings.Contains(md, "Round 2") {
		t.Errorf("expected review details to remain visible for multi-round review, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewShowsWarningForUnresolvedRiskWithoutFindings(t *testing.T) {
	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "⚠️ **Review** - medium risk") {
		t.Errorf("expected warning status for medium risk review without findings, got:\n%s", md)
	}
	if strings.Contains(md, "✅ **Review**") {
		t.Errorf("did not expect passed emoji for medium risk review, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ShowsParseFailureForInvalidRoundFindings(t *testing.T) {
	invalidFindings := `{"findings":[`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &invalidFindings, DurationMS: 1000}},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "failed to parse findings") {
		t.Errorf("expected parse failure message for invalid round findings, got:\n%s", md)
	}
	if strings.Contains(md, "**Round 1** - \n") {
		t.Errorf("did not expect blank round summary for invalid round findings, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_FindingSeverityEmoji(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"critical bug"},{"id":"review-2","severity":"warning","description":"minor issue"},{"id":"review-3","severity":"info","description":"suggestion"}],"summary":"3 findings","risk_level":"high","risk_rationale":"critical bug found"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "🚨") {
		t.Errorf("expected error emoji in details, got:\n%s", md)
	}
	if !strings.Contains(md, "ℹ️") {
		t.Errorf("expected info emoji in details, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_EmptySteps(t *testing.T) {
	md := BuildPipelineSummary(nil, nil)
	if md != "" {
		t.Errorf("expected empty string for nil steps, got: %q", md)
	}
}

func TestBuildPipelineSummary_RebaseWithConflicts(t *testing.T) {
	findings := `{"findings":[{"id":"rebase-1","severity":"warning","file":"pkg/foo.go","description":"merge conflict resolved by agent"}],"summary":"1 conflict resolved"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepRebase, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 2000}},
	}
	md := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "**Rebase**") {
		t.Errorf("expected Rebase in output, got:\n%s", md)
	}
	if !strings.Contains(md, "conflict") {
		t.Errorf("expected conflict mention in output, got:\n%s", md)
	}
}
