package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// answerFixture registers a real Executor for one run id, which is what the
// answer handler needs: the executor is the single owner of where the run's
// review conversation lives, because that path depends on effective config.
func answerFixture(t *testing.T) (*RunManager, *paths.Paths, string) {
	t.Helper()
	return answerFixtureWithConversation(t, true)
}

func answerFixtureWithConversation(t *testing.T, conversation bool) (*RunManager, *paths.Paths, string) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	m := NewRunManager(database, p, nil)
	const runID = "run-answer-1"
	cfg := &config.Config{Review: config.Review{Conversation: conversation}}
	exec := pipeline.NewExecutor(database, p, cfg, nil, nil, nil)
	m.mu.Lock()
	m.executors[runID] = exec
	m.mu.Unlock()
	return m, p, runID
}

// appendAgentQuestionLine appends one verbatim questions.ndjson line, standing
// in for the reviewer's own file tools - the only writer of that file in
// production, since internal/reviewqa deliberately owns no question writer. The
// raw JSON keeps the shape under test visible, including the fields the
// reviewer's prompt leaves out of its worked example.
func appendAgentQuestionLine(t *testing.T, dir, line string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, reviewqa.QuestionsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func conversationDir(p *paths.Paths, runID string) string {
	return reviewqa.Dir(p.RunEvidenceDir("", runID))
}

// TestAnswerReviewQuestionRecordsBeforeItDecidesToRelease is the property that
// makes the answer channel safe: the answer is on disk before anything is
// attempted with the gate, so a reviewer that is still mid-pass reads it at its
// next checkpoint and an unreleasable gate never costs the operator the answer.
func TestAnswerReviewQuestionRecordsBeforeItDecidesToRelease(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	for _, id := range []string{"q1", "q2"} {
		// No asked_at: the reviewer's prompt never mentions one, so its own
		// lines do not carry it.
		appendAgentQuestionLine(t, dir, fmt.Sprintf(`{"id":%q,"kind":"question","question":"question %s","options":["a","b"],"weight":"major"}`, id, id))
	}

	// First answer: one question still open, so the gate is deliberately not
	// touched and the caller is told how many remain.
	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "a", "captain")
	if err != nil {
		t.Fatalf("answer q1: %v", err)
	}
	if !result.OK || result.Open != 1 || result.Resumed {
		t.Fatalf("q1 result = %+v, want ok with 1 open and not resumed", result)
	}
	if len(result.OpenIDs) != 1 || result.OpenIDs[0] != "q2" {
		t.Fatalf("open ids = %v, want [q2]", result.OpenIDs)
	}

	// Second answer: nothing is open, so a release is attempted. This fixture
	// has no parked gate, so the release fails - and that must still be a
	// recorded answer and a successful call, because it is the ordinary
	// mid-turn case (the reviewer is still working, there is no gate yet).
	result, err = m.HandleAnswerReviewQuestion(runID, "q2", "b", "firstmate")
	if err != nil {
		t.Fatalf("answer q2: %v", err)
	}
	if !result.OK || result.Open != 0 {
		t.Fatalf("q2 result = %+v, want ok with nothing open", result)
	}
	if result.Resumed {
		t.Fatalf("q2 result = %+v, want not resumed: there is no parked gate here", result)
	}
	if !strings.Contains(result.Note, "not released") {
		t.Fatalf("note = %q, want it to say the gate was not released", result.Note)
	}

	// Both answers survived, attributed, whatever the gate did.
	conv, err := reviewqa.Load(dir)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(conv.Open()) != 0 || len(conv.Answered()) != 2 {
		t.Fatalf("conversation = %+v, want 2 answered and 0 open", conv)
	}
	byID := map[string]reviewqa.Answer{}
	for _, e := range conv.Answered() {
		byID[e.ID] = *e.Answer
	}
	if byID["q1"].Answer != "a" || byID["q1"].AnsweredBy != "captain" {
		t.Fatalf("q1 = %+v", byID["q1"])
	}
	if byID["q2"].Answer != "b" || byID["q2"].AnsweredBy != "firstmate" {
		t.Fatalf("q2 = %+v", byID["q2"])
	}
}

