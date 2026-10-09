package db

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewAnswersAreKeyedByBranchAndSurviveANewRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.InsertRun(repo.ID, "feature", "head-1", "base")
	if err != nil {
		t.Fatal(err)
	}

	answers, truncated, err := d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 0 || truncated {
		t.Fatalf("empty branch = %#v truncated=%v", answers, truncated)
	}

	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: first.ID, AskOrdinal: 1,
		Question: "keep /v1?", Options: []string{"keep", "drop"},
		File: "internal/api/router.go", Line: 88,
		Answer: "keep behind a flag", AnsweredBy: "captain", AnsweredAt: "2026-09-15T13:31:40Z",
	}); err != nil {
		t.Fatal(err)
	}

	// A later run on the same branch must read it: the whole point is that the
	// next COLD reviewer does not re-ask a settled question.
	second, err := d.InsertRun(repo.ID, "feature", "head-2", "base")
	if err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 {
		t.Fatalf("branch answers = %#v, want 1", answers)
	}
	got := answers[0]
	if got.Answer != "keep behind a flag" || got.AnsweredBy != "captain" || got.RunID != first.ID {
		t.Fatalf("answer = %#v", got)
	}
	if len(got.Options) != 2 || got.Options[0] != "keep" || got.File != "internal/api/router.go" || got.Line != 88 {
		t.Fatalf("round-trip lost fields: %#v", got)
	}

	// A correction WITHIN THE SAME RUN replaces rather than accumulating,
	// matching the file protocol where the last answers.ndjson line for an id
	// wins.
	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: first.ID, AskOrdinal: 1,
		Question: "keep /v1?", Answer: "keep it unconditionally", AnsweredBy: "captain",
	}); err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Answer != "keep it unconditionally" {
		t.Fatalf("same-run correction did not replace: %#v", answers)
	}

	// A DIFFERENT run reusing the same question id keeps its own row. Ids are
	// chosen by the agent and unique only by accident, so overwriting here lost
	// the first run's settled decision and left the surviving row pairing the
	// new question text with the old answer - a record of an exchange that
	// never happened.
	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: second.ID, AskOrdinal: 1,
		Question: "should /v2 answer too?", Answer: "no", AnsweredBy: "firstmate",
	}); err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 {
		t.Fatalf("a second run's reused question id overwrote the first: %#v", answers)
	}
	byRun := map[string]ReviewAnswer{}
	for _, a := range answers {
		byRun[a.RunID] = a
	}
	if got := byRun[first.ID]; got.Answer != "keep it unconditionally" || got.Question != "keep /v1?" {
		t.Fatalf("the first run's settled answer was lost or re-paired: %#v", got)
	}
	if got := byRun[second.ID]; got.Answer != "no" || got.Question != "should /v2 answer too?" {
		t.Fatalf("the second run's answer is wrong: %#v", got)
	}

	// Another branch's conversation is not visible.
	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "other", QuestionID: "q1", RunID: second.ID, AskOrdinal: 1,
		Question: "unrelated", Answer: "yes",
	}); err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 {
		t.Fatalf("branch scoping broken: %#v", answers)
	}
}

