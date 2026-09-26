---
title: The Review Conversation
description: How the reviewer asks questions while it works, how answers reach it, and what the pipeline persists.
---

The review step used to be a monologue. One agent turn read the diff, returned
every finding at the end, and any finding it could not decide became an
`ask-user` finding that parked the run until a human answered it. The human's
answer then arrived as a gate response - approve, fix, or skip - which is a
verdict on the whole round, not an answer to the question that was asked.

The review conversation is **opt-in and off by default**. A repository asks for
it with trusted
[`review.conversation: true`](/no-mistakes/reference/repo-config/#reviewconversation)
on its default branch; without that, everything on this page is inert and the
review step behaves exactly as the paragraph above describes. The setting is
read only from the trusted default-branch copy, in both directions: a pushed
branch cannot make its own review park for a human answer, and it cannot
decline a conversation the maintainer asked for.

With it on, the review conversation replaces the monologue with a two-way
channel that runs *while* the review runs:

- the reviewer emits each substantiated question the moment it has one, instead
  of holding it to the end of the turn;
- it keeps reviewing other areas while a question is open;
- it re-reads answers at its own checkpoints and adjusts;
- when it has reviewed everything it can and every question is emitted, the
  turn ends and the step parks, so nothing idles and no timeout burns;
- an answer wakes the *same* reviewer session to finalize, so nothing is
  re-read from scratch;
- once code changes, a fresh cold reviewer reads the result.

The independence guarantee `internal/pipeline/steps/review.go` documents is
unchanged: a reviewer session never spans a code change, so a reviewer never
certifies its own prescription.

## The file protocol

Every run owns a review-conversation directory under the run's evidence
directory (`pipeline.StepContext.EvidenceDir`, resolved once by the executor and
always outside the worktree):

```
<evidence-root>/<run-id>/review/
    questions.ndjson    # append-only, written by the reviewer
    answers.ndjson      # append-only, written by the operator
```

Writing there is an explicit exception to the workspace boundary every agent
prompt carries, and the reviewer's protocol section says so in as many words.
The directory is the run's own managed area rather than the project's, so the
boundary's out-of-worktree rule does not reach it - and a reviewer that resolved
the two instructions the other way would create nothing, which reads exactly
like having had no question to ask.

Both files are newline-delimited JSON. Append-only is the whole durability
story: a crash mid-write loses at most the trailing line, a reader can `tail -f`
either file, and no writer ever needs a lock on something another process is
reading. `internal/reviewqa` owns the shape and is the only parser.

Only `id` and `question` are load-bearing on a question line. The reviewer is
the sole writer of `questions.ndjson` - it appends with its own file tools, and
there is no Go writer for that file - so every other field is optional on read:
`kind` defaults to `question` when absent, an absent `weight` is treated as
`major` (only an explicit `minor` is dropped), and `asked_at` is never read at
all, since an ask is settled only by an answer carrying its own `ask_ordinal`
rather than by comparing timestamps. A line that omits them is the normal case,
not a degraded one.

The conversation stays LOCAL to the run. The files live in the run's evidence
directory, but the conversation directory is excluded from the evidence-branch
publication walk, so opting into
[`test.evidence.store_in_repo`](/no-mistakes/reference/repo-config/) publishes
the run's test evidence and never the conversation. The only published copy is
the bounded rendering in the PR body, which goes through the home-path redaction
every published body does. Publishing the raw files instead would put the full
question text, the full answer text and who answered on an orphan branch
verbatim and permanently, with neither of those protections.

### questions.ndjson

The reviewer appends one line per event, using the file tools it already has. No
MCP server is involved: an MCP server per run would be a process, a handshake
and a per-adapter support matrix for a capability the agent already has.

```json
{"id":"q1","kind":"question","question":"Should the legacy /v1 route keep answering after this change?","options":["Keep answering","Remove it","Keep behind a flag"],"weight":"major","file":"internal/api/router.go","line":88,"area":"routing","asked_at":"2026-09-15T13:04:11Z"}
{"id":"q1","kind":"retract","reason":"answered by the migration note in docs/api.md","at":"2026-09-15T13:19:02Z"}
```

- `id` is the reviewer's own handle for the question. A later line with the same
  `id` supersedes the earlier one for that question's state, and revives it if it
  had been retracted - but it is a new **ask**, not an edit of the answered one.

  An `id` is chosen by the reviewer and is only unique by accident - every
  review turn of a run appends to the same file, and a cold rereview in a fix
  round is shown only the questions still open, so it can reuse `q1` for a
  genuinely different question. So each **ask** is settled only by an answer of
  its own: an answer carries the `ask_ordinal` it settles (see below), and a
  re-ask re-opens the entry and parks the gate again rather than inheriting an
  answer written before it. An answer with no `ask_ordinal` - which only an
  answer for an `id` nobody asked can be - settles nothing, ever. That rule
  compares no timestamps, because the two files are appended
  independently and `asked_at`/`answered_at` are optional. The same reason makes
  the per-branch answer store keyed by run as well as by question id, so two
  runs that both use `q1` keep their own settled decisions instead of one
  overwriting the other's.
- `kind` is `question` or `retract`. A retracted question is closed: it never
  blocks the step and never needs an answer. Re-asking it revives it, and it
  comes back open rather than carrying whatever answer preceded the retraction.
- `options` carries the multiple-choice alternatives. The reviewer is required
  to supply 2-4 of them, because a question reaches the captain in the same
  multiple-choice form he already receives - an open-ended question is a worse
  question, not a shorter one. Nothing validates that, though: a question that
  arrives with no options is still accepted and still parks the gate, it just
  reaches the operator open-ended.
- `weight` is `major` (escalate) - see [Routing by weight](#routing-by-weight).
  Minor questions are never emitted at all.

### answers.ndjson

```json
{"id":"q1","answer":"Keep behind a flag","answered_by":"captain","answered_at":"2026-09-15T13:31:40Z","ask_ordinal":1}
```

Written by the daemon, and only by the daemon: `no-mistakes axi answer` reaches
it through `RunManager.HandleAnswerReviewQuestion`, which is the sole writer of
this file. It has to be, because only it can stamp `ask_ordinal`, and an
unstamped line settles nothing at all. An answer whose conversation cannot be
read is refused rather than written unstamped, so no question is left parked
with its operator told they had answered it.

`ask_ordinal` is which ask of that `id` the answer settles, 1-based, stamped by
the daemon with the `id`'s ask count at the moment of the append. The
last line for the same `id` and `ask_ordinal` wins, so a correction is another
append - and it corrects the ask it was written for, so it never pre-answers a
later re-ask of that `id`. That binding has to come from the writer: at read
time two asks and two answers look identical whether the second answer corrects
the first ask or answers the re-ask, and that ambiguity is the whole defect.

The ask ordinal is counted over the question lines the load retained, and
retention can no longer shift it: dropping a question line makes the whole
question history incomplete, so nothing in that load settles and no answer is
ever bound to a renumbered ask.

Both of the reader's bounds then fail the same way, and this is the rule that
matters most in the file: if `questions.ndjson` could not be read IN FULL,
nothing in that load settles and every question stays open. The byte bound and
an unreadable line stop the scan, so a later ask of an `id` is unknowable. The
line bound drops a leading prefix instead, and that is no safer: entries are
built from the retained lines, so a question whose only line is in the dropped
prefix has no entry at all - it is missing from the open set, no finding is
emitted for it, and it is absent even from the omission notice's id list,
because that list is built from the open set. Treated as complete, such a load
would report nothing open and release the gate with a major question
unanswered, with a later answer for it recorded as an orphan. Reaching it takes
more than 2000 accepted question lines in one run, which is precisely the
runaway-appending reviewer the bound exists for.

The review step has its own rule for that condition, and it replaces the
question channel rather than adding to it: when the question history could not
be read in full, the step emits ONE `ask-user` warning
(`review-questions-unreadable`) instead of any `question-<id>` row, whatever
remains open. Every one of those rows would end in "Answer it with:
`no-mistakes axi answer --question <id>`", and the answer path refuses every
answer for such a conversation, so each row would instruct a command guaranteed
to fail. The warning names the ids and count of the questions that were still
open in the part of the history that was read, and points at the run's
`questions.ndjson` as the full record, so the loss is bounded rather than
silent. It deliberately carries no review-question category: that category is
what resumes the reviewer once nothing is open, and nothing may resume a
reviewer off a history it could not read. Both automatic resolvers - `axi
--yes` and the TUI's yolo - instead stand aside on it by finding ID, because a
fixer handed "decide this gate yourself" can only edit code and converge on an
approve; a human's own approve, fix or skip stays allowed. For the same
no-file reason it is also dropped from the review carry-forward's verification
input, exactly as an open question is: it anchors to no path, so leaving it in
would refuse to clear every genuinely fixed finding for as long as the
append-only history stayed incomplete.

A dropped ANSWER line is deliberately not the same: an answer that scrolled out
of the window cannot settle anything either way, and the ask it belonged to
simply stays open, which is the safe direction - and an answer may still be
appended for it.

Where the QUESTION history could not be read to the end, the answer path
refuses rather than appending: an answer stamped against a history that is not
all there could never close its question, so recording it would leave the gate
parked forever with its operator told they had answered. The refusal names that
cause, distinctly from a conversation that cannot be read at all, and writes
nothing.

A read that FAILS is not an empty conversation either: the review step stops,
exactly as the answer path refuses, because a swallowed failure would complete a
review with no open question rather than parking on the ones that were asked.

An answer for an unknown or retracted `id` is recorded and ignored, never an
error: the writer may be racing a retraction it has not read yet. Ignored is
permanent in both cases. An unknown `id` carries no `ask_ordinal`, so a later
ask of that `id` is a different question and stays open; a retracted ask is
never settled by any answer, so a question the reviewer withdrew is never
persisted as a branch decision. An earlier ask of a re-asked id that was later
retracted keeps its own settled answer.

## Routing by weight

Unchanged from today, by the captain's ruling of 2026-09-15: the reviewer
decides minor questions itself (pass or fail) and emits only the larger ones.
"Larger" is the reviewer's judgement, stated in the prompt as the existing
`ask-user` threshold - product behaviour, deliberate author intent, access
policy, or a remedy that would extend the change. A question the reviewer can
settle from the diff, the intent, the repository instructions or a recorded
decision is not a question; it is a finding or a pass.

## State machine

```
                        ┌──────────────────────────────────────┐
                        │ reviewing                            │
      turn starts ─────▶│ - emits questions as substantiated   │
                        │ - re-reads answers.ndjson at         │
                        │   its own checkpoints                │
                        └───────────────┬──────────────────────┘
                                        │ turn ends
                        ┌───────────────┴───────────────┐
               no open question                 open question(s)
                        │                               │
                        ▼                               ▼
            ┌───────────────────┐        ┌──────────────────────────────┐
            │ findings → gate   │        │ waiting-on-answers           │
            │ (today's review   │        │ step parked, no agent alive, │
            │  gate, unchanged) │        │ review_agent_timeout not     │
            └───────────────────┘        │ running, park accounted      │
                                         └──────────────┬───────────────┘
                                                        │ every open question answered
                                                        ▼
                                         ┌──────────────────────────────┐
                                         │ finalizing                   │
                                         │ SAME reviewer session        │
                                         │ resumed with the answers     │
                                         └──────────────┬───────────────┘
                                                        │
                                       ┌────────────────┴──────────────┐
                              no open question                  new question(s)
                                       │                               │
                                       ▼                               ▼
                           findings → gate            back to waiting-on-answers
                                       │
                           ┌───────────┴────────────┐
                    human approves            findings fixed
                           │                        │
                           ▼                        ▼
                    step completes       code changes → reviewer session
                                         dropped → next pass is COLD
```

`waiting-on-answers` is deliberately the pipeline's existing approval park, not
a new durable status. Each open question is carried as an `ask-user` finding
whose category is `review-question`, which means:

- `runs.awaiting_agent_since` is stamped and `runs.parked_ms` accrues, exactly
  as documented in `AGENTS.md` under **Parked / Awaiting-Agent Signal**;
- the TUI, the IPC event stream and `axi status` already surface the park;
- `review_agent_timeout` (30 m) cannot count the wait, because there is no
  agent turn in flight: the turn ended before the park, and the finalize turn
  is a fresh invocation with a fresh deadline from `reviewAgentContext`.

The findings payload rides the IPC event stream, so the gate renders at most 50
question rows. When more are open, one further notice reports the count and
names the remaining question ids: the gate releases only once every open
question is answered, and a question dropped from the rows is not re-emitted
later, so those ids are answered with `axi answer` exactly like the ones that
have a row of their own.

The reviewer therefore never idles. It idles only in the sense the captain
required - after it has reviewed everything it can and emitted every question -
and in that state no process is alive at all, so there is nothing to time out
and nothing to poll.

Reusing the approval park costs one thing that has to be paid for: the park is
released by a response, and there is a window in which no response can arrive.
The review step builds a finding for each open question and returns, and only
afterwards does the executor register the gate as waiting. An answer landing in
between is recorded on disk, but `axi answer` finds no gate to release and says
so - and the gate then parks on a snapshot that is already stale, with no
reviewer left to read the answer. So the parked review gate re-checks the
conversation on a timer (`pipeline.ApprovalGateResumer`, the same cadence as
[`gate_reconcile_interval`](/no-mistakes/reference/global-config/)) and, once
nothing is open, resumes the reviewer itself.

It resumes rather than completing, which is the distinction that interface
exists for: completing the step here would approve the run's head off the stale
snapshot without the reviewer ever seeing the answers. Three conditions must all
hold before it acts - the conversation is on, the parked gate really does carry
review-question findings, and nothing is open - so a review gate parked on
ordinary code findings is never answered out from under the operator, and a
repository that has not opted in sees no change at all.

### Notification is a push, not a poll

While the reviewer is *working*, it re-reads `answers.ndjson` itself at its own
checkpoints. That is not idling and not a wait: it is a file read interleaved
with work it was doing anyway, and it is what lets an early answer redirect the
pass before the effort is spent.

While the reviewer is *parked*, nothing polls. `axi answer` appends the answer
and, once no question is open, sends one `respond --action answer` to the
daemon. The executor then resumes the reviewer's own session with the answers as
its next message. From the reviewer's side an answer arrives as a message it did
not ask for - a push - and it never learns that time passed.

A live `--input-format stream-json` stdin channel to a held-open subprocess was
considered and rejected: a park lasts tens of minutes to hours, a daemon restart
would kill the held process, and the reviewer has no idle window that channel
could serve that the resume does not. The resume path already exists for the
fixer and survives a daemon restart, because the session id is persisted in
`run_agent_sessions`.

## Session lifecycle

| Turn | Session | Why |
| --- | --- | --- |
| Initial review pass | fresh `reviewer` session, persisted | the finalize turn must be able to resume it |
| Finalize after answers | resumes `reviewer` | conversational within a round; nothing re-read |
| Re-review after any code change | cold | independence: a reviewer must not certify its own prescription |
| In-run fix round (`sctx.Fixing`) | cold, and the `reviewer` session is dropped first | same reason |
| A restart back to review (a CI repair's `RestartFrom`) | fresh, the stored identity dropped first | it re-enters the step on a new head inside the same run, so the identity in hand reviewed the OLD head |

The table assumes the default [`session_reuse: true`](/no-mistakes/reference/global-config/#session_reuse). With it off no reviewer identity is persisted and the finalize turn runs cold like every other turn; it still receives the answers, because the finalize prompt is the whole review prompt plus them.

`RunSessions` is keyed by `(run, role)`, so an author push that supersedes the
run starts a new run and therefore a cold reviewer with no extra work. Within a
run the rule is stated as a single narrow permission rather than a list of
exclusions: a stored reviewer identity may be resumed by the finalize turn of
the pass that created it, and by nothing else. Every other entry into the
review step calls `Forget(SessionRoleReviewer)` first, which deletes the
persisted row too, so the rule holds across a daemon restart. That covers the
fix round and the restart-back-to-review above without either needing its own
special case.

An answer settles only the question it answers. The finalize prompt says so
explicitly: an answer of "that is intended" closes the question it names and
gives the reviewer no licence to soften a finding it did not ask about.

### The finalize turn re-adjudicates what it carried in

That matters for the outstanding finding set. A review finding stays outstanding
until a later round positively verifies it under the
[Review carry-forward rule](/no-mistakes/reference/pipeline-steps/#review),
and a fix round earns that right for the findings it dispatched to the fixer.

An answer round earns no pending-verification entries of its own, and the set it
carries in is not given any. The one exception is inherited rather than earned
here: when the question was asked by a rereview INSIDE a fix round, that fix
round's dispatched findings already earned their entries and keep them, so the
finalize turn is still their verification rereview under the ordinary coverage
rule. For every finding the answer round merely carries, silence keeps it.

A fix CHANGES the code, so a rereview that names the file and no longer reports
the defect is evidence the change worked. An answer changes nothing but what the
reviewer knows, so the same silence proves nothing about any particular finding:
a finalize turn that covered a file used to take every carried finding in it,
including ones the answers had no bearing on, and the gate could complete having
silently dropped a defect nobody fixed, selected or approved.

So an answer round retracts by NAMING. The carried set rides the prompt, the
turn walks it item by item with the answers in hand, and each finding either
appears again in `findings` because it still holds, or appears in
`withdrawn_findings` with the reason the answers disproved it. Anything the turn
leaves out of both is KEPT. Silence never retracts a finding.

This is what the protocol's `PENDING ANSWER (<id>)` prefix needs. Such a finding
is routine in a round that asks a question, and an answer frequently disproves
it; without a retraction channel the carry-forward re-injects it anyway, still
pointing the operator at a question that is already settled, with no way to
clear it but approving over it.

The coverage rule itself is untouched for every other round type, and a
withdrawal is a claim the reviewer has to make in its own words, which is
reviewable after the fact in a way silence never was. Only a finalize turn may
retract, and two layers keep it that way: the `withdrawn_findings` property is
declared only for a finalize turn with the conversation on, so an initial review
or a fix-round rereview is never offered it at all, and the executor applies a
retraction list only when the round it came from is a finalize turn. Those
rounds are held to the coverage rule, and a retraction they claimed would clear
a selected finding nothing positively verified.

A retraction also has a floor. An operator-authored finding - one the operator
added with `respond --action fix --add-finding` - is never retractable, whatever
the turn names: it is not a claim the reviewer made but an instruction the
operator gave, so it leaves the outstanding set only on positive coverage or on
the operator's own approve, skip or abort. The carried-findings prompt marks
those rows `[operator]` so the turn does not spend a round trying. Every
retraction that is applied is written to the step log with the reason the turn
gave, and recorded on the round, so a finding that disappears between rounds can
be read back against the claim that removed it.

### Turning the setting off does not strand a question already asked

`review.conversation` answers two questions, and they are keyed differently.

*May the reviewer ASK?* is keyed on the setting alone. With it off the review
prompt carries no question protocol, no settled-questions section, no files are
created, and the PR body publishes what it published before the feature existed.
A repository that never opted in is byte-for-byte upstream.

*May a conversation that already EXISTS be read and answered?* is keyed on the
files being on disk. The setting is trusted-default-branch-only and is
re-resolved on recovery from the current default-branch tip, so a maintainer who
turns it off - or a trusted-config fetch that fails, which recovery resolves the
same way - between the ask and the answer would otherwise make those questions
permanently unanswerable.

The two cases are distinguishable without ambiguity: a repository that never
enabled the conversation cannot have a questions file, so the read side is off
for it too. A file can only exist because an enabled turn wrote it.

Both halves of the read side move together, and that is the point. The answer
path accepts the answer, and the review step reads the same conversation for its
finalize turn. Opening only the write side was tried and is worse than refusing:
the answer lands on disk, the gate is released, and the finalize turn runs a
plain review that never sees it - while the CLI reports that the reviewer resumed
with the answer.

Emitting a question finding stays on the ask side, because that is what PARKS the
step: a repository that has turned the conversation off must not have a fresh
review inherit questions an earlier run asked.

## What is persisted, and where the next cold reviewer reads it

A mid-turn answer is not a gate response, so it cannot ride the existing
`step_rounds` decision channel. The review step mirrors each answered question
into `review_questions` when it finalizes: repository, branch, run, question id,
ask ordinal, question text, options, answer, who answered, and timestamps.
Nothing deletes those rows, for the same reason nothing deletes a branch
decision - an answer a human gave about this branch keeps standing.

The ask ordinal is part of the key, and it is what makes that promise true. A
question id belongs to the agent and is unique only by accident: a cold rereview
in a fix round is shown only the still-open questions, so it starts numbering at
`q1` again for a genuinely different question, and the reader treats that as a
re-ask rather than a correction. One row per settled ASK therefore keeps both
decisions; a correction to the same ask still replaces. Keyed by id alone within
a run, answering the second `q1` would have overwritten the first's row, and the
earlier decision would have disappeared from the section below and from the PR
body - silently.

The ask ordinal arrived with the table, so every installation's database has it.
A *development* database created from an earlier commit of this feature's branch
does not, and there this store cannot work at all: every read and every write
names the column, so each one fails and says so at ERROR level. The remedy is to
drop `review_questions` by hand (`sqlite3 <db> 'DROP TABLE review_questions'`)
and let it be recreated. There is deliberately no migration - SQLite cannot
ALTER a column into a primary key, and the migration list is re-run with its
errors tolerated on every start, which a create/copy/drop/rename could not
survive. Nothing else in the daemon is affected by such a table: an opt-in
review feature must not be able to stop the daemon starting, or block custody
recovery, for a repository that does not use it.

Every later review turn on the same branch, in any run, receives them as a
**Settled questions (do not re-raise)** prompt section, rendered separately from
the acceptance criteria and from the branch-decision section. That separation is
the point: an acceptance criterion is something the change must satisfy, while a
settled question is something the reviewer must stop asking.

`internal/db.GetBranchReviewAnswers` is the reader; the section is bounded by the
same line/byte budget as the other decision channels
(`internal/pipeline/steps/round_history.go`).

## Round history across a supersede

With the coding agent applying the fixes, a worker push supersedes the parked run
and a new run starts. The superseded run's per-round fix summaries would be lost:
`stepRoundHistorySection` is scoped to one step result, and
`uncertifiedRoundHistoryPromptSection` covers only *pipeline-authored* commits a
previous run left uncertified.

The initial review of a run therefore also receives the most recent *other*
run's review rounds on the same branch - the selector is deliberately
unfiltered by that run's status, so it renders for an ordinary second push onto
a branch whose previous run completed as well as for a supersede - as a
**Previous run's review rounds** section. It deliberately carries no fix-round provenance clause, and it does not
characterise the authorship of those commits in either direction. The
adversarial framing is only ever *added*, by `fixRoundProvenanceClause`, which
returns the empty string when neither `sctx.Fixing` nor an uncertified range
applies - so nothing in the prompt applies that standard by default and this
section has nothing to correct. Claiming the commits are the author's own would
be worse than silence: the selector is unfiltered by run status on purpose, so
the previous run may well have taken a pipeline fix round that completed, which
certifies its range and leaves `UncertifiedSourceRunID` empty, so the skip in
`BindPreviousRunReviewRounds` does not fire and the fixer's commits are inside
this run's own `base..head` scope.

## No round cap

There is none, and none may be added. The captain's ruling of 2026-09-15:
"there is no round cap we have introduced here". The pipeline enforces no limit
on review fix rounds - neither user-driven nor answer-driven - and there is no
`review.max_fix_rounds` setting. A worker-side convention about filing
follow-ups after a couple of rounds is a convention; it is not a pipeline limit
and must not become one.

## The PR body

The PR body records the conversation alongside the existing decision and
deferred lists: each question asked, its answer, and who answered it. A
retracted question is listed as withdrawn.

A question can also be published as **unanswered**. The review step never
completes on its own while one is open, but a human may approve the gate over
it, and that is the case a reader of the PR most needs to see - so it is listed
explicitly rather than quietly omitted, and it is listed before the withdrawn
questions so a length bound cannot be what drops it.

## What is unchanged

- A full review pass completes before any question blocks anything. The
  reviewer does not stop at its first question.
- Test runs after review, document and lint as today.
- Attestation semantics, the tests-kept gate and the checks-green gate are
  untouched.
- The in-run fixer path still exists and still works; it is simply no longer
  the default route for review findings.
