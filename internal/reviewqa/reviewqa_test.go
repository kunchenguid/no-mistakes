package reviewqa

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// appendQuestionLine appends ONE verbatim questions.ndjson line, which is the
// only way that file is ever written in production: the reviewer agent appends
// it with its own file tools, so there is no Go writer to reuse. Fixtures spell
// the JSON out so the shape under test - in particular which fields are ABSENT
// - is visible at the call site.
func appendQuestionLine(t *testing.T, dir, line string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, QuestionsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingDirectoryIsAnEmptyConversation(t *testing.T) {
	conv, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != 0 || len(conv.Open()) != 0 {
		t.Fatalf("want empty conversation, got %+v", conv)
	}
	if conv, err := Load(""); err != nil || len(conv.Entries) != 0 {
		t.Fatalf("empty dir: %+v %v", conv, err)
	}
}

func TestOpenAnsweredAndWithdrawnPartitionTheConversation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","kind":"question","question":"keep /v1?","options":["keep","drop"],"weight":"major"}`,
		`{"id":"q2","kind":"question","question":"flag default?","options":["on","off"]}`,
		`{"id":"q3","kind":"question","question":"rename table?","options":["yes","no"]}`,
		`{"id":"q3","kind":"retract","reason":"answered by the migration note"}`,
	)
	write(t, dir, AnswersFile,
		`{"id":"q2","answer":"off","answered_by":"captain","ask_ordinal":1}`,
	)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(conv.Entries); got != 3 {
		t.Fatalf("entries = %d, want 3", got)
	}
	open := conv.Open()
	if len(open) != 1 || open[0].ID != "q1" {
		t.Fatalf("open = %+v, want only q1", open)
	}
	answered := conv.Answered()
	if len(answered) != 1 || answered[0].ID != "q2" || answered[0].Answer.Answer != "off" {
		t.Fatalf("answered = %+v, want q2=off", answered)
	}
	withdrawn := conv.Withdrawn()
	if len(withdrawn) != 1 || withdrawn[0].ID != "q3" {
		t.Fatalf("withdrawn = %+v, want only q3", withdrawn)
	}
	// Order is first-asked, so a consumer renders the conversation as it happened.
	if conv.Entries[0].ID != "q1" || conv.Entries[2].ID != "q3" {
		t.Fatalf("order = %v", []string{conv.Entries[0].ID, conv.Entries[1].ID, conv.Entries[2].ID})
	}
}

func TestLaterLinesSupersedeEarlierOnesForTheSameID(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","question":"first wording","options":["a"]}`,
		`{"id":"q1","question":"sharper wording","options":["a","b"]}`,
		`{"id":"q2","question":"withdrawn then re-asked","options":["a"]}`,
		`{"id":"q2","kind":"retract","reason":"thought it was settled"}`,
		`{"id":"q2","question":"withdrawn then re-asked","options":["a"]}`,
	)
	// Both stamped for q1's SECOND ask: the daemon stamps with the id's ask
	// count at the moment of the append, and the edit was already on disk.
	write(t, dir, AnswersFile,
		`{"id":"q1","answer":"a","ask_ordinal":2}`,
		`{"id":"q1","answer":"b, on reflection","ask_ordinal":2}`,
	)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (an edit is not a duplicate)", len(conv.Entries))
	}
	if conv.Entries[0].Question.Question != "sharper wording" {
		t.Fatalf("question = %q, want the last wording", conv.Entries[0].Question.Question)
	}
	if conv.Entries[0].Answer.Answer != "b, on reflection" {
		t.Fatalf("answer = %q, want the last answer", conv.Entries[0].Answer.Answer)
	}
	// Re-asking revives a retracted question: it is how the reviewer says the
	// retraction was wrong, and it must block again.
	if !conv.Entries[1].Open() {
		t.Fatalf("re-asked question should be open again, got %+v", conv.Entries[1])
	}
}