func TestGetBranchReviewAnswersBoundsAndReportsTruncation(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2", "q3"} {
		if err := d.RecordReviewAnswer(ReviewAnswer{
			RepoID: repo.ID, Branch: "feature", QuestionID: id, RunID: run.ID, AskOrdinal: 1,
			Question: "q " + id, Answer: "a " + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	answers, truncated, err := d.GetBranchReviewAnswers(repo.ID, "feature", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || !truncated {
		t.Fatalf("answers = %d truncated = %v, want 2 and true", len(answers), truncated)
	}
}

func TestRecordReviewAnswerRequiresItsKey(t *testing.T) {
	d := openTestDB(t)
	if err := d.RecordReviewAnswer(ReviewAnswer{Branch: "feature", QuestionID: "q1", AskOrdinal: 1, Answer: "a"}); err == nil {
		t.Fatal("want error with no repo id")
	}
	if err := d.RecordReviewAnswer(ReviewAnswer{RepoID: "r", QuestionID: "q1", AskOrdinal: 1, Answer: "a"}); err == nil {
		t.Fatal("want error with no branch")
	}
	if err := d.RecordReviewAnswer(ReviewAnswer{RepoID: "r", Branch: "feature", AskOrdinal: 1, Answer: "a"}); err == nil {
		t.Fatal("want error with no question id")
	}
	// The ask ordinal is part of the key too, and a caller with none to give is
	// refused rather than coerced: coercing it to 1 would let the upsert
	// overwrite the FIRST ask's recorded human decision, which is the failure
	// the ordinal joined the key to prevent. A real repo and run, because the
	// checks above return before any SQL runs and a fake repo id would fail
	// the insert on its own.
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	settled := ReviewAnswer{
		RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: run.ID, AskOrdinal: 1,
		Question: "Is the legacy route deliberate?", Answer: "yes", AnsweredBy: "captain",
	}
	if err := d.RecordReviewAnswer(settled); err != nil {
		t.Fatal(err)
	}
	unordinalled := settled
	unordinalled.AskOrdinal = 0
	unordinalled.Question = "Should /v3 answer too?"
	unordinalled.Answer = "no"
	if err := d.RecordReviewAnswer(unordinalled); err == nil {
		t.Fatal("want error with no ask ordinal")
	}
	answers, _, err := d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Question != settled.Question || answers[0].Answer != "yes" {
		t.Fatalf("the refused answer overwrote ask 1's recorded decision: %#v", answers)
	}
}

// TestGetPreviousRunReviewRoundsDoesNotDeadlockTheSingleConnection is a real
// regression: the first version of this query held an open *sql.Rows while
// calling GetRoundsByStep, and this pool is SetMaxOpenConns(1), so the nested
// query waited forever for the connection its own caller held. It presented as
// a pipeline run wedged at the review step with no error anywhere - one
// `internal/daemon` test hit its 10-minute package timeout with the goroutine
// parked in database/sql's connection wait.
//
// The bound makes the assertion real rather than decorative: on the old code
// this test never returns, so it fails by timeout instead of passing quietly.
func TestGetPreviousRunReviewRoundsDoesNotDeadlockTheSingleConnection(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := d.InsertRun(repo.ID, "feature", "old-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	current, err := d.InsertRun(repo.ID, "feature", "new-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := d.InsertStepResult(previous.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"f-1","severity":"error","description":"drops the straggler","action":"ask-user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if _, err := d.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 0); err != nil {
		t.Fatal(err)
	}

	type result struct {
		got *PreviousReviewRounds
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := d.GetPreviousRunReviewRounds(repo.ID, "feature", current.ID)
		done <- result{got, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("GetPreviousRunReviewRounds: %v", r.err)
		}
		if r.got == nil || r.got.RunID != previous.ID || len(r.got.Rounds) != 1 {
			t.Fatalf("result = %#v, want the previous run's single round", r.got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("GetPreviousRunReviewRounds deadlocked: a nested query cannot run while its caller holds the only connection")
	}
}

// No previous run, and a previous run whose review recorded no round, both read
// as "nothing to carry forward" rather than an error, so an ordinary first run
// on a branch is unaffected.
func TestGetPreviousRunReviewRoundsReportsNothingToCarryForward(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	current, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}

	got, err := d.GetPreviousRunReviewRounds(repo.ID, "feature", current.ID)
	if err != nil || got != nil {
		t.Fatalf("no previous run = (%#v, %v), want (nil, nil)", got, err)
	}

	previous, err := d.InsertRun(repo.ID, "feature", "old-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepResult(previous.ID, types.StepReview); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetPreviousRunReviewRounds(repo.ID, "feature", current.ID)
	if err != nil || got != nil {
		t.Fatalf("previous run with no rounds = (%#v, %v), want (nil, nil)", got, err)
	}
}

// TestReviewAnswersAreKeyedPerAskWithinOneRun is the store half of the
// re-used-id defect: within ONE run a re-asked id used to overwrite the earlier
// row, so the first human decision vanished from the do-not-re-raise set and
// from the PR body with nothing erroring - and the design doc promises nothing
// deletes these rows.
func TestReviewAnswersAreKeyedPerAskWithinOneRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head-1", "base")
	if err != nil {
		t.Fatal(err)
	}
	base := ReviewAnswer{RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: run.ID}

	first := base
	first.AskOrdinal = 1
	first.Question = "Is the legacy route deliberate?"
	first.Answer = "yes"
	first.AnsweredBy = "captain"
	if err := d.RecordReviewAnswer(first); err != nil {
		t.Fatal(err)
	}
	// The fix round's cold rereview re-used the id for a different question.
	second := base
	second.AskOrdinal = 2
	second.Question = "Should the new /v3 route answer too?"
	second.Answer = "no"
	second.AnsweredBy = "captain"
	if err := d.RecordReviewAnswer(second); err != nil {
		t.Fatal(err)
	}

	got, _, err := d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want both decisions kept: %+v", len(got), got)
	}
	byOrdinal := map[int]ReviewAnswer{}
	for _, a := range got {
		byOrdinal[a.AskOrdinal] = a
	}
	if a := byOrdinal[1]; a.Question != first.Question || a.Answer != "yes" {
		t.Fatalf("ask 1 was lost or mispaired: %+v", a)
	}
	if a := byOrdinal[2]; a.Question != second.Question || a.Answer != "no" {
		t.Fatalf("ask 2 was lost or mispaired: %+v", a)
	}

	// A correction to the SAME ask still replaces, which is the stated contract.
	correction := second
	correction.Answer = "yes, behind a flag"
	if err := d.RecordReviewAnswer(correction); err != nil {
		t.Fatal(err)
	}
	got, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("a correction to one ask accumulated: %+v", got)
	}
	for _, a := range got {
		if a.AskOrdinal == 2 && a.Answer != "yes, behind a flag" {
			t.Fatalf("the correction did not replace ask 2: %+v", a)
		}
		if a.AskOrdinal == 1 && a.Answer != "yes" {
			t.Fatalf("correcting ask 2 disturbed ask 1: %+v", a)
		}
	}
}
