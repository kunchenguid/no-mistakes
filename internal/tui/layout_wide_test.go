package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestModel_View_WideLayoutPlacesPipelineBesideFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 140
	m.height = 40

	view := m.View()
	if !strings.Contains(stripANSI(view), "Findings -") {
		t.Fatalf("expected findings box in view, got:\n%s", stripANSI(view))
	}
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected wide layout to render parallel boxes, got:\n%s", stripANSI(view))
	}
}

func TestModel_View_WideLayoutPlacesPipelineBesideLog(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.width = 140
	m.height = 40
	m.logs = []string{"running go test ./..."}

	view := m.View()
	if !strings.Contains(stripANSI(view), "Log") {
		t.Fatalf("expected log box in view, got:\n%s", stripANSI(view))
	}
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected wide layout to render pipeline beside log, got:\n%s", stripANSI(view))
	}
}

func TestModel_View_NarrowLayoutKeepsPipelineStackedAboveFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40

	view := m.View()
	if hasParallelBoxRow(view) {
		t.Fatalf("expected narrow layout to keep pipeline stacked above findings, got:\n%s", stripANSI(view))
	}
}

func TestModel_View_Width100UsesResponsiveLayout(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 100
	m.height = 40

	view := m.View()
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected width 100 to use responsive layout, got:\n%s", stripANSI(view))
	}
}

func TestHelpOverlay_NavigationDescriptionsAligned(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	result := renderHelpOverlay(80, testRun(), true, true, true, false)
	plain := stripANSI(result)
	lines := strings.Split(plain, "\n")

	// Find lines containing navigation descriptions and measure their visual column positions.
	navDescriptions := []string{"scroll line by line", "jump to start/end", "half-page down/up"}
	var descColumns []int
	for _, line := range lines {
		for _, desc := range navDescriptions {
			if col := visualColumn(line, desc); col >= 0 {
				descColumns = append(descColumns, col)
			}
		}
	}

	if len(descColumns) < 3 {
		t.Fatalf("expected at least 3 navigation entries, found %d in:\n%s", len(descColumns), plain)
	}

	// All descriptions should start at the same visual column.
	for i := 1; i < len(descColumns); i++ {
		if descColumns[i] != descColumns[0] {
			t.Errorf("navigation descriptions not aligned: column %d vs %d in:\n%s",
				descColumns[0], descColumns[i], plain)
		}
	}
}

func TestHelpOverlay_ActionDescriptionsAligned(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	result := renderHelpOverlay(80, testRun(), true, false, true, false)
	plain := stripANSI(result)
	lines := strings.Split(plain, "\n")

	// Find lines containing action descriptions and measure their visual column positions.
	actionDescriptions := []string{"approve", "fix", "skip", "abort (press twice)"}
	var descColumns []int
	for _, line := range lines {
		for _, desc := range actionDescriptions {
			if col := visualColumn(line, desc); col >= 0 {
				descColumns = append(descColumns, col)
			}
		}
	}

	if len(descColumns) < 4 {
		t.Fatalf("expected 4 action entries, found %d in:\n%s", len(descColumns), plain)
	}

	// All descriptions should start at the same visual column.
	for i := 1; i < len(descColumns); i++ {
		if descColumns[i] != descColumns[0] {
			t.Errorf("action descriptions not aligned: column %d vs %d in:\n%s",
				descColumns[0], descColumns[i], plain)
		}
	}
}

func TestHelpOverlay_ShowsRunContext(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	result := stripANSI(renderHelpOverlay(80, run, true, false, true, false))
	if !strings.Contains(result, run.Branch) {
		t.Fatalf("expected help overlay to show branch name, got:\n%s", result)
	}
	if !strings.Contains(result, run.HeadSHA[:8]) {
		t.Fatalf("expected help overlay to show short commit SHA, got:\n%s", result)
	}
	if !strings.Contains(result, run.ID) {
		t.Fatalf("expected help overlay to show pipeline ID, got:\n%s", result)
	}
}

func TestHelpOverlay_ShowsOpenPRActionWhenPRURLPresent(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	prURL := "https://github.com/test/repo/pull/42"
	run.PRURL = &prURL

	result := stripANSI(renderHelpOverlay(80, run, true, false, true, false))
	if !strings.Contains(result, "open PR in browser") {
		t.Fatalf("expected help overlay to include PR browser action, got:\n%s", result)
	}
	if !strings.Contains(result, "o") {
		t.Fatalf("expected help overlay to include 'o' keybinding, got:\n%s", result)
	}
}

// Test: action bar to findings box should have exactly 1 blank line, not 2.
func TestModel_View_OneBlankLineBetweenActionBarAndFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	// Find the last line of the action bar (contains "approve") and the Findings box top border.
	actionBarEnd := -1
	findingsStart := -1
	for i, line := range lines {
		if strings.Contains(line, "approve") && strings.Contains(line, "skip") {
			actionBarEnd = i
		}
		if strings.Contains(line, "╭") && strings.Contains(line, "Findings") {
			findingsStart = i
			break
		}
	}

	if actionBarEnd < 0 || findingsStart < 0 {
		t.Fatalf("could not find action bar or findings box in view:\n%s", plain)
	}

	blankCount := findingsStart - actionBarEnd - 1
	if blankCount != 1 {
		t.Errorf("expected 1 blank line between action bar and findings box, got %d\naction bar line %d: %q\nfindings line %d: %q",
			blankCount, actionBarEnd, lines[actionBarEnd], findingsStart, lines[findingsStart])
	}
}

