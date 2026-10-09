// Package reviewqa owns the review conversation's on-disk protocol: the
// questions a review turn emits while it works, and the answers an operator
// writes back.
//
// Two append-only newline-delimited JSON files live in the run's evidence
// directory. Append-only is the durability story: a crash mid-write loses at
// most the trailing line, a reader can tail either file while a writer is
// appending, and neither side needs a lock. The reviewer writes questions with
// the file tools it already has, which is why there is no MCP server here - one
// per run would be a process, a handshake and a per-adapter support matrix for
// a capability the agent already has.
//
// So this package deliberately writes ANSWERS only (AppendAnswer, for the
// daemon's answer handler) and never questions: the reviewer is the sole writer
// of questions.ndjson, and every field its prompt's worked example omits - kind
// and weight whenever the model trims the example - has to be absent-tolerant
// on the READ side. A Go writer here would have defaulted
// those fields and left the tolerant branches unexercised, so tests append the
// raw lines the agent actually emits instead.
//
// User-facing semantics are owned by
// docs/src/content/docs/concepts/review-conversation.md.
package reviewqa

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kinds a questions.ndjson line can carry.
const (
	KindQuestion = "question"
	KindRetract  = "retract"
)

// WeightMinor is the only weight this package reads. Only major questions are
// ever emitted: the reviewer decides minor ones itself (captain's ruling of
// 2026-09-15, routing by weight unchanged), so a minor line is a protocol
// violation and is reported rather than silently escalated. An absent weight is
// therefore treated as major, which is why there is no constant for it - one
// existed, was never referenced anywhere in the tree, and Go does not report an
// unused constant.
const WeightMinor = "minor"

// File names inside the conversation directory.
const (
	QuestionsFile = "questions.ndjson"
	AnswersFile   = "answers.ndjson"
)

// Bounds on what is read from either file. They exist because every loaded
// entry is rendered into an agent prompt and into `axi` output, so an agent
// that appends in a loop must degrade to a truncated conversation rather than
// an unbounded one.
//
// The two bounds behave oppositely and both matter. maxLines counts accepted
// lines and keeps the NEWEST of them, because a later line supersedes an
// earlier one with the same id. maxFileBytes stops the scan instead, so a file
// over that size keeps its LEADING bytes and a trailing retraction or answer
// may not be read at all.
const (
	maxLines     = 2000
	maxFileBytes = 4 << 20 // 4 MiB
	maxLineBytes = 64 << 10
)

// Question is one line of questions.ndjson. A KindRetract line carries only ID,
// Kind and Reason.
//
// The timestamps the protocol lets the reviewer write (asked_at on a question,
// at on a retraction) are deliberately absent here: nothing reads them, and the
// ordering that matters comes from the append order of the two files, not from
// a clock the reviewer controls. Unknown fields decode away, so a line that
// carries them still parses.
type Question struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
	Weight   string   `json:"weight,omitempty"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Area     string   `json:"area,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// Answer is one line of answers.ndjson.
//
// AskOrdinal is which ASK of that id this answer settles, 1-based, stamped by
// the writer with the id's ask count at the moment of the append. It is what
// tells a correction to an already-settled ask apart from an answer to a later
// re-ask of the same id: the two files are appended independently, so at load
// time two asks and two answers are otherwise indistinguishable between those
// two sequences, and reading them as the second let a genuinely different
// question arrive pre-answered. It is absent (0) only in an answer for an id
// nobody has asked, which settles nothing, ever.
type Answer struct {
	ID         string `json:"id"`
	Answer     string `json:"answer"`
	AnsweredBy string `json:"answered_by,omitempty"`
	AnsweredAt string `json:"answered_at,omitempty"`
	AskOrdinal int    `json:"ask_ordinal,omitempty"`
}

// Entry is a question resolved against every later line about the same id:
// the newest question wins, a retraction closes it, and the newest answer for
// it is attached.
type Entry struct {
	Question
	Answer    *Answer
	Retracted bool
}

