package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

func TestBuildPipelineSummary_LegacyUserFixRoundsRenderAsAutoFix(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"legacy round"}],"summary":"1 warning"}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {
			{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 1000},
			{Round: 2, Trigger: "user_fix", DurationMS: 700},
		},
	}
	md, _ := BuildPipelineSummary(steps, rounds)

	if !strings.Contains(md, "auto-fixed") {
		t.Errorf("expected legacy user_fix round to render as auto-fixed, got:\n%s", md)
	}
	if !strings.Contains(md, "Round 2** (auto-fix) - passed") {
		t.Errorf("expected legacy user_fix round label to render as auto-fix, got:\n%s", md)
	}
	if strings.Contains(md, "user-fix") || strings.Contains(md, "user-fixed") {
		t.Errorf("did not expect legacy user-fix wording in summary, got:\n%s", md)
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
