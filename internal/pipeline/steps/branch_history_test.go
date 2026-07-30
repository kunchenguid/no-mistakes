package steps

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type branchHistoryFixture struct {
	db     *db.DB
	repoID string
	branch string
}

func newBranchHistoryFixture(t *testing.T) *branchHistoryFixture {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	return &branchHistoryFixture{db: database, repoID: repo.ID, branch: "refs/heads/feature"}
}

// priorRun records a completed earlier run on the branch whose review round
// produced findingsJSON and then had selectedIDs chosen by source.
func (f *branchHistoryFixture) priorRun(t *testing.T, findingsJSON string, selectedIDs string, source string) {
	t.Helper()
	run, err := f.db.InsertRun(f.repoID, f.branch, "prior-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := f.db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := f.db.InsertStepRound(sr.ID, 1, "initial", &findingsJSON, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if selectedIDs != "" {
		if err := f.db.SetStepRoundSelection(round.ID, &selectedIDs, source); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *branchHistoryFixture) currentContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	run, err := f.db.InsertRun(f.repoID, f.branch, "current-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := f.db.GetRepo(f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.StepContext{DB: f.db, Run: run, Repo: repo}
}

func TestBranchHistoryPromptSection_EmptyWithoutPriorRun(t *testing.T) {
	f := newBranchHistoryFixture(t)
	if got := branchHistoryPromptSection(f.currentContext(t)); got != "" {
		t.Errorf("expected no section without prior run, got: %q", got)
	}
}

// A finding the user saw and declined is the class that must never come back
// unprompted: re-raising it is pure noise.
func TestBranchHistoryPromptSection_LabelsUserDeclinedFindings(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t,
		`{"findings":[
			{"id":"kept-on-purpose","severity":"warning","file":"a.go","description":"hardcoded timeout"},
			{"id":"worth-fixing","severity":"error","file":"b.go","description":"nil deref"}
		]}`,
		`["worth-fixing"]`, db.RoundSelectionSourceUser)

	got := branchHistoryPromptSection(f.currentContext(t))
	if !strings.Contains(got, "\ndeclined_by_user:") {
		t.Fatalf("missing declined_by_user grouping in:\n%s", got)
	}
	declined := sectionAfter(got, "declined_by_user")
	if !strings.Contains(declined, "kept-on-purpose") {
		t.Errorf("unselected finding not reported as declined:\n%s", got)
	}
	if strings.Contains(declined, "worth-fixing") {
		t.Errorf("selected finding wrongly reported as declined:\n%s", got)
	}
}

func TestBranchHistoryPromptSection_LabelsAddressedFindings(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t,
		`{"findings":[{"id":"was-fixed","severity":"error","file":"b.go","description":"nil deref"}]}`,
		`["was-fixed"]`, db.RoundSelectionSourceAutoFix)

	got := branchHistoryPromptSection(f.currentContext(t))
	addressed := sectionAfter(got, "addressed")
	if !strings.Contains(addressed, "was-fixed") {
		t.Fatalf("fixed finding not reported as addressed:\n%s", got)
	}
}

// A finding nobody selected and nobody declined at a user gate is simply still
// open. It stays reportable, so it must not be labelled as declined.
func TestBranchHistoryPromptSection_LabelsUnselectedAutoFixRoundAsStillOpen(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t,
		`{"findings":[
			{"id":"below-floor","severity":"info","file":"a.go","description":"nit"},
			{"id":"picked","severity":"warning","file":"b.go","description":"real"}
		]}`,
		`["picked"]`, db.RoundSelectionSourceAutoFix)

	got := branchHistoryPromptSection(f.currentContext(t))
	if strings.Contains(got, "\ndeclined_by_user:") {
		t.Fatalf("auto-fix filtering must not be recorded as a user decision:\n%s", got)
	}
	open := sectionAfter(got, "reported_not_addressed")
	if !strings.Contains(open, "below-floor") {
		t.Errorf("unselected auto-fix finding not reported as still open:\n%s", got)
	}
}

// The section is the input-side saving: it must tell the reviewer what to do
// with each disposition, not merely list them.
func TestBranchHistoryPromptSection_CarriesDispositionInstructions(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t,
		`{"findings":[{"id":"x","severity":"warning","file":"a.go","description":"d"}]}`,
		`["x"]`, db.RoundSelectionSourceUser)

	got := branchHistoryPromptSection(f.currentContext(t))
	for _, phrase := range []string{
		"do not re-derive",
		"unless the code it refers to has changed",
		"regression",
		"metadata only",
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("section missing instruction %q in:\n%s", phrase, got)
		}
	}
}

func TestBranchHistoryPromptSection_IgnoresOtherBranches(t *testing.T) {
	f := newBranchHistoryFixture(t)
	other := &branchHistoryFixture{db: f.db, repoID: f.repoID, branch: "refs/heads/unrelated"}
	other.priorRun(t, `{"findings":[{"id":"elsewhere","severity":"error","description":"d"}]}`, "", "")

	if got := branchHistoryPromptSection(f.currentContext(t)); got != "" {
		t.Errorf("history leaked from another branch:\n%s", got)
	}
}

func TestBranchHistoryPromptSection_BoundsFindingCount(t *testing.T) {
	f := newBranchHistoryFixture(t)
	var items []string
	for i := 0; i < branchHistoryMaxFindings+10; i++ {
		items = append(items, `{"id":"f`+strconv.Itoa(i)+`","severity":"info","file":"a.go","description":"d"}`)
	}
	f.priorRun(t, `{"findings":[`+strings.Join(items, ",")+`]}`, "", "")

	got := branchHistoryPromptSection(f.currentContext(t))
	if lines := strings.Count(got, `{"id":"f`); lines > branchHistoryMaxFindings {
		t.Errorf("rendered %d findings, want at most %d", lines, branchHistoryMaxFindings)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation not disclosed to the reviewer:\n%s", got)
	}
}

// sectionAfter returns the body of a disposition grouping. The leading newline
// distinguishes the grouping header from the same word in the legend, which is
// rendered as a bulleted line.
func sectionAfter(text, disposition string) string {
	header := "\n" + disposition + ":"
	idx := strings.Index(text, header)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(header):]
	for _, next := range []string{"\ndeclined_by_user:", "\naddressed:", "\nreported_not_addressed:"} {
		if end := strings.Index(rest, next); end >= 0 {
			rest = rest[:end]
		}
	}
	return rest
}
