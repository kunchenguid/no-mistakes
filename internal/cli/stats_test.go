package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestStatsCommandRendersAllRepoDashboard(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repoA, _ := database.InsertRepo("/work/alpha", "git@example.com:alpha.git", "main")
	repoB, _ := database.InsertRepo("/work/beta", "git@example.com:beta.git", "main")

	runA, _ := database.InsertRun(repoA.ID, "feature-a", "head-a", "base-a")
	reviewA, _ := database.InsertStepResult(runA.ID, types.StepReview, 0)
	reviewAInitial := `{"findings":[{"id":"r1","severity":"warning","description":"one","action":"auto-fix"},{"id":"r2","severity":"warning","description":"two","action":"auto-fix"},{"id":"r3","severity":"warning","description":"three","action":"auto-fix"}],"summary":"three","risk_level":"medium","risk_rationale":"test"}`
	reviewAFinal := `{"findings":[{"id":"r3","severity":"warning","description":"three","action":"ask-user"}],"summary":"one left","risk_level":"medium","risk_rationale":"test"}`
	insertRound(t, database, reviewA.ID, 1, "initial", &reviewAInitial)
	insertRound(t, database, reviewA.ID, 2, "auto_fix", &reviewAFinal)

	lintA, _ := database.InsertStepResult(runA.ID, types.StepLint, 0)
	lintAInitial := `{"findings":[{"id":"l1","severity":"error","description":"lint","action":"auto-fix"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	insertRound(t, database, lintA.ID, 1, "initial", &lintAInitial)
	insertRound(t, database, lintA.ID, 2, "auto_fix", nil)

	runB, _ := database.InsertRun(repoB.ID, "feature-b", "head-b", "base-b")
	testB, _ := database.InsertStepResult(runB.ID, types.StepTest, 0)
	testBInitial := `{"findings":[{"id":"t1","severity":"error","description":"test","action":"ask-user"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	insertRound(t, database, testB.ID, 1, "initial", &testBInitial)

	out, err := executeCmd("stats")
	if err != nil {
		t.Fatalf("stats command: %v\n%s", err, out)
	}

	for _, want := range []string{
		"╭─ git push no-mistakes",
		"_  _ ____    _  _ _ ____ ___ ____ _  _ ____ ____",
		"Total changes",
		"Rescued changes",
		"Rescue rate",
		"50%",
		"Mistakes",
		"Reported",
		"Fixed",
		"Fixes by step",
		"review",
		"lint",
		"Top repos",
		"alpha",
		"3 fixes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats output missing %q:\n%s", want, out)
		}
	}
	assertOrder(t, out, "Total changes", "Rescued changes", "Rescue rate", "Mistakes", "Reported", "Fixed")
	for _, notWant := range []string{"Saved", "Rescue runs", "Mistakes fixed", "auto-fix", "caught in review", "╭─ no-mistakes"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("stats output should not contain %q:\n%s", notWant, out)
		}
	}
}

func TestStatsDashboardCapsTopReposAndUsesPipelineStepOrder(t *testing.T) {
	stats := &db.Stats{
		TotalRuns:        4,
		RescueRuns:       4,
		ReportedFindings: 12,
		FixedFindings:    12,
		StepStats: []db.StepStats{
			{StepName: types.StepLint, FixedFindings: 3},
			{StepName: types.StepReview, FixedFindings: 1},
			{StepName: types.StepDocument, FixedFindings: 4},
			{StepName: types.StepTest, FixedFindings: 2},
		},
		RepoStats: []db.RepoStats{
			{WorkingPath: "/repos/one", Runs: 1, RescueRuns: 1, FixedFindings: 4},
			{WorkingPath: "/repos/two", Runs: 1, RescueRuns: 1, FixedFindings: 3},
			{WorkingPath: "/repos/three", Runs: 1, RescueRuns: 1, FixedFindings: 2},
			{WorkingPath: "/repos/four", Runs: 1, RescueRuns: 1, FixedFindings: 1},
		},
	}
	out := renderStatsDashboard(stats)

	assertOrder(t, out, "review", "test", "document", "lint")
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats output missing top repo %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "four") {
		t.Fatalf("stats output should cap top repos at 3:\n%s", out)
	}
}

func TestStatsDashboardTopBorderShowsGitPushNoMistakes(t *testing.T) {
	out := renderStatsDashboard(&db.Stats{})
	firstLine := strings.Split(out, "\n")[0]
	if !strings.Contains(firstLine, "git push no-mistakes") {
		t.Fatalf("top border should include eyebrow, got %q", firstLine)
	}
}

