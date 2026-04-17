package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