// Test: log box to help overlay should have exactly 1 blank line, not 2.
func TestModel_View_OneBlankLineBetweenLogAndHelp(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.logs = []string{"running tests..."}
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	// Find the log box bottom border and help box top border.
	logBottom := -1
	helpTop := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "╯") && logBottom < 0 {
			// Check if this is the log box bottom (after a log box, not the pipeline box).
			if i > 0 {
				// Look backwards for "Log" title to confirm this is the log box.
				for j := i - 1; j >= 0 && j > i-10; j-- {
					if strings.Contains(lines[j], "Log") && strings.Contains(lines[j], "╭") {
						logBottom = i
						break
					}
				}
			}
		}
		if strings.Contains(line, "╭") && strings.Contains(line, "Help") {
			helpTop = i
			break
		}
	}

	if logBottom < 0 || helpTop < 0 {
		t.Fatalf("could not find log box bottom or help box top in view:\n%s", plain)
	}

	blankCount := helpTop - logBottom - 1
	if blankCount != 1 {
		t.Errorf("expected 1 blank line between log box and help overlay, got %d\nlog bottom line %d: %q\nhelp top line %d: %q",
			blankCount, logBottom, lines[logBottom], helpTop, lines[helpTop])
	}
}

func TestModel_View_ResponsiveLayoutKeepsHelpVisibleWithLogs(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 120
	m.height = 40
	m.logs = make([]string, 40)
	for i := range m.logs {
		m.logs[i] = fmt.Sprintf("log line %02d", i)
	}
	m.showHelp = true

	view := stripANSI(m.View())

	if !strings.Contains(view, "Help") {
		t.Fatalf("expected help overlay to remain visible in responsive layout with logs, got:\n%s", view)
	}
	if !strings.Contains(view, "close help") {
		t.Fatalf("expected help overlay content in responsive layout with logs, got:\n%s", view)
	}
}

func TestModel_View_ResponsiveLayoutReservesGapBeforeLogBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 120
	m.height = 20
	m.logs = make([]string, 40)
	for i := range m.logs {
		m.logs[i] = fmt.Sprintf("log line %02d", i)
	}

	view := stripANSI(m.View())
	if !strings.Contains(view, "q detach") {
		t.Fatalf("expected footer to remain visible, got:\n%s", view)
	}
	if !strings.Contains(view, "approve") {
		t.Fatalf("expected action bar to remain visible, got:\n%s", view)
	}
	if !strings.Contains(view, "Log") {
		t.Fatalf("expected log box to remain visible, got:\n%s", view)
	}
	if strings.Contains(view, "log line 26") {
		t.Fatalf("expected responsive log box to reserve one line for the action-bar separator, got:\n%s", view)
	}
	if !strings.Contains(view, "log line 27") {
		t.Fatalf("expected responsive log box to keep the newest lines after reserving the separator, got:\n%s", view)
	}
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected responsive layout with side-by-side columns, got:\n%s", view)
	}
}

// Test: footer should have consistent spacing (1 blank line) after any preceding section.
func TestModel_View_ConsistentFooterSpacing(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	// Test with pipeline box only (completed, no log, no findings).
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunCompleted,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusCompleted},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.done = true

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	// Find footer line (contains "q" and "quit").
	footerLine := -1
	for i, line := range lines {
		if strings.Contains(line, "q") && strings.Contains(line, "quit") && strings.Contains(line, "?") {
			footerLine = i
			break
		}
	}
	if footerLine < 0 {
		t.Fatalf("could not find footer in view:\n%s", plain)
	}

	// Find the last non-blank line before footer.
	lastContent := -1
	for i := footerLine - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastContent = i
			break
		}
	}
	if lastContent < 0 {
		t.Fatalf("no content before footer in view:\n%s", plain)
	}

	blankCount := footerLine - lastContent - 1
	if blankCount != 1 {
		t.Errorf("expected 1 blank line before footer, got %d\nlast content line %d: %q\nfooter line %d: %q",
			blankCount, lastContent, lines[lastContent], footerLine, lines[footerLine])
	}
}

func TestHelpOverlay_SelectionDescriptionsAligned(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// showDiff=false so selection section is visible.
	result := renderHelpOverlay(80, testRun(), true, false, true, false)
	plain := stripANSI(result)
	lines := strings.Split(plain, "\n")

	// Find lines containing selection descriptions using visual column positions.
	selDescriptions := []string{"toggle current", "select all", "select none"}
	var descColumns []int
	for _, line := range lines {
		for _, desc := range selDescriptions {
			if col := visualColumn(line, desc); col >= 0 {
				descColumns = append(descColumns, col)
			}
		}
	}

	if len(descColumns) < 3 {
		t.Fatalf("expected 3 selection entries, found %d in:\n%s", len(descColumns), plain)
	}

	// All descriptions should start at the same visual column.
	for i := 1; i < len(descColumns); i++ {
		if descColumns[i] != descColumns[0] {
			t.Errorf("selection descriptions not aligned: column %d vs %d in:\n%s",
				descColumns[0], descColumns[i], plain)
		}
	}
}