// Open reports whether this entry still blocks the review step: a live
// question with no answer.
func (e Entry) Open() bool { return !e.Retracted && e.Answer == nil }

// Answered reports whether a live question has an answer.
func (e Entry) Answered() bool { return !e.Retracted && e.Answer != nil }

// Ask is ONE question line and the answer that settled it, paired by ordinal:
// the Nth time an id was asked is paired with the Nth answer for that id.
//
// It exists because Entry deliberately collapses to the LATEST state of an id -
// which is what the gate needs - while the durable answer store must keep every
// decision a human gave. An agent reuses an id (a cold rereview in a fix round
// is shown only the still-open questions, so it starts numbering at q1 again),
// so without per-ask pairing the second q1's answer overwrote the first's and a
// human's recorded decision vanished from the do-not-re-raise set.
type Ask struct {
	// Ordinal is 1-based: the Nth time this id was asked in this conversation.
	Ordinal  int
	Question Question
	Answer   *Answer
}

// Conversation is one run's questions in the order they were first asked.
type Conversation struct {
	Entries []Entry
	// Notes carries bounded, operator-readable reasons a line was dropped:
	// malformed JSON, a missing id, a minor-weight question, an answer for an
	// id nobody asked. Never an error - a half-written trailing line is
	// expected while the reviewer is still appending.
	Notes []string
	// Asks is every accepted question LINE in file order, each paired with the
	// answer that settled it. Entries collapse an id to its latest state; Asks
	// does not, which is what lets the durable store keep both decisions when
	// an id is re-asked.
	Asks []Ask
	// QuestionsIncomplete reports that the scan never reached the end of
	// questions.ndjson, so a later ask of any id is unknowable and nothing in
	// this load is settled. The reader fails toward OPEN on it; the WRITER
	// refuses on it, because an answer stamped against a history that is not
	// all there could never close its question and would park the gate
	// forever with the operator told they had answered it.
	QuestionsIncomplete bool
}