// A correction is another append, and the last one wins - matching the file
// protocol, so the operator never has to undo an answer.
func TestAnswerReviewQuestionCorrectionReplacesTheEarlierAnswer(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	appendAgentQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep it?","options":["keep","drop"],"weight":"major"}`)
	for _, answer := range []string{"keep", "drop, on reflection"} {
		if _, err := m.HandleAnswerReviewQuestion(runID, "q1", answer, "captain"); err != nil {
			t.Fatalf("answer: %v", err)
		}
	}
	conv, err := reviewqa.Load(dir)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(conv.Answered()) != 1 || conv.Answered()[0].Answer.Answer != "drop, on reflection" {
		t.Fatalf("conversation = %+v, want the last answer to win", conv)
	}
}

func TestAnswerReviewQuestionRefusesIncompleteOrUnknownInput(t *testing.T) {
	m, _, runID := answerFixture(t)

	if _, err := m.HandleAnswerReviewQuestion(runID, "", "a", ""); err == nil {
		t.Fatal("want an error with no question id")
	}
	if _, err := m.HandleAnswerReviewQuestion(runID, "q1", "   ", ""); err == nil {
		t.Fatal("want an error with a blank answer")
	}
	// No active executor means no run that could be resumed, and no owner of
	// the conversation path either.
	if _, err := m.HandleAnswerReviewQuestion("no-such-run", "q1", "a", ""); err == nil {
		t.Fatal("want an error for a run with no active executor")
	}
}

// An answer for a question nobody asked is recorded and ignored rather than
// failing: the writer may be racing a question it has not read yet. It must not
// be counted as open, or it would park a run on a question the reviewer never
// asked.
func TestAnswerReviewQuestionForAnUnknownQuestionDoesNotOpenOne(t *testing.T) {
	m, p, runID := answerFixture(t)
	result, err := m.HandleAnswerReviewQuestion(runID, "ghost", "a", "captain")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if result.Open != 0 {
		t.Fatalf("result = %+v, want nothing open", result)
	}
	conv, err := reviewqa.Load(conversationDir(p, runID))
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(conv.Entries) != 0 {
		t.Fatalf("entries = %+v, want none", conv.Entries)
	}
	if !strings.Contains(strings.Join(conv.Notes, "\n"), "unknown question") {
		t.Fatalf("orphan answer not disclosed: %v", conv.Notes)
	}
}

// TestAnswerReviewQuestionRefusesWhenTheConversationIsOff is the opt-in half of
// the answer channel. A repository that has not set review.conversation has no
// reviewer that was ever told to ask, so an answer has nothing to settle and
// nothing to release - and the refusal has to name the setting that would
// accept one, or an operator reading "no review conversation directory" would
// go looking for a missing directory instead of an unset key.
func TestAnswerReviewQuestionRefusesWhenTheConversationIsOff(t *testing.T) {
	m, p, runID := answerFixtureWithConversation(t, false)

	// Nothing on disk: a repository that never opted in cannot have a
	// conversation, so the setting is the only thing that could accept an
	// answer. A question that IS on disk was asked while the channel was open
	// and stays answerable; that case has its own test below.

	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "keep", "captain")
	if err == nil {
		t.Fatalf("answering with the conversation off must fail, got %+v", result)
	}
	if !strings.Contains(err.Error(), "review.conversation") {
		t.Fatalf("refusal does not name the setting that would accept an answer: %v", err)
	}

	// Nothing recorded: a refused answer must not leave a half-written channel
	// a later enabled run would read as settled.
	if answers, readErr := os.ReadFile(filepath.Join(conversationDir(p, runID), reviewqa.AnswersFile)); readErr == nil {
		t.Fatalf("a refused answer was written to disk: %s", answers)
	}
}