func TestMalformedMinorAndOrphanLinesAreNotedNotFatal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","question":"real","options":["a"]}`,
		`not json at all`,
		`{"question":"no id","options":["a"]}`,
		`{"id":"q9","question":"minor thing","weight":"minor","options":["a"]}`,
		`{"id":"q8","question":"","options":["a"]}`,
		`{"id":"q7","kind":"shout","question":"odd kind"}`,
		`{"id":"q6","kind":"retract"}`,
	)
	write(t, dir, AnswersFile,
		`{"id":"q1","answer":"a","ask_ordinal":1}`,
		`{"id":"nope","answer":"whatever"}`,
		`{"id":"q1","answer":"","ask_ordinal":1}`,
		`garbage`,
	)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != 1 || conv.Entries[0].ID != "q1" {
		t.Fatalf("entries = %+v, want only q1", conv.Entries)
	}
	if !conv.Entries[0].Answered() {
		t.Fatalf("q1 should be answered")
	}
	// A minor question is a protocol violation, never an escalation: it must
	// not become an open question the run parks on.
	if len(conv.Open()) != 0 {
		t.Fatalf("open = %+v, want none", conv.Open())
	}
	joined := strings.Join(conv.Notes, "\n")
	for _, want := range []string{"malformed questions", "no id", "minor-weight question", "no question text", "unknown kind", "retraction for unknown question", "answer for unknown question", "malformed answers"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("notes missing %q:\n%s", want, joined)
		}
	}
}

// TestAppendAnswerRoundTripsAndValidates covers the one writer this package
// owns. There is deliberately no question-writing counterpart: the reviewer
// appends questions itself (see the package comment), so the question side's
// contract is the READER's tolerance, covered by the Load tests.
func TestAppendAnswerRoundTripsAndValidates(t *testing.T) {
	dir := Dir(t.TempDir())
	// The line the reviewer's prompt actually produces: no asked_at, which
	// nothing in that prompt mentions.
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep /v1?","options":["keep","drop"],"weight":"major"}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "drop", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatalf("AppendAnswer: %v", err)
	}
	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Answered()) != 1 {
		t.Fatalf("conversation = %+v", conv)
	}
	entry := conv.Answered()[0]
	if entry.Answer.AnsweredAt == "" {
		t.Fatalf("answered_at default not applied: %+v", entry.Answer)
	}

	if err := AppendAnswer(dir, Answer{ID: "q2"}); err == nil {
		t.Fatal("want error for an answer with no text")
	}
	if err := AppendAnswer(dir, Answer{Answer: "x"}); err == nil {
		t.Fatal("want error for an answer with no question id")
	}
	if err := AppendAnswer("", Answer{ID: "q2", Answer: "x"}); err == nil {
		t.Fatal("want error for an unset directory")
	}
	if err := AppendAnswer(dir, Answer{ID: "big", Answer: strings.Repeat("x", maxLineBytes)}); err == nil {
		t.Fatal("want error for an overlong line")
	}
}

func TestLoadKeepsTheNewestLinesWhenTheFileIsOverBound(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, 0, maxLines+10)
	for i := 0; i < maxLines+10; i++ {
		lines = append(lines, fmt.Sprintf(`{"id":"q%d","question":"q %d","options":["a"]}`, i, i))
	}
	write(t, dir, QuestionsFile, lines...)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != maxLines {
		t.Fatalf("entries = %d, want the %d newest", len(conv.Entries), maxLines)
	}
	// Newest kept: a later line supersedes an earlier one, so dropping the
	// oldest is the only truncation that cannot lose a supersede.
	if conv.Entries[0].ID != "q10" {
		t.Fatalf("first kept = %q, want q10", conv.Entries[0].ID)
	}
	if !strings.Contains(strings.Join(conv.Notes, "\n"), "size bound") {
		t.Fatalf("truncation not disclosed: %v", conv.Notes)
	}
}

func TestDirIsUnderTheRunEvidenceDirectory(t *testing.T) {
	if got := Dir("/var/evidence/run1"); got != filepath.Join("/var/evidence/run1", "review") {
		t.Fatalf("Dir = %q", got)
	}
	if got := Dir("   "); got != "" {
		t.Fatalf("Dir(blank) = %q, want empty", got)
	}
}