func TestStatsDashboardCentersBannerAsBlock(t *testing.T) {
	out := renderStatsDashboard(&db.Stats{})
	lines := strings.Split(out, "\n")
	var bannerLines []string
	for _, line := range lines {
		if strings.Contains(line, "_  _ ____") || strings.Contains(line, `|\ | |  |`) || strings.Contains(line, `| \| |__|`) {
			bannerLines = append(bannerLines, strings.TrimSuffix(strings.TrimPrefix(line, "│ "), " │"))
		}
	}
	if len(bannerLines) != 3 {
		t.Fatalf("found %d banner lines, want 3:\n%s", len(bannerLines), out)
	}

	indent := leadingSpaces(bannerLines[0])
	for _, line := range bannerLines[1:] {
		if got := leadingSpaces(line); got != indent {
			t.Fatalf("banner line indent = %d, want %d:\n%s", got, indent, out)
		}
	}
}

func TestStatsDashboardStylesBannerAndProgressBars(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	out := renderStatsDashboard(&db.Stats{TotalRuns: 1, RescueRuns: 1, ReportedFindings: 2, FixedFindings: 1})
	if !strings.Contains(out, sCyan.Render("_  _ ____    _  _ _ ____ ___ ____ _  _ ____ ____")) {
		t.Fatalf("stats banner should be cyan:\n%s", out)
	}
	if !strings.Contains(out, "\x1b[32m") {
		t.Fatalf("stats progress bars should use green filled segments:\n%s", out)
	}
	if !strings.Contains(out, "\x1b[90m") {
		t.Fatalf("stats progress bars should use dim empty segments:\n%s", out)
	}
}

func TestStatsDashboardAddsMarginAboveBottomBorder(t *testing.T) {
	out := renderStatsDashboard(&db.Stats{})
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("stats output too short:\n%s", out)
	}
	if got := lines[len(lines)-2]; strings.TrimSpace(strings.Trim(got, "│")) != "" {
		t.Fatalf("line above bottom border should be blank, got %q:\n%s", got, out)
	}
}

func TestStatsDashboardFixedPercentMovesIntoValueColumnAndBarsReachRightEdge(t *testing.T) {
	stats := &db.Stats{
		TotalRuns:        1,
		RescueRuns:       1,
		ReportedFindings: 5,
		FixedFindings:    4,
		RepoStats: []db.RepoStats{
			{WorkingPath: "/repos/one", Runs: 1, RescueRuns: 1, FixedFindings: 4},
		},
	}
	out := renderStatsDashboard(stats)
	lines := strings.Split(out, "\n")
	fixedLine := findLineContaining(t, lines, "Fixed")
	if !strings.Contains(fixedLine, "Fixed              80%") {
		t.Fatalf("fixed row should show percent in the value column:\n%s", out)
	}
	if strings.Count(fixedLine, "80%") != 1 {
		t.Fatalf("fixed row should not repeat percent after the bar:\n%s", out)
	}
	if barEnd := lipgloss.Width(fixedLine) - 3; barEnd != statsContentWidth+1 {
		t.Fatalf("fixed bar should reach right edge, got end %d want %d:\n%s", barEnd, statsContentWidth+1, out)
	}
}

func TestStatsDashboardReportedBarIsEmptyWhenNoFindingsReported(t *testing.T) {
	out := renderStatsDashboard(&db.Stats{})
	line := findLineContaining(t, strings.Split(out, "\n"), "Reported")
	if strings.Contains(line, "█") {
		t.Fatalf("reported row should not show filled progress for zero findings:\n%s", out)
	}
}

func insertRound(t *testing.T, database *db.DB, stepID string, round int, trigger string, findings *string) {
	t.Helper()
	if _, err := database.InsertStepRound(stepID, round, trigger, findings, nil, 100); err != nil {
		t.Fatal(err)
	}
}

func findLineContaining(t *testing.T, lines []string, needle string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("missing line containing %q", needle)
	return ""
}

func leadingSpaces(value string) int {
	return len(value) - len(strings.TrimLeft(value, " "))
}

func assertOrder(t *testing.T, value string, ordered ...string) {
	t.Helper()
	last := -1
	for _, item := range ordered {
		idx := strings.Index(value, item)
		if idx == -1 {
			t.Fatalf("missing %q in:\n%s", item, value)
		}
		if idx <= last {
			t.Fatalf("%q appeared out of order in:\n%s", item, value)
		}
		last = idx
	}
}
