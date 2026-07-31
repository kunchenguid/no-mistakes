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

// priorRun records an earlier run on the branch that reached status, whose
// review round produced findingsJSON and then had selectedIDs chosen by source.
func (f *branchHistoryFixture) priorRun(t *testing.T, status types.RunStatus, findingsJSON string, selectedIDs string, source string) {
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
	if err := f.db.UpdateRunStatus(run.ID, status); err != nil {
		t.Fatal(err)
	}
}

// priorRound is one recorded review round of an earlier run on the branch.
type priorRound struct {
	findingsJSON string
	selectedIDs  string
	source       string
}

// priorRunWithRounds records an earlier run whose review step went through
// several rounds, in order, so replay semantics can be exercised.
func (f *branchHistoryFixture) priorRunWithRounds(t *testing.T, status types.RunStatus, rounds ...priorRound) {
	t.Helper()
	run, err := f.db.InsertRun(f.repoID, f.branch, "prior-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := f.db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rounds {
		findings := r.findingsJSON
		round, err := f.db.InsertStepRound(sr.ID, i+1, "initial", &findings, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if r.selectedIDs != "" {
			if err := f.db.SetStepRoundSelection(round.ID, &r.selectedIDs, r.source); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.db.UpdateRunStatus(run.ID, status); err != nil {
		t.Fatal(err)
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
// unprompted: re-raising it is pure noise. Only a run that ran to completion
// proves the user actually finished deciding.
func TestBranchHistoryPromptSection_LabelsUserDeclinedFindings(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t, types.RunCompleted,
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

// With auto_fix.review disabled every gate resolves through a user selection,
// so selection_source alone cannot distinguish "the user accepted this" from
// "the run died before the user got to it". A run that failed or was cancelled
// proves nothing was accepted, and telling the reviewer otherwise silences real
// findings.
func TestBranchHistoryPromptSection_UnfinishedRunNeverDeclinesFindings(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunFailed, types.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			f := newBranchHistoryFixture(t)
			f.priorRun(t, status,
				`{"findings":[
					{"id":"never-decided","severity":"warning","file":"a.go","description":"hardcoded timeout"},
					{"id":"was-picked","severity":"error","file":"b.go","description":"nil deref"}
				]}`,
				`["was-picked"]`, db.RoundSelectionSourceUser)

			got := branchHistoryPromptSection(f.currentContext(t))
			if strings.Contains(got, "\ndeclined_by_user:") {
				t.Fatalf("unfinished run must not report declined findings:\n%s", got)
			}
			open := sectionAfter(got, "reported_not_addressed")
			if !strings.Contains(open, "never-decided") {
				t.Errorf("undecided finding not reported as still open:\n%s", got)
			}
		})
	}
}

func TestBranchHistoryPromptSection_LabelsAddressedFindings(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t, types.RunCompleted,
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
	f.priorRun(t, types.RunCompleted,
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

// The section exists to spend fewer tokens than re-deriving the findings would,
// so it carries recognition metadata only: the reviewer needs to match a past
// finding to current code, not re-read the argument for it.
func TestBranchHistoryPromptSection_RendersFindingsCompactly(t *testing.T) {
	f := newBranchHistoryFixture(t)
	longRationale := "The claim handler drops the error. " + strings.Repeat("Further rationale that the reviewer does not need again. ", 20)
	f.priorRun(t, types.RunCompleted,
		`{"findings":[{"id":"verbose","severity":"warning","file":"a.go","line":42,"description":"`+longRationale+`","action":"auto-fix","source":"review","user_instructions":"do the thing"}]}`,
		"", "")

	got := branchHistoryPromptSection(f.currentContext(t))
	if strings.Contains(got, `{"id":`) {
		t.Errorf("findings rendered as raw JSON rather than compactly:\n%s", got)
	}
	if !strings.Contains(got, "verbose | warning | a.go:42 | The claim handler drops the error.") {
		t.Errorf("compact rendering missing or malformed:\n%s", got)
	}
	if strings.Contains(got, "Further rationale") {
		t.Errorf("full rationale carried into history:\n%s", got)
	}
	if strings.Contains(got, "user_instructions") || strings.Contains(got, "do the thing") {
		t.Errorf("fixer-only fields carried into history:\n%s", got)
	}
}

// A description with no sentence break must still be bounded, or a single
// run-on finding reintroduces the cost the compact rendering removes.
func TestBranchHistoryPromptSection_BoundsSentencelessDescription(t *testing.T) {
	f := newBranchHistoryFixture(t)
	runOn := strings.Repeat("word ", 200)
	f.priorRun(t, types.RunCompleted,
		`{"findings":[{"id":"runon","severity":"info","file":"a.go","description":"`+runOn+`"}]}`,
		"", "")

	got := branchHistoryPromptSection(f.currentContext(t))
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "runon |") && len(line) > branchHistoryMaxDescriptionChars+120 {
			t.Errorf("unbounded finding line (%d chars):\n%s", len(line), line)
		}
	}
}

// The section is the input-side saving: it must tell the reviewer what to do
// with each disposition, not merely list them.
func TestBranchHistoryPromptSection_CarriesDispositionInstructions(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t, types.RunCompleted,
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

// The reviewer fans the review out to per-aspect sub-agents that do not inherit
// this prompt. Without an explicit instruction to pass the section on, every
// sub-agent re-derives the findings at full cost and the saving is lost.
func TestBranchHistoryPromptSection_InstructsPropagationToSubAgents(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRun(t, types.RunCompleted,
		`{"findings":[{"id":"x","severity":"warning","file":"a.go","description":"d"}]}`,
		"", "")

	got := branchHistoryPromptSection(f.currentContext(t))
	if !strings.Contains(got, "sub-agent") {
		t.Errorf("section does not tell the reviewer to propagate it to sub-agents:\n%s", got)
	}
}

func TestBranchHistoryPromptSection_IgnoresOtherBranches(t *testing.T) {
	f := newBranchHistoryFixture(t)
	other := &branchHistoryFixture{db: f.db, repoID: f.repoID, branch: "refs/heads/unrelated"}
	other.priorRun(t, types.RunCompleted, `{"findings":[{"id":"elsewhere","severity":"error","description":"d"}]}`, "", "")

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
	f.priorRun(t, types.RunCompleted, `{"findings":[`+strings.Join(items, ",")+`]}`, "", "")

	got := branchHistoryPromptSection(f.currentContext(t))
	if lines := strings.Count(got, " | info | "); lines > branchHistoryMaxFindings {
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

// Rounds replay in order and the last thing that happened to a finding wins:
// a finding declined in round one and selected in round two is addressed, not
// declined, and a finding declined in every round it appeared stays declined.
func TestBranchHistoryPromptSection_LastRoundWinsAcrossRounds(t *testing.T) {
	f := newBranchHistoryFixture(t)
	f.priorRunWithRounds(t, types.RunCompleted,
		priorRound{
			findingsJSON: `{"findings":[
				{"id":"reconsidered","severity":"warning","file":"a.go","description":"hardcoded timeout"},
				{"id":"kept-on-purpose","severity":"info","file":"b.go","description":"nit"},
				{"id":"fixed-first","severity":"error","file":"c.go","description":"nil deref"}
			]}`,
			selectedIDs: `["fixed-first"]`,
			source:      db.RoundSelectionSourceUser,
		},
		priorRound{
			findingsJSON: `{"findings":[
				{"id":"reconsidered","severity":"warning","file":"a.go","description":"hardcoded timeout"},
				{"id":"kept-on-purpose","severity":"info","file":"b.go","description":"nit"}
			]}`,
			selectedIDs: `["reconsidered"]`,
			source:      db.RoundSelectionSourceUser,
		},
	)

	got := branchHistoryPromptSection(f.currentContext(t))
	addressed := sectionAfter(got, "addressed")
	if !strings.Contains(addressed, "reconsidered") {
		t.Errorf("finding selected in the later round must be addressed:\n%s", got)
	}
	if !strings.Contains(addressed, "fixed-first") {
		t.Errorf("finding selected in the earlier round and never re-raised must stay addressed:\n%s", got)
	}
	declined := sectionAfter(got, "declined_by_user")
	if !strings.Contains(declined, "kept-on-purpose") {
		t.Errorf("finding left unselected in every round must be declined:\n%s", got)
	}
	if strings.Contains(declined, "reconsidered") {
		t.Errorf("round one's decline must not survive round two's selection:\n%s", got)
	}
}

// A selection is only evidence that a fix landed if the run reached
// completion. A run that died mid-loop selected findings it never verified, so
// every one of its rounds degrades to still-open rather than addressed.
func TestBranchHistoryPromptSection_UnfinishedRunNeverAddressesFindings(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunFailed, types.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			f := newBranchHistoryFixture(t)
			f.priorRunWithRounds(t, status,
				priorRound{
					findingsJSON: `{"findings":[{"id":"selected-then-crashed","severity":"error","file":"a.go","description":"nil deref"}]}`,
					selectedIDs:  `["selected-then-crashed"]`,
					source:       db.RoundSelectionSourceUser,
				},
				priorRound{
					findingsJSON: `{"findings":[{"id":"still-there","severity":"warning","file":"b.go","description":"race"}]}`,
					selectedIDs:  `["still-there"]`,
					source:       db.RoundSelectionSourceAutoFix,
				},
			)

			got := branchHistoryPromptSection(f.currentContext(t))
			if strings.Contains(got, "\naddressed:") {
				t.Fatalf("unfinished run must not report addressed findings:\n%s", got)
			}
			open := sectionAfter(got, "reported_not_addressed")
			for _, id := range []string{"selected-then-crashed", "still-there"} {
				if !strings.Contains(open, id) {
					t.Errorf("%s not reported as still open:\n%s", id, got)
				}
			}
		})
	}
}