// TestLoadSupersedingQuestionReopensAnAnsweredEntry is the regression for the
// way a major question could be silently discarded.
//
// Question ids are chosen by the agent - the protocol's worked example is
// literally "q1" - and the conversation directory is per RUN, so every review
// turn of a run appends to one file. A cold rereview in a fix round is shown
// only the OPEN questions, so it reuses "q1" for a genuinely different
// question. While the edit branch kept the earlier answer attached, that new
// question arrived pre-answered: Open() was empty, no ask-user finding was
// emitted, the gate never parked, and the reviewer's question reached nobody.
func TestLoadSupersedingQuestionReopensAnAnsweredEntry(t *testing.T) {
	dir := t.TempDir()

	// The prompt's worked example, minus asked_at, which it never mentions.
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy /v1 route?","options":["keep","remove"],"weight":"major"}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "keep", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Open()) != 0 || len(conv.Answered()) != 1 {
		t.Fatalf("answered question is not settled: open=%d answered=%d", len(conv.Open()), len(conv.Answered()))
	}

	// A later turn reuses the id for a different question, and this time the
	// model trims the example down to the fields it was told are required -
	// no kind, no weight - which the reader must still take as a major
	// question rather than skipping or dropping it.
	appendQuestionLine(t, dir, `{"id":"q1","question":"should the new /v3 route answer too?","options":["yes","no"]}`)
	conv, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	open := conv.Open()
	if len(open) != 1 {
		t.Fatalf("a superseding question did not re-open the entry: open=%d answered=%d", len(open), len(conv.Answered()))
	}
	if open[0].Question.Question != "should the new /v3 route answer too?" {
		t.Fatalf("open question text = %q", open[0].Question.Question)
	}
	if open[0].Answer != nil {
		t.Fatalf("the superseded answer is still attached: %#v", open[0].Answer)
	}
	if len(conv.Answered()) != 0 {
		t.Fatalf("the new question still reads as answered: %#v", conv.Answered())
	}

	// Answering it again settles the NEW question, with the new text.
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "no", AnsweredBy: "captain", AskOrdinal: 2}); err != nil {
		t.Fatal(err)
	}
	conv, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	answered := conv.Answered()
	if len(answered) != 1 || answered[0].Answer.Answer != "no" || answered[0].Question.Question != "should the new /v3 route answer too?" {
		t.Fatalf("re-answer did not settle the new question: %#v", answered)
	}
}

// Re-asking a question the reviewer had retracted still revives it, and it
// comes back OPEN rather than carrying whatever answer preceded the
// retraction.
func TestLoadReAskAfterRetractionComesBackOpen(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)
	// The retraction exactly as the prompt spells it: no `at`, which the
	// deleted Go writer used to supply.
	appendQuestionLine(t, dir, `{"id":"q1","kind":"retract","reason":"the migration note answers it"}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "keep", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route after all?","options":["keep","remove"],"weight":"major"}`)
	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Withdrawn()) != 0 {
		t.Fatalf("re-asking did not revive the question: %#v", conv.Withdrawn())
	}
	if len(conv.Open()) != 1 {
		t.Fatalf("a revived question must be open, not pre-answered: open=%d answered=%d", len(conv.Open()), len(conv.Answered()))
	}
}

// TestSettledAsksKeepEveryDecisionForAReusedID is the load half of the
// re-used-id defect. Entry collapses an id to its latest state, which is what
// the gate needs, so it cannot supply the earlier (question, answer) pair - and
// the durable store has to, or a human's decision disappears.
func TestSettledAsksKeepEveryDecisionForAReusedID(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"Is the legacy route deliberate?","options":["yes","no"],"weight":"major"}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "yes", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	// A cold rereview in a fix round is shown only OPEN questions, so it
	// re-uses q1 for a different question.
	appendQuestionLine(t, dir, `{"id":"q1","question":"Should the new /v3 route answer too?","options":["yes","no"]}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "no", AnsweredBy: "captain", AskOrdinal: 2}); err != nil {
		t.Fatal(err)
	}

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	settled := conv.SettledAsks()
	if len(settled) != 2 {
		t.Fatalf("settled asks = %d, want both decisions: %#v", len(settled), settled)
	}
	if settled[0].Ordinal != 1 || settled[0].Question.Question != "Is the legacy route deliberate?" || settled[0].Answer.Answer != "yes" {
		t.Fatalf("ask 1 lost its own pairing: %#v / %#v", settled[0].Question, settled[0].Answer)
	}
	if settled[1].Ordinal != 2 || settled[1].Question.Question != "Should the new /v3 route answer too?" || settled[1].Answer.Answer != "no" {
		t.Fatalf("ask 2 is mispaired: %#v / %#v", settled[1].Question, settled[1].Answer)
	}
	// Entry still reports only the latest, which the gate depends on.
	if answered := conv.Answered(); len(answered) != 1 || answered[0].Answer.Answer != "no" {
		t.Fatalf("Answered() should still collapse to the latest state: %#v", answered)
	}
}

// A correction to the SAME ask still replaces rather than accumulating, so the
// contract the intent states is unchanged.
func TestSettledAsksTreatASurplusAnswerAsACorrection(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep it?","options":["keep","drop"],"weight":"major"}`)
	// Both stamped for ask 1, as the daemon stamps them: the second is the
	// operator correcting the first.
	for _, a := range []string{"keep", "drop, on reflection"} {
		if err := AppendAnswer(dir, Answer{ID: "q1", Answer: a, AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
			t.Fatal(err)
		}
	}

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	settled := conv.SettledAsks()
	if len(settled) != 1 {
		t.Fatalf("a correction accumulated instead of replacing: %#v", settled)
	}
	if settled[0].Answer.Answer != "drop, on reflection" {
		t.Fatalf("the correction did not win: %#v", settled[0].Answer)
	}
}

