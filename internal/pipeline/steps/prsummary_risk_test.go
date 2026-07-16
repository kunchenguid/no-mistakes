package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildRiskLine_ReviewWithRisk(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"warning","file":"cmd/main.go","line":10,"description":"potential nil deref"}],"summary":"1 warning","risk_level":"low","risk_rationale":"straightforward refactor with no behavioral changes"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	risk := BuildRiskLine(steps, rounds)

	if !strings.Contains(risk, "Low") || !strings.Contains(risk, "straightforward refactor") {
		t.Errorf("expected risk line with capitalized level and rationale, got: %q", risk)
	}
	if !strings.Contains(risk, "✅") {
		t.Errorf("expected checkmark emoji for low risk, got: %q", risk)
	}
}

func TestBuildRiskLine_ReviewUsesFinalCleanState(t *testing.T) {
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
	risk := BuildRiskLine(steps, rounds)

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
}

func TestBuildRiskLine_ShowsWarningForUnresolvedRiskWithoutFindings(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	risk := BuildRiskLine(steps, rounds)

	if !strings.Contains(risk, "Medium") || !strings.Contains(risk, "touches critical error handling") {
		t.Errorf("expected risk line with capitalized medium level, got: %q", risk)
	}
	if !strings.Contains(risk, "⚠️") {
		t.Errorf("expected warning emoji for medium risk, got: %q", risk)
	}
}

func TestBuildRiskLine_DoesNotReuseInitialRiskWhenFinalUnreadable(t *testing.T) {
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
	risk := BuildRiskLine(steps, rounds)

	if risk != "" {
		t.Errorf("expected empty risk when final findings are unreadable, got: %q", risk)
	}
}

func TestBuildRiskLine_FindingSeverityEmoji(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"critical bug"},{"id":"review-2","severity":"warning","description":"minor issue"},{"id":"review-3","severity":"info","description":"suggestion"}],"summary":"3 findings","risk_level":"high","risk_rationale":"critical bug found"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000}},
	}
	risk := BuildRiskLine(steps, rounds)

	if !strings.Contains(risk, "High") || !strings.Contains(risk, "critical bug found") {
		t.Errorf("expected risk line with capitalized high level, got: %q", risk)
	}
	if !strings.Contains(risk, "🚨") {
		t.Errorf("expected error emoji for high risk, got: %q", risk)
	}
}

func TestBuildRiskLine_UsesLatestRoundNotFirst(t *testing.T) {
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
	risk := BuildRiskLine(steps, rounds)

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