// SettledAsks returns every (question, answer) pair this conversation has
// settled, oldest first, including earlier asks of an id that was later
// re-asked. Entry-based accessors report only the latest state of each id.
//
// A RETRACTED ask is never settled, whatever answer landed on it. The reviewer
// withdraws a question it has answered for itself, and an operator who saw that
// question before the retraction can still answer it - the window is the whole
// time the reviewer keeps working. That answer stays recorded on disk, like any
// orphan or duplicate, but it must not become a durable branch decision: the
// only consumer of this list writes review_questions, whose rows reach every
// later reviewer as questions not to re-raise and are never deleted.
//
// Only the LAST ask of a retracted id is skipped, because that is the ask the
// retraction closed. An earlier ask of the same id was a different question,
// and a human's decision on it would otherwise vanish - an id asked, answered,
// re-asked and then retracted inside one turn is recorded once, at the end of
// that turn.
func (c Conversation) SettledAsks() []Ask {
	retracted := make(map[string]bool, len(c.Entries))
	for _, e := range c.Entries {
		if e.Retracted {
			retracted[e.ID] = true
		}
	}
	lastAsk := make(map[string]int, len(c.Asks))
	for _, a := range c.Asks {
		if a.Ordinal > lastAsk[a.Question.ID] {
			lastAsk[a.Question.ID] = a.Ordinal
		}
	}
	out := make([]Ask, 0, len(c.Asks))
	for _, a := range c.Asks {
		if a.Answer == nil {
			continue
		}
		if retracted[a.Question.ID] && a.Ordinal == lastAsk[a.Question.ID] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Open returns the entries that still block the step.
func (c Conversation) Open() []Entry { return c.filter(Entry.Open) }

// Answered returns the live, answered entries.
func (c Conversation) Answered() []Entry { return c.filter(Entry.Answered) }

// Withdrawn returns the retracted entries.
func (c Conversation) Withdrawn() []Entry {
	return c.filter(func(e Entry) bool { return e.Retracted })
}

func (c Conversation) filter(keep func(Entry) bool) []Entry {
	var out []Entry
	for _, e := range c.Entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// DirName is the conversation directory's name inside the run's evidence
// directory. It is exported because the conversation shares that directory
// with the run's TEST evidence, which is publishable: the PR step excludes this
// name from the evidence-branch walk, and naming it here keeps the exclusion
// and the location from drifting apart.
const DirName = "review"

// Dir is the review conversation directory for a run, given that run's
// evidence directory. Empty in, empty out: an embedding with no evidence
// directory has no conversation, and callers treat that as "no questions".
func Dir(evidenceDir string) string {
	if strings.TrimSpace(evidenceDir) == "" {
		return ""
	}
	return filepath.Join(evidenceDir, DirName)
}

// Load reads a conversation directory. A missing directory or a missing file
// is an empty conversation, not an error: the reviewer creates the files only
// when it has something to say.
func Load(dir string) (Conversation, error) {
	var conv Conversation
	if strings.TrimSpace(dir) == "" {
		return conv, nil
	}

	questionLines, droppedQuestionLine, questionsIncomplete, err := readLines(filepath.Join(dir, QuestionsFile))
	// Both read bounds have to fail the same way, and they did not. The byte
	// bound stops the scan, so the tail is unseen and nothing settles. The line
	// cap instead KEEPS the newest lines and drops a prefix - and Entries are
	// built from the retained lines only, so a question whose only line is in
	// that prefix has no Entry at all: it is missing from Open(), no
	// "question-<id>" finding is emitted for it, and it is absent from the
	// omission marker's id list too, because that list is built from Open().
	// Both release paths would then see nothing open and release the gate with
	// a major question unanswered, with a later answer for it recorded as an
	// orphan. Reaching it needs more than maxLines accepted question lines in
	// one run, which is exactly the runaway-appending reviewer the bound exists
	// for.
	//
	// So a dropped question line is an incomplete question history too, and
	// from here on the two are one condition. A dropped ANSWER line is not the
	// same and deliberately does not set it: an answer that scrolled out of the
	// window cannot settle anything either way, and the ask it belonged to
	// simply stays open, which is the safe direction.
	questionsIncomplete = questionsIncomplete || droppedQuestionLine
	if err != nil {
		return conv, err
	}
	answerLines, droppedAnswerLine, answersIncomplete, err := readLines(filepath.Join(dir, AnswersFile))
	if err != nil {
		return conv, err
	}

	order := make([]string, 0, len(questionLines))
	byID := make(map[string]*Entry, len(questionLines))
	// An id can be ASKED more than once: ids are chosen by the agent (the
	// protocol's worked example is literally "q1"), the conversation directory
	// is per RUN, and a cold rereview in a fix round is shown only the OPEN
	// questions - so it reuses "q1" for a genuinely different question. An
	// answer written before that re-ask answered the OLD question, and letting
	// it settle the new one meant the new question arrived pre-answered:
	// Open() was empty, no finding was emitted, the gate never parked, and a
	// major question reached nobody.
	//
	// So an ask is settled only by an answer stamped with ITS AskOrdinal, and
	// nothing else settles it. That is not a comparison of timestamps: the two
	// files are appended independently, asked_at and answered_at are optional
	// and written by whoever appends the line, and second-granularity RFC3339
	// from two writers cannot order a fast exchange. It fails toward OPEN - a
	// re-ask asks again rather than inheriting an answer written before it,
	// including a correction to the ask it supersedes - which is the safe
	// direction here and also the behaviour under a byte-truncated answers
	// file.
	asks := make(map[string]int, len(questionLines))
	// Every retained question line per id, in file order, so an earlier ask
	// survives a later one for the durable store's benefit.
	lines := make(map[string][]Question, len(questionLines))
	askOrder := make([]string, 0, len(questionLines))
	answersByID := make(map[string][]Answer, len(answerLines))
	for _, line := range questionLines {
		q, note, ok := classifyQuestionLine(line)
		switch {
		case note != "":
			conv.Notes = append(conv.Notes, note)
			continue
		case !ok:
			// A retraction: it carries no question text, so it is never an ask.
			if entry, exists := byID[q.ID]; exists {
				entry.Retracted = true
				entry.Reason = q.Reason
			} else {
				conv.Notes = append(conv.Notes, fmt.Sprintf("retraction for unknown question %q ignored", q.ID))
			}
			continue
		}
		asks[q.ID]++
		lines[q.ID] = append(lines[q.ID], q)
		askOrder = append(askOrder, q.ID)
		if entry, ok := byID[q.ID]; ok {
			// A later question line for the same id supersedes the earlier
			// one for THIS entry's state, and revives a retracted question,
			// because re-asking is how the reviewer says the retraction was
			// wrong. For settling it is a new ASK (counted above): it needs an
			// answer stamped with its own ordinal and never inherits one
			// written before it.
			entry.Question = q
			entry.Retracted = false
			continue
		}
		entry := &Entry{Question: q}
		byID[q.ID] = entry
		order = append(order, q.ID)
	}

	for _, line := range answerLines {
		var a Answer
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			conv.Notes = append(conv.Notes, "skipped a malformed answers.ndjson line")
			continue
		}
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" || strings.TrimSpace(a.Answer) == "" {
			conv.Notes = append(conv.Notes, "skipped an answers.ndjson line with no id or no answer")
			continue
		}
		if _, ok := byID[a.ID]; !ok {
			// The writer may be racing a question it has not read yet, or
			// answering something the reviewer withdrew. Recorded, ignored.
			conv.Notes = append(conv.Notes, fmt.Sprintf("answer for unknown question %q ignored", a.ID))
			continue
		}
		answersByID[a.ID] = append(answersByID[a.ID], a)
	}

	// Pair every ask with the answer that settled it, and let the LAST ask of
	// an id be the entry's state: the gate needs the latest, the durable store
	// needs them all, and one rule for both keeps them from disagreeing.
	//
	// A questions.ndjson the scan could not read to the end settles NOTHING:
	// the ask a stamped answer names may be past the seen region, and the last
	// line we saw for an id is not necessarily its last ask, so attaching
	// answers there could report a live question as answered. Fail toward OPEN.
	seen := make(map[string]int, len(lines))
	conv.Asks = make([]Ask, 0, len(askOrder))
	for _, id := range askOrder {
		seen[id]++
		ordinal := seen[id]
		ask := Ask{Ordinal: ordinal, Question: lines[id][seen[id]-1]}
		if !questionsIncomplete {
			ask.Answer = settlingAnswer(answersByID[id], ordinal)
		}
		conv.Asks = append(conv.Asks, ask)
		if ordinal == asks[id] {
			byID[id].Answer = ask.Answer
		}
	}

	conv.Entries = make([]Entry, 0, len(order))
	for _, id := range order {
		conv.Entries = append(conv.Entries, *byID[id])
	}
	if questionsIncomplete || answersIncomplete || droppedAnswerLine {
		conv.Notes = append(conv.Notes, "review conversation file exceeded its size bound; older lines were not read")
	}
	if questionsIncomplete {
		conv.QuestionsIncomplete = true
		conv.Notes = append(conv.Notes, "questions.ndjson could not be read in full; no answer settles a question until it can be")
	}
	return conv, nil
}

// classifyQuestionLine decides what one questions.ndjson line is, applying the
// acceptance rules in one place.
//
// A non-empty note is a stateless rejection, already phrased for the operator.
// Otherwise ok reports whether the line is an ASK; the one line that is neither
// is a retraction, whose effect depends on state this function does not have.
func classifyQuestionLine(line string) (Question, string, bool) {
	var q Question
	if err := json.Unmarshal([]byte(line), &q); err != nil {
		return q, "skipped a malformed questions.ndjson line", false
	}
	q.ID = strings.TrimSpace(q.ID)
	if q.ID == "" {
		return q, "skipped a questions.ndjson line with no id", false
	}
	switch strings.TrimSpace(q.Kind) {
	case KindRetract:
		q.Kind = KindRetract
		return q, "", false
	case KindQuestion, "":
		q.Kind = KindQuestion
	default:
		return q, fmt.Sprintf("skipped question %q with unknown kind %q", q.ID, q.Kind), false
	}
	if strings.TrimSpace(q.Question) == "" {
		return q, fmt.Sprintf("skipped question %q with no question text", q.ID), false
	}
	// Routing by weight is the reviewer's own job, so a minor question is
	// never escalated on its behalf: emitting one is the protocol violation,
	// and reporting it keeps that visible instead of parking the run on a
	// question the reviewer was told to decide itself.
	if strings.EqualFold(strings.TrimSpace(q.Weight), WeightMinor) {
		return q, fmt.Sprintf("dropped minor-weight question %q; the reviewer decides minor questions itself", q.ID), false
	}
	return q, "", true
}

// settlingAnswer returns the answer stamped for this ask of one id - the latest
// of them, so a correction to the same ask replaces - or nil while that ask is
// still open.
//
// An UNSTAMPED answer settles nothing. It used to pair positionally, for "a
// file written before ask_ordinal existed", but no such file can exist:
// answers.ndjson arrives with the field and the daemon is its only writer. The
// one thing that branch really reached was the orphan - an answer for an id
// nobody has asked yet, which is written unstamped because there is no ask to
// stamp - and there it settled the first LATER ask of that id, pre-answering a
// genuinely different question, which is the defect the stamp exists to close.
// An orphan stays recorded and inert instead.
func settlingAnswer(answers []Answer, ordinal int) *Answer {
	var settling *Answer
	for _, a := range answers {
		if a.AskOrdinal == ordinal {
			settling = &a
		}
	}
	return settling
}

// AppendAnswer appends one answer, creating the directory on first use.
func AppendAnswer(dir string, a Answer) error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("review answer requires a question id")
	}
	if strings.TrimSpace(a.Answer) == "" {
		return errors.New("review answer requires answer text")
	}
	if strings.TrimSpace(a.AnsweredAt) == "" {
		a.AnsweredAt = time.Now().UTC().Format(time.RFC3339)
	}
	return appendLine(dir, AnswersFile, a)
}