// An id asked twice with only one answer settles NOTHING: the re-ask is still
// open and the gate must park again, so the earlier ask is not reported settled
// off the strength of an answer that belongs to it alone.
func TestSettledAsksLeaveAReAskShortOfAnAnswerOpen(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"first","options":["a","b"],"weight":"major"}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "a", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"second","options":["a","b"],"weight":"major"}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Open()) != 1 {
		t.Fatalf("the re-ask must stay open: open=%d", len(conv.Open()))
	}
	settled := conv.SettledAsks()
	if len(settled) != 1 || settled[0].Ordinal != 1 || settled[0].Answer.Answer != "a" {
		t.Fatalf("ask 1 should be settled by its own answer and ask 2 open: %#v", settled)
	}
}

// TestASurplusAnswerBindsToItsOwnAskAndLeavesTheReAskOpen is the regression for
// the hole the ask/answer counting alone left open. A correction sent after an
// ask was already settled is an ordinary thing to do - axi answer advertises
// it - and it used to make asks and answers match, so the NEXT re-ask of that
// id (a cold rereview is shown only the OPEN questions, so it starts numbering
// at q1 again for a genuinely different question) arrived pre-answered: nothing
// was open, no question finding was emitted, the gate never parked, and a major
// question reached nobody.
//
// TestSettledAsksTreatASurplusAnswerAsACorrection has the surplus answer with
// no re-ask, and TestSettledAsksLeaveAReAskShortOfAnAnswerOpen has the re-ask
// with no surplus; neither exercises the two together.
func TestASurplusAnswerBindsToItsOwnAskAndLeavesTheReAskOpen(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"Is the legacy route deliberate?","options":["yes","no"],"weight":"major"}`)
	// Both stamped for ask 1, as the daemon stamps them: the second is the
	// operator correcting the first.
	for _, a := range []string{"yes", "no, on reflection"} {
		if err := AppendAnswer(dir, Answer{ID: "q1", Answer: a, AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if settled, err := Load(dir); err != nil {
		t.Fatal(err)
	} else if len(settled.Answered()) != 1 {
		t.Fatalf("the seeded ask did not read back as answered: %+v", settled)
	}

	appendQuestionLine(t, dir, `{"id":"q1","question":"Should the new /v3 route answer too?","options":["yes","no"]}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	open := conv.Open()
	if len(open) != 1 || open[0].Question.Question != "Should the new /v3 route answer too?" {
		t.Fatalf("the re-ask must stay open and park the gate again: %+v", conv.Entries)
	}
	settled := conv.SettledAsks()
	if len(settled) != 1 || settled[0].Ordinal != 1 {
		t.Fatalf("want only ask 1 settled: %#v", settled)
	}
	if settled[0].Question.Question != "Is the legacy route deliberate?" || settled[0].Answer.Answer != "no, on reflection" {
		t.Fatalf("ask 1 lost its correction or took the wrong question: %#v / %#v", settled[0].Question, settled[0].Answer)
	}
}

