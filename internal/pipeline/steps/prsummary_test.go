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

func TestBuildPipelineSummary_ReviewWithRisk(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"warning","file":"cmd/main.go","line":10,"description":"potential nil deref"}],"summary":"1 warning","risk_level":"low","risk_rationale":"straightforward refactor with no behavioral changes"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md, risk := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "⚠️ **Review** - 1 warning") {
		t.Errorf("expected findings count in review line, got:\n%s", md)
	}
	if !strings.Contains(risk, "Low") || !strings.Contains(risk, "straightforward refactor") {
		t.Errorf("expected risk line with capitalized level and rationale, got: %q", risk)
	}
	if !strings.Contains(risk, "✅") {
		t.Errorf("expected checkmark emoji for low risk, got: %q", risk)
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
	t.Parallel()
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
	md, _ := BuildPipelineSummary(steps, rounds)

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

func TestBuildPipelineSummary_MultiRoundWithFollowUpFix(t *testing.T) {
	t.Parallel()
	findings1 := `{"findings":[{"id":"test-1","severity":"error","file":"pkg/handler_test.go","line":42,"description":"expected 429 got 200"},{"id":"test-2","severity":"error","file":"pkg/handler_test.go","line":78,"description":"context deadline exceeded"}],"summary":"2 failures"}`
	findings2 := `{"findings":[{"id":"test-2","severity":"error","file":"pkg/handler_test.go","line":78,"description":"context deadline exceeded"}],"summary":"1 failure"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &findings1, DurationMS: 1000},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &findings2, DurationMS: 900},
			{Round: 3, Trigger: "auto_fix", DurationMS: 700},
		},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "Round 3") {
		t.Errorf("expected 3 rounds in details, got:\n%s", md)
	}
	if strings.Contains(md, "user-fix") || strings.Contains(md, "user-fixed") {
		t.Errorf("did not expect user-fix wording, got:\n%s", md)
	}
	if !strings.Contains(md, "auto-fixed (2)") {
		t.Errorf("expected consolidated auto-fix count, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_MultiRoundStillFailing(t *testing.T) {
	t.Parallel()
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
	md, _ := BuildPipelineSummary(steps, rounds)

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
	t.Parallel()
	finalFindings := `{"findings":[{"id":"test-1","severity":"error","file":"pkg/handler_test.go","line":42,"description":"expected 429 got 200"}],"summary":"1 failure"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &finalFindings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", DurationMS: 1000},
		},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if strings.Contains(md, "passed") {
		t.Errorf("did not expect passed status when step result still has findings, got:\n%s", md)
	}
	if !strings.Contains(md, "⚠️ **Test** - 1 error") {
		t.Errorf("expected final findings count in status line, got:\n%s", md)
	}
	if !strings.Contains(md, "<summary>⚠️ **Test** - 1 error</summary>") {
		t.Errorf("expected unresolved test step to render as a collapsible summary, got:\n%s", md)
	}
	if !strings.Contains(md, "**Round 1** - findings not recorded") {
		t.Errorf("expected missing round findings data to be called out explicitly, got:\n%s", md)
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

func TestBuildPipelineSummary_EscapesFindingDescriptionsInDetails(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"warning","file":"cmd/main.go","line":10,"description":"break </details><summary>oops</summary> after"}],"summary":"1 warning","risk_level":"low","risk_rationale":"safe"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}

	md, _ := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "break &lt;/details&gt;&lt;summary&gt;oops&lt;/summary&gt; after") {
		t.Errorf("expected finding description to be HTML-escaped, got:\n%s", md)
	}
	if strings.Contains(md, "- ⚠️ `cmd/main.go:10` - break </details><summary>oops</summary> after") {
		t.Errorf("did not expect raw HTML in finding description, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewUsesFinalCleanState(t *testing.T) {
	t.Parallel()
	initialFindings := `{"findings":[{"id":"review-1","severity":"warning","description":"risky change"}],"summary":"1 warning","risk_level":"medium","risk_rationale":"initial risk rationale"}`
	finalFindings := `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"follow-up fixes reduced risk"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &finalFindings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &initialFindings, DurationMS: 1000},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &finalFindings, DurationMS: 700},
		},
	}
	md, risk := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "🔧 **Review**") {
		t.Errorf("expected fixed review status, got:\n%s", md)
	}
	if !strings.Contains(md, "auto-fixed") {
		t.Errorf("expected auto-fixed in review line, got:\n%s", md)
	}
	if strings.Contains(md, "user-fixed") {
		t.Errorf("did not expect user-fixed in review line, got:\n%s", md)
	}
	if strings.Contains(risk, "initial risk rationale") {
		t.Errorf("did not expect stale initial rationale in risk, got: %q", risk)
	}
	if !strings.Contains(risk, "follow-up fixes reduced risk") {
		t.Errorf("expected final rationale in risk, got: %q", risk)
	}
	if !strings.Contains(risk, "Low") {
		t.Errorf("expected capitalized Low in risk, got: %q", risk)
	}
	if !strings.Contains(risk, "✅") {
		t.Errorf("expected checkmark for low risk, got: %q", risk)
	}
	if !strings.Contains(md, "Round 2") {
		t.Errorf("expected review details to remain visible for multi-round review, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewShowsWarningForUnresolvedRiskWithoutFindings(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md, risk := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "⚠️ **Review** - medium risk") {
		t.Errorf("expected medium-risk review status when no findings, got:\n%s", md)
	}
	if strings.Contains(md, "✅ **Review** - passed") {
		t.Errorf("did not expect passed review status for medium risk, got:\n%s", md)
	}
	if !strings.Contains(risk, "Medium") || !strings.Contains(risk, "touches critical error handling") {
		t.Errorf("expected risk line with capitalized medium level, got: %q", risk)
	}
	if !strings.Contains(risk, "⚠️") {
		t.Errorf("expected warning emoji for medium risk, got: %q", risk)
	}
}

func TestBuildPipelineSummary_ShowsParseFailureForInvalidRoundFindings(t *testing.T) {
	t.Parallel()
	invalidFindings := `{"findings":[`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &invalidFindings, DurationMS: 1000}},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "failed to parse findings") {
		t.Errorf("expected parse failure message for invalid round findings, got:\n%s", md)
	}
	if strings.Contains(md, "✅ **Test** - passed") {
		t.Errorf("did not expect passed status when round findings cannot be parsed, got:\n%s", md)
	}
	if !strings.Contains(md, "⚠️ **Test** - findings unavailable") {
		t.Errorf("expected warning status when round findings cannot be parsed, got:\n%s", md)
	}
	if strings.Contains(md, "**Round 1** - \n") {
		t.Errorf("did not expect blank round summary for invalid round findings, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_DoesNotClaimFixedWhenFinalFindingsUnreadable(t *testing.T) {
	t.Parallel()
	initialFindings := `{"findings":[{"id":"lint-1","severity":"warning","description":"still broken"}],"summary":"1 issue"}`
	invalidFinalFindings := `{"findings":[`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepLint, Status: types.StepStatusCompleted, FindingsJSON: &invalidFinalFindings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &initialFindings, DurationMS: 800},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &invalidFinalFindings, DurationMS: 600},
		},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if strings.Contains(md, "🔧 **Lint**") {
		t.Errorf("did not expect fixed status when final findings are unreadable, got:\n%s", md)
	}
	if !strings.Contains(md, "⚠️ **Lint** - findings unavailable") {
		t.Errorf("expected unavailable status when final findings are unreadable, got:\n%s", md)
	}
	if !strings.Contains(md, "failed to parse findings") {
		t.Errorf("expected parse failure details for unreadable final findings, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_ReviewDoesNotReuseInitialRiskWhenFinalUnreadable(t *testing.T) {
	t.Parallel()
	initialFindings := `{"findings":[{"id":"review-1","severity":"warning","description":"risky change"}],"summary":"1 warning","risk_level":"medium","risk_rationale":"initial risk rationale"}`
	invalidFinalFindings := `{"findings":[`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &invalidFinalFindings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &initialFindings, DurationMS: 1000},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &invalidFinalFindings, DurationMS: 700},
		},
	}
	md, risk := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "⚠️ **Review** - findings unavailable") {
		t.Errorf("expected unavailable review status when final findings are unreadable, got:\n%s", md)
	}
	if risk != "" {
		t.Errorf("expected empty risk when final findings are unreadable, got: %q", risk)
	}
	if !strings.Contains(md, "failed to parse findings") {
		t.Errorf("expected parse failure details for unreadable final review findings, got:\n%s", md)
	}
}

func TestBuildPipelineSummary_FindingSeverityEmoji(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"critical bug"},{"id":"review-2","severity":"warning","description":"minor issue"},{"id":"review-3","severity":"info","description":"suggestion"}],"summary":"3 findings","risk_level":"high","risk_rationale":"critical bug found"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	md, risk := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "🚨") {
		t.Errorf("expected error emoji in details, got:\n%s", md)
	}
	if !strings.Contains(md, "ℹ️") {
		t.Errorf("expected info emoji in details, got:\n%s", md)
	}
	if !strings.Contains(risk, "High") || !strings.Contains(risk, "critical bug found") {
		t.Errorf("expected risk line with capitalized high level, got: %q", risk)
	}
	if !strings.Contains(risk, "🚨") {
		t.Errorf("expected error emoji for high risk, got: %q", risk)
	}
}

func TestBuildPipelineSummary_ReviewUsesLatestRoundNotFirst(t *testing.T) {
	t.Parallel()
	// When sr.FindingsJSON is nil (cleared), the fallback should use the
	// latest round's risk assessment, not the first round's stale one.
	initialFindings := `{"findings":[{"id":"review-1","severity":"warning","description":"issue"}],"summary":"1 warning","risk_level":"medium","risk_rationale":"initial concern"}`
	latestFindings := `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"issues resolved"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: nil},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &initialFindings, DurationMS: 1000},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &latestFindings, DurationMS: 700},
		},
	}
	_, risk := BuildPipelineSummary(steps, rounds)

	if strings.Contains(risk, "initial concern") {
		t.Errorf("expected latest round risk, not stale first round, got: %q", risk)
	}
	if !strings.Contains(risk, "issues resolved") {
		t.Errorf("expected latest round rationale, got: %q", risk)
	}
	if !strings.Contains(risk, "Low") {
		t.Errorf("expected Low risk from latest round, got: %q", risk)
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