func appendLine(dir, name string, payload any) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("review conversation directory is not set")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode review conversation line: %w", err)
	}
	if len(encoded)+1 > maxLineBytes {
		return fmt.Errorf("review conversation line is %d bytes, over the %d byte limit", len(encoded), maxLineBytes)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create review conversation dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("append %s: %w", name, err)
	}
	return nil
}

// readLines returns the non-empty lines of an ndjson file, newest-last and
// bounded. A missing file is no lines and no error.
//
// It reports its two bounds separately because the caller treats them
// differently per file. dropped says the maxLines cap removed a leading prefix;
// incomplete says the scan never reached the end of the file (the maxFileBytes
// cut, or a scanner error). For questions.ndjson both mean the same thing - the
// history cannot be read in full, so nothing settles - while a dropped ANSWER
// line only leaves its ask open, which is already the safe direction.
func readLines(path string) (kept []string, dropped, incomplete bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, false, nil
		}
		return nil, false, false, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	var lines []string
	read := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		read += len(scanner.Bytes()) + 1
		if read > maxFileBytes {
			incomplete = true
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		// A line over the scanner budget, or a torn read while the reviewer is
		// still appending. Keep what parsed rather than losing the file.
		incomplete = true
	}
	if len(lines) > maxLines {
		dropped = true
		lines = lines[len(lines)-maxLines:]
	}
	return lines, dropped, incomplete, nil
}