// TestAnOrphanAnswerNeverSettlesALaterAskOfThatID is the regression for the
// last path that pre-answered a genuinely different question. An answer for an
// id nobody has asked is written UNSTAMPED - there is no ask to stamp - and the
// positional fallback then handed it to the first LATER ask of that id: the
// gate never parked and the reviewer's question reached nobody.
//
// Ids are the agent's own tiny space, so a mistyped `axi answer --question q7`
// names an id the reviewer is likely to use later, and the orphan is recorded
// with no error by design.
func TestAnOrphanAnswerNeverSettlesALaterAskOfThatID(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)
	// Answering an id nobody has asked: the daemon finds no ask for it, so the
	// line carries no ask_ordinal.
	if err := AppendAnswer(dir, Answer{ID: "q7", Answer: "remove it", AnsweredBy: "captain"}); err != nil {
		t.Fatal(err)
	}
	if seeded, err := Load(dir); err != nil {
		t.Fatal(err)
	} else if len(seeded.Open()) != 1 || seeded.Open()[0].ID != "q1" {
		t.Fatalf("the seed did not read back as one open question: %+v", seeded)
	}

	appendQuestionLine(t, dir, `{"id":"q7","question":"should /v3 answer too?","options":["yes","no"]}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var q7 *Entry
	for i := range conv.Entries {
		if conv.Entries[i].ID == "q7" {
			q7 = &conv.Entries[i]
		}
	}
	if q7 == nil {
		t.Fatalf("q7 was not read back at all: %+v", conv.Entries)
	}
	if !q7.Open() {
		t.Fatalf("the orphan answer pre-settled a question it was never about: %#v", q7)
	}
	if len(conv.SettledAsks()) != 0 {
		t.Fatalf("an orphan answer settled an ask: %#v", conv.SettledAsks())
	}

	// The orphan is still on disk: recorded and inert is the contract, not
	// refused.
	answers, err := os.ReadFile(filepath.Join(dir, AnswersFile))
	if err != nil || !strings.Contains(string(answers), "remove it") {
		t.Fatalf("the orphan answer was not kept: err=%v content=%q", err, answers)
	}
}

// TestARetractedAskIsNeverSettled covers the answer that lands on a question
// the reviewer has just withdrawn. The retract line marks the entry retracted
// but adds no ask and removes none, so the answer is stamped with that ask's
// ordinal and paired with it - which used to make the withdrawn question a
// SETTLED one: it was written to review_questions as a standing branch
// decision nothing deletes, and rendered twice in the PR body, once as an
// answered pair and once as withdrawn.
//
// The answer is still recorded. This is a settling rule, not a refusal.
func TestARetractedAskIsNeverSettled(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)
	appendQuestionLine(t, dir, `{"id":"q1","kind":"retract","reason":"the migration note answers it"}`)
	// The operator saw q1 before the retraction. The daemon stamps it with the
	// id's ask count, which the retraction did not change.
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "keep", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Withdrawn()) != 1 {
		t.Fatalf("the seed did not read back as one withdrawn question: %+v", conv.Entries)
	}
	if settled := conv.SettledAsks(); len(settled) != 0 {
		t.Fatalf("a withdrawn question was settled by a late answer: %#v", settled)
	}

	answers, err := os.ReadFile(filepath.Join(dir, AnswersFile))
	if err != nil || !strings.Contains(string(answers), `"keep"`) {
		t.Fatalf("the answer was not recorded: err=%v content=%q", err, answers)
	}
}

// TestARetractionLeavesAnEarlierSettledAskAlone is the precision half of the
// rule above. Skipping every ask of a retracted id would drop a decision a
// human really gave: an id asked, answered, re-asked and then withdrawn inside
// one turn is recorded once, at the end of that turn, so ask 1's answer would
// reach no store at all - the same silent loss the ask ordinal joined the
// durable key to prevent.
func TestARetractionLeavesAnEarlierSettledAskAlone(t *testing.T) {
	dir := t.TempDir()
	appendQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "keep", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	appendQuestionLine(t, dir, `{"id":"q1","question":"should /v3 answer too?","options":["yes","no"]}`)
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "no", AnsweredBy: "captain", AskOrdinal: 2}); err != nil {
		t.Fatal(err)
	}
	appendQuestionLine(t, dir, `{"id":"q1","kind":"retract","reason":"the migration note answers it"}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	settled := conv.SettledAsks()
	if len(settled) != 1 || settled[0].Ordinal != 1 {
		t.Fatalf("want only ask 1 settled, got %#v", settled)
	}
	if settled[0].Question.Question != "keep the legacy route?" || settled[0].Answer.Answer != "keep" {
		t.Fatalf("ask 1 lost its own pairing: %#v / %#v", settled[0].Question, settled[0].Answer)
	}
}

