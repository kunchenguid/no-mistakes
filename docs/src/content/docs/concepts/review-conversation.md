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
all, since a re-ask is detected by counting asks against answers rather than by
comparing timestamps. A line that omits them is the normal case, not a
degraded one.

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

The count the stamp is taken against is every accepted question line for that
`id` in the file, so which lines the reader retains never shifts it: the reader
counts the leading lines its line bound discards rather than renumbering the
survivors.

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

A dropped ANSWER line is deliberately not the same: an answer that scrolled out
of the window cannot settle anything either way, and the ask it belonged to
simply stays open, which is the safe direction. The answer
path refuses there rather than appending: an answer stamped against a history
that is not all there could never close its question, so recording it would
leave the gate parked forever with its operator told they had answered. The
refusal names that cause, distinctly from a conversation that cannot be read at
all, and writes nothing. A read that
FAILS is not an empty conversation either: the review step stops, exactly as the
answer path refuses, because a swallowed failure would complete a review with no
open question rather than parking on the ones that were asked.

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

### The finalize turn is a full review pass

That matters for the outstanding finding set. A review finding stays outstanding
until a later round positively verifies it - covers its file in `reviewed_paths`
and stops reporting it - and a fix round earns that right for the findings it
dispatched to the fixer. An answer round dispatches nothing, so the findings it
carries in are what it may verify.

This is deliberate, and it rests on what a finalize turn actually is: its prompt
is the whole review prompt plus the answers, over the same head, so it is a
review pass and not a narrow re-read of one question. Its silence about a
finding is worth exactly what any rereview's silence is worth, and the coverage
rule still applies unchanged - a finding leaves only if that turn named its file
and did not report it.

The reason it must be allowed at all is the protocol's own instruction to prefix
a finding contingent on an open question with `PENDING ANSWER (<id>)`. Such a
finding is routine in a round that asks a question, and an answer frequently
disproves it. Without this the reviewer could withdraw it and the carry-forward
would re-inject it anyway, still pointing the operator at a question that is
already settled, leaving no way to clear it but approving over it.

The cost is stated rather than hidden: an answer round can also clear an
unrelated finding that the same turn covered and did not re-report. For that to
lose something real, the reviewer has to re-read unchanged code, name the file,
and decline to report a defect that is still there - a reviewer contradicting
itself rather than an answer overriding a verdict.

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

The initial review of a run therefore also receives the most recent superseded
run's review rounds on the same branch, as a **Previous run's review rounds**
section. It is labelled as author-fixed and deliberately carries no fix-round
provenance clause: the code under review is the author's, reviewed under the
ordinary standard, not pipeline-authored code needing the adversarial framing.

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