// TestAnswerReviewQuestionOnDiskQuestionSurvivesTheSettingBeingTurnedOff pins
// the one exception to the opt-in refusal above.
//
// The reviewer asked while the channel was open - the file on disk is the proof
// - and review.conversation was turned off, or a trusted-config fetch failed
// and recovery resolved it the same way, before the answer landed. Refusing
// then strands a parked run for good. The review step reads the same
// conversation from disk for its finalize turn, so the answer reaches the
// agent rather than merely unsticking the gate.
func TestAnswerReviewQuestionOnDiskQuestionSurvivesTheSettingBeingTurnedOff(t *testing.T) {
	m, p, runID := answerFixtureWithConversation(t, false)

	appendAgentQuestionLine(t, conversationDir(p, runID), `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)

	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "keep", "captain")
	if err != nil {
		t.Fatalf("a question already on disk must stay answerable after the setting is turned off: %v", err)
	}
	if result == nil || !result.OK {
		t.Fatalf("result = %+v, want an accepted answer", result)
	}
	answers, readErr := os.ReadFile(filepath.Join(conversationDir(p, runID), reviewqa.AnswersFile))
	if readErr != nil {
		t.Fatalf("the accepted answer was not written to disk: %v", readErr)
	}
	if !strings.Contains(string(answers), "q1") {
		t.Fatalf("the answer on disk does not carry its question id: %s", answers)
	}
}

// parkingReviewStep parks a review gate on one ordinary ask-user CODE finding -
// no review question involved - which is the gate an orphan answer used to be
// able to steal.
type parkingReviewStep struct{ entered chan struct{} }

func (s *parkingReviewStep) Name() types.StepName { return types.StepReview }

func (s *parkingReviewStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	return &pipeline.StepOutcome{
		Findings: `{"findings":[{"id":"review-1","severity":"warning","description":"needs an operator decision","action":"ask-user"}],"summary":"one issue"}`,
	}, nil
}

// liveParkedGateFixture stands up a REAL executor parked at a review gate, so
// "was the gate released" is observable rather than inferred. answerFixture's
// bare executor is never waiting, so exec.Respond fails there for every input
// and could not tell a correct refusal from the bug.
func liveParkedGateFixture(t *testing.T) (*RunManager, *paths.Paths, string, *pipeline.Executor) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", "head-1", "base-1")
	if err != nil {
		t.Fatal(err)
	}

	step := &parkingReviewStep{entered: make(chan struct{}, 1)}
	cfg := &config.Config{Review: config.Review{Conversation: true}}
	exec := pipeline.NewExecutor(database, p, cfg, nil, []pipeline.Step{step}, nil)

	m := NewRunManager(database, p, nil)
	m.mu.Lock()
	m.executors[run.ID] = exec
	m.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("parked executor never finished")
		}
	})

	select {
	case <-step.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("review step never ran")
	}
	// Wait until the gate is genuinely registered as waiting, which is what
	// makes a release observable. The probe names a DIFFERENT step on purpose:
	// RespondWithOverrides checks e.waiting first and the step name second, and
	// returns on a mismatch before clearing e.waiting, so this distinguishes
	// "not waiting yet" from "waiting" without consuming the gate. A probe with
	// the real step name would answer it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepTest, types.ActionApprove, nil)
		if err == nil {
			t.Fatal("a probe naming another step must never be accepted")
		}
		if strings.Contains(err.Error(), "step mismatch") {
			break
		}
		if !strings.Contains(err.Error(), "no step awaiting approval") {
			t.Fatalf("unexpected probe error: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("review gate never registered as awaiting approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return m, p, run.ID, exec
}

// TestAnswerReviewQuestionOrphanAnswerLeavesAParkedGateAlone is the regression
// for the release rule. The handler used to decide purely from
// len(conv.Open()) == 0 after the append, so an answer for an id nobody asked -
// a typo, or an id carried over from a previous run - was recorded as an orphan,
// left the open count at zero, and still fired
// exec.Respond(StepReview, ActionAnswer). When the gate was parked on ordinary
// ask-user CODE findings that released it: the step re-executed as a finalize
// turn, burned a review round, the operator's verdict never happened, and their
// follow-up axi respond failed with "no step awaiting approval".
//
// TestAnswerReviewQuestionForAnUnknownQuestionDoesNotOpenOne cannot catch this,
// because its fixture has no parked gate at all.
func TestAnswerReviewQuestionOrphanAnswerLeavesAParkedGateAlone(t *testing.T) {
	m, p, runID, exec := liveParkedGateFixture(t)

	result, err := m.HandleAnswerReviewQuestion(runID, "q-typo", "keep", "captain")
	if err != nil {
		t.Fatalf("an orphan answer must still be recorded: %v", err)
	}
	if result.Resumed {
		t.Fatal("an answer that closed no open question resumed the reviewer")
	}
	if result.Open != 0 {
		t.Fatalf("open = %d, want 0", result.Open)
	}

	// Recorded durably all the same, so it is never silently lost.
	answers, readErr := os.ReadFile(filepath.Join(conversationDir(p, runID), reviewqa.AnswersFile))
	if readErr != nil || !strings.Contains(string(answers), "q-typo") {
		t.Fatalf("the orphan answer was not recorded: err=%v content=%q", readErr, answers)
	}

	// The property that matters: the operator's verdict is still possible.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("the orphan answer stole the operator's verdict: %v", err)
	}
}

// TestAnswerReviewQuestionDuplicateAnswerLeavesAParkedGateAlone covers the same
// rule for the other way the open count reaches zero without this answer
// closing anything: a correction or a resend arriving after the last question
// was already answered.
func TestAnswerReviewQuestionDuplicateAnswerLeavesAParkedGateAlone(t *testing.T) {
	m, p, runID, exec := liveParkedGateFixture(t)
	dir := conversationDir(p, runID)

	// Trimmed to the fields the prompt calls required - no kind, no weight -
	// which is the other shape a model following that prompt produces.
	appendAgentQuestionLine(t, dir, `{"id":"q1","question":"keep the legacy route?","options":["keep","remove"]}`)
	if err := reviewqa.AppendAnswer(dir, reviewqa.Answer{ID: "q1", Answer: "keep", AskOrdinal: 1}); err != nil {
		t.Fatalf("seed answer: %v", err)
	}
	// Assert the precondition rather than assuming it: an empty conversation
	// also has nothing open, so a seed the reader discarded would let this
	// test pass without ever exercising a duplicate answer.
	if seeded, err := reviewqa.Load(dir); err != nil {
		t.Fatal(err)
	} else if len(seeded.Answered()) != 1 {
		t.Fatalf("seeded question did not read back as answered: %+v", seeded)
	}

	// q1 is already closed, so this resend closes nothing.
	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "keep, behind a flag", "captain")
	if err != nil {
		t.Fatalf("a corrected answer must still be recorded: %v", err)
	}
	if result.Resumed {
		t.Fatal("a duplicate answer resumed the reviewer")
	}
	answers, readErr := os.ReadFile(filepath.Join(dir, reviewqa.AnswersFile))
	if readErr != nil || !strings.Contains(string(answers), "behind a flag") {
		t.Fatalf("the corrected answer was not recorded: err=%v content=%q", readErr, answers)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("the duplicate answer stole the operator's verdict: %v", err)
	}
}

// TestAnswerReviewQuestionCorrectionDoesNotPreAnswerAReAsk drives the whole
// answer path for the sequence every step of which is ordinary: the reviewer
// asks q1, the operator answers it, the operator then corrects that answer -
// which axi answer advertises as recorded for the reviewer's next checkpoint -
// and a later cold rereview, shown only the still-OPEN questions, re-uses q1
// for a genuinely different question.
//
// Before the answer carried the ask it settles, that left two asks and two
// answers, which reviewqa.Load could only read as "settled": the new question
// arrived pre-answered, no question finding was emitted, and the gate never
// parked on it.
func TestAnswerReviewQuestionCorrectionDoesNotPreAnswerAReAsk(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	appendAgentQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)

	for _, answer := range []string{"keep", "remove, on reflection"} {
		if _, err := m.HandleAnswerReviewQuestion(runID, "q1", answer, "captain"); err != nil {
			t.Fatalf("answer %q: %v", answer, err)
		}
	}

	appendAgentQuestionLine(t, dir, `{"id":"q1","question":"should /v3 answer too?","options":["yes","no"]}`)

	conv, err := reviewqa.Load(dir)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	open := conv.Open()
	if len(open) != 1 || open[0].Question.Question != "should /v3 answer too?" {
		t.Fatalf("the re-asked question arrived pre-answered: %+v", conv.Entries)
	}
	settled := conv.SettledAsks()
	if len(settled) != 1 || settled[0].Question.Question != "keep the legacy route?" || settled[0].Answer.Answer != "remove, on reflection" {
		t.Fatalf("the correction did not stay bound to the ask it corrects: %#v", settled)
	}
}

// TestAnswerReviewQuestionRefusesWhenTheConversationCannotBeRead closes the one
// path that still reproduced the defect the ask ordinal exists to close. The
// stamp comes from the pre-append snapshot, so a conversation that could not be
// read used to fall through and append an UNSTAMPED answer - and an unstamped
// answer settles nothing at all, so the question it was meant for parks forever
// while the operator is told it was recorded.
//
// The refusal must also name the read failure. "It answered no open question" is
// the truthful report for a conversation that reads fine with nothing open, and
// telling an operator that about a conversation nobody could read sends them
// looking for the wrong thing.
func TestAnswerReviewQuestionRefusesWhenTheConversationCannotBeRead(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A self-referencing symlink: reading questions.ndjson fails for every uid,
	// with no dependence on file permissions, while answers.ndjson stays
	// perfectly appendable - so a handler that writes through a read failure
	// really does land its unstamped line here.
	if err := os.Symlink(reviewqa.QuestionsFile, filepath.Join(dir, reviewqa.QuestionsFile)); err != nil {
		t.Skipf("this platform will not create the unreadable fixture: %v", err)
	}

	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "keep", "captain")
	if err == nil {
		t.Fatalf("an unreadable conversation must refuse the answer, got %+v", result)
	}
	// The refusal is before the write, so no unstamped answer that settles
	// nothing was recorded.
	if answers, readErr := os.ReadFile(filepath.Join(dir, reviewqa.AnswersFile)); !os.IsNotExist(readErr) {
		t.Fatalf("an unstamped answer was recorded through the read failure: err=%v content=%q", readErr, answers)
	}
	if !strings.Contains(err.Error(), "read run "+runID+"'s review conversation") {
		t.Fatalf("the refusal does not name the read failure: %v", err)
	}
	if strings.Contains(err.Error(), "no open question") {
		t.Fatalf("an unreadable conversation was reported as having nothing open: %v", err)
	}
}

// TestAnswerReviewQuestionRefusesAnIncompleteQuestionHistory is the write-side
// half of the rule the reader already applies: a questions.ndjson the scan
// cannot reach the end of settles nothing, ever, so an answer stamped against
// it could never close its question and the gate would park forever with the
// operator told they had answered it. The refusal must name THAT cause rather
// than the ordinary "no question was open", which is a different, non-error
// outcome, and it must write nothing at all.
func TestAnswerReviewQuestionRefusesAnIncompleteQuestionHistory(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	appendAgentQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)
	// Over the reader's per-line budget, so its scan stops here and whatever
	// follows - including a later ask of q1 - is never seen.
	appendAgentQuestionLine(t, dir, `{"id":"q2","kind":"question","question":"`+strings.Repeat("x", 64<<10)+`"}`)

	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "keep", "captain")
	if err == nil {
		t.Fatalf("answering against an incomplete question history must fail, got %+v", result)
	}
	if !strings.Contains(err.Error(), "could not be read to the end") {
		t.Fatalf("refusal does not name the incompleteness: %v", err)
	}
	if strings.Contains(err.Error(), "no open question") {
		t.Fatalf("refusal reads as the ordinary nothing-was-open case: %v", err)
	}

	if answers, readErr := os.ReadFile(filepath.Join(dir, reviewqa.AnswersFile)); readErr == nil {
		t.Fatalf("a refused answer was written to disk: %s", answers)
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("read answers file: %v", readErr)
	}
}