// TestLineCapDroppingAQuestionLineSettlesNothing drives the real retention
// path: the file passes maxLines and its leading lines are dropped, so the
// history can no longer be read in full and no stamped answer may settle an
// ask. The dropped prefix may hold an OPEN question that has no Entry here at
// all, so reporting anything as settled could release the gate on a major
// question nobody saw.
func TestLineCapDroppingAQuestionLineSettlesNothing(t *testing.T) {
	dir := t.TempDir()
	lines := []string{`{"id":"q1","question":"first ask","options":["a","b"]}`}
	for i := 0; i < maxLines+3; i++ {
		lines = append(lines, fmt.Sprintf(`{"id":"filler%d","question":"f %d","options":["a"]}`, i, i))
	}
	// The second ask of q1 is the newest line, so it survives the cap while
	// the first ask is in the dropped prefix.
	lines = append(lines, `{"id":"q1","question":"second ask","options":["a","b"]}`)
	write(t, dir, QuestionsFile, lines...)
	write(t, dir, AnswersFile, `{"id":"q1","answer":"b","ask_ordinal":2}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var q1 *Entry
	for i := range conv.Entries {
		if conv.Entries[i].ID == "q1" {
			q1 = &conv.Entries[i]
		}
	}
	if q1 == nil {
		t.Fatal("q1 was dropped by the cap; the fixture no longer exercises the retained re-ask")
	}
	if q1.Question.Question != "second ask" {
		t.Fatalf("q1 = %q, want the retained second ask", q1.Question.Question)
	}
	if !conv.QuestionsIncomplete {
		t.Fatal("the line cap dropped question lines but the history reads as complete")
	}
	if q1.Answered() {
		t.Fatalf("an incomplete question history settled an ask: %+v", q1)
	}
	if len(conv.SettledAsks()) != 0 {
		t.Fatalf("an incomplete question history reported settled asks: %+v", conv.SettledAsks())
	}
}

// TestLoadSettlesNothingWhenTheQuestionFileWasNotReadToTheEnd covers the other
// truncation: a scan that stopped short cannot know an id's later asks, so the
// last line it saw is not provably the last ask and a stamped answer must not
// be allowed to report a live question as answered. Fails toward OPEN.
func TestLoadSettlesNothingWhenTheQuestionFileWasNotReadToTheEnd(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","question":"keep the legacy route?","options":["keep","remove"]}`,
		// Over the scanner's per-line budget, so the scan stops here and the
		// rest of the file is never seen.
		`{"id":"q2","question":"`+strings.Repeat("x", maxLineBytes)+`"}`,
	)
	write(t, dir, AnswersFile, `{"id":"q1","answer":"keep","ask_ordinal":1}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Open()) != 1 || conv.Open()[0].ID != "q1" {
		t.Fatalf("open = %+v, want q1 still open on a short scan", conv.Open())
	}
	if len(conv.SettledAsks()) != 0 {
		t.Fatalf("a short scan settled %d ask(s)", len(conv.SettledAsks()))
	}
	if !strings.Contains(strings.Join(conv.Notes, "\n"), "could not be read in full") {
		t.Fatalf("short scan not disclosed: %v", conv.Notes)
	}
}

// TestLoadSettlesNothingWhenTheLineCapDroppedAQuestion is the line bound's half
// of the same rule. The byte bound stops the scan, so the tail is unseen; the
// line cap keeps the NEWEST lines and drops a prefix, and Entries are built
// only from what is retained - so a question whose only line is in that prefix
// has no Entry, is missing from Open(), is never emitted as a finding, and is
// absent from the omission marker's id list too, because that list comes from
// Open(). Both release paths would then see nothing open and release the gate
// with a major question unanswered.
func TestLoadSettlesNothingWhenTheLineCapDroppedAQuestion(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	// The first question is pushed out of the retention window by the rest.
	lines = append(lines, `{"id":"q-oldest","kind":"question","question":"was the legacy route meant to go?","weight":"major"}`)
	for i := 0; i < maxLines; i++ {
		lines = append(lines, fmt.Sprintf(`{"id":"q%d","kind":"question","question":"later question %d","weight":"major"}`, i, i))
	}
	write(t, dir, QuestionsFile, lines...)
	write(t, dir, AnswersFile, `{"id":"q0","answer":"yes","ask_ordinal":1}`)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !conv.QuestionsIncomplete {
		t.Fatal("a question line dropped by the line cap left the history looking complete, so an answer could settle a question and release the gate with a dropped question still open")
	}
	if len(conv.SettledAsks()) != 0 {
		t.Fatalf("an incomplete question history settled %d ask(s)", len(conv.SettledAsks()))
	}
	if !strings.Contains(strings.Join(conv.Notes, "\n"), "could not be read in full") {
		t.Fatalf("the dropped question was not disclosed: %v", conv.Notes)
	}
}
