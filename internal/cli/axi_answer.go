package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

// newAxiAnswerCmd answers one question the run's reviewer asked while it
// worked.
//
// It is deliberately a separate verb from `respond`. `respond` is a verdict on
// a round - approve, fix, skip - and its actions are the operator's. An answer
// is neither: it settles one question the reviewer itself raised, no code
// changes, and the round's findings are not being accepted or declined. Folding
// it into `respond --action answer` would let an operator release a review gate
// while questions were still open, which is exactly what this command refuses
// to do: the daemon releases the gate only when nothing is left open, and does
// it by resuming the reviewer's own session.
//
// The run is always the caller's current branch's active run. There is no
// --run: axi's run resolution is branch-scoped by design (see the AXI Run
// Resolution section in AGENTS.md), and this is a MUTATING command, so a second
// selection path is exactly what that scoping exists to prevent - it would let
// an answer meant for one branch's reviewer land on another's. The cost is
// accepted and known: the command must run from a clone of the repository whose
// run it answers, since the repository itself is resolved from the working
// directory.
func newAxiAnswerCmd() *cobra.Command {
	var questionID, answer, answeredBy string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "answer",
		Short: "Answer a question the reviewer asked while reviewing",
		Long: "Records an answer to one question the run's reviewer asked. Questions\n" +
			"appear in `no-mistakes axi status` as the review gate's `question-<id>`\n" +
			"findings, each carrying its id and the options the reviewer stated. When\n" +
			"more questions are open than the gate renders as rows, the gate's\n" +
			"omission notice names the remaining ids.\n\n" +
			"This is not a gate response. The answer is appended to the run's review\n" +
			"conversation immediately, so a reviewer that is still working reads it at\n" +
			"its next checkpoint and can redirect the rest of its pass. Once no\n" +
			"question is left open, the daemon resumes that same reviewer session with\n" +
			"the answers so it can finish - you do not approve or fix to release it.\n\n" +
			"Answer with one of the question's stated options wherever you can; the\n" +
			"reviewer wrote them so the answer would be unambiguous.\n\n" +
			"The answer that closes the last open question sets the run moving again,\n" +
			"so, exactly like `axi respond`, it then blocks until the next gate,\n" +
			"CI-ready decision point, or final outcome, bounded by --wait (default 8m).\n" +
			"Any other answer returns at once.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-answer", "/axi/answer", nil, func() error {
				return runAxiAnswer(cmd, answerArgs{
					questionID: strings.TrimSpace(questionID),
					answer:     strings.TrimSpace(answer),
					answeredBy: strings.TrimSpace(answeredBy),
					wait:       wait,
				})
			})
		},
	}
	cmd.Flags().StringVar(&questionID, "question", "", "question id, as carried by the review gate's `question-<id>` findings or named by the gate's omission notice (required)")
	cmd.Flags().StringVar(&answer, "answer", "", "the answer, ideally one of the question's stated options (required)")
	cmd.Flags().StringVar(&answeredBy, "by", "", "who answered, recorded on the PR and in the branch's settled questions")
	bindAxiWaitFlag(cmd, &wait)
	return cmd
}

type answerArgs struct {
	questionID string
	answer     string
	answeredBy string
	wait       time.Duration
}

func runAxiAnswer(cmd *cobra.Command, aa answerArgs) error {
	if aa.questionID == "" || aa.answer == "" {
		return emitError(cmd, 2, "--question and --answer are both required",
			`Run `+"`no-mistakes axi status`"+` to list the reviewer's open questions and their ids`)
	}
	if err := validateAxiWait(aa.wait); err != nil {
		return emitError(cmd, 2, err.Error(), "Pass a positive duration such as --wait 8m")
	}

	ctx := cmd.Context()
	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}
	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
	}
	if active.Run == nil {
		return emitError(cmd, 1, "no active run to answer",
			"A review question can only be answered while its run is still active; run `no-mistakes axi status` to check")
	}
	runID := active.Run.ID
	// The park this answer releases is identified before the answer is sent:
	// the reviewer can resume and park its finalize turn at the same status
	// before any post-answer read lands, and waiting on that new park would
	// hold the caller until --wait elapsed instead of returning it.
	var preAnswer *gateIdentity
	if gate, ok := reviewGate(active.Run); ok {
		id := gate.identity()
		preAnswer = &id
	}

	var result ipc.AnswerReviewQuestionResult
	if err := env.client.Call(ipc.MethodAnswerReview, &ipc.AnswerReviewQuestionParams{
		RunID:      runID,
		QuestionID: aa.questionID,
		Answer:     aa.answer,
		AnsweredBy: aa.answeredBy,
	}, &result); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("answer review question: %v", err))
	}

	fields := []toon.Field{
		{Key: "answered", Value: result.OK},
		{Key: "run", Value: runID},
		{Key: "question", Value: aa.questionID},
		{Key: "open_questions", Value: result.Open},
	}
	if len(result.OpenIDs) > 0 {
		fields = append(fields, toon.Field{Key: "open_question_ids", Value: result.OpenIDs})
	}
	fields = append(fields, toon.Field{Key: "reviewer_resumed", Value: result.Resumed})
	if result.Note != "" {
		fields = append(fields, toon.Field{Key: "detail", Value: result.Note})
	}
	if result.ClosedLast || result.Resumed {
		// This answer set the run moving, so follow it to the next decision
		// point exactly as respond does. Returning here instead left a driving
		// agent with neither a gate to answer nor an outcome to report, while
		// the finalize turn's next park reached nobody. A resumed reviewer
		// without closed_last is a daemon older than that field: it resumes
		// only when the last open question closed, so it gets the follow too.
		driveCtx, cancel, err := boundAxiWait(ctx, aa.wait)
		if err != nil {
			return emitError(cmd, 2, err.Error(), "Pass a positive duration such as --wait 8m")
		}
		defer cancel()
		grace := env.cfg.GateReconcileTimeout
		if grace <= 0 {
			grace = config.DefaultGateReconcileTimeout
		}
		final, ciReady, err := followAnsweredReview(driveCtx, cmd.ErrOrStderr(), env.client, env.p.Socket(), runID, result.Resumed, preAnswer, grace)
		if err != nil {
			if isAxiWaitElapsed(ctx, driveCtx, err) {
				return emitAxiWaitElapsed(cmd, aa.wait, "no-mistakes axi run")
			}
			return emitError(cmd, 1, fmt.Sprintf("follow the resumed review: %v", err),
				"The answer is recorded; run `no-mistakes axi run` to reattach to the run")
		}
		// The run object that follows carries the id, so the scalar run key
		// is dropped rather than emitted twice.
		lead := make([]toon.Field, 0, len(fields))
		for _, f := range fields {
			if f.Key != "run" {
				lead = append(lead, f)
			}
		}
		return renderDriveResult(cmd, final, ciReady, lead...)
	}
	var help []string
	switch {
	case result.Open > 0:
		help = append(help, "Answer the remaining questions with `no-mistakes axi answer --question <id> --answer \"...\"`; the review stays parked until none are open")
	case result.Resumed:
		help = append(help, "The reviewer is finishing its pass with your answers; run `no-mistakes axi status` for its findings and the next gate")
	default:
		help = append(help, "The answer is recorded and the reviewer will read it at its next checkpoint; run `no-mistakes axi status` to follow the run")
	}
	fields = append(fields, toon.Field{Key: "help", Value: help})
	emitDoc(cmd, fields...)
	return nil
}

// followAnsweredReview drives a run whose last open review question was just
// answered to its next decision point.
//
// Two parks must not be mistaken for that decision point. The gate this answer
// released may still read as parked for a moment, which waitStepLeavesGate
// covers exactly as it does for respond - told the identity that park had
// before the answer was sent (preAnswer), since the finalize turn can park
// again at the same status and only its round tells the two apart; the park is
// read now instead only when the review step was not parked then. And an answer
// that lands between the reviewer's turn ending and the executor registering
// its park is recorded before that park is published, so the park appears
// afterwards still carrying the questions this answer closed until the gate's
// own resumer releases it. Only the daemon can tell that park from a genuine
// new question, and it says so by releasing it: a LIVE park re-checks its
// conversation the moment it registers (waitForApprovalOrReconcile with
// immediate=true), bounded by gate_reconcile_timeout. So each question park
// gets grace (that same timeout, read from the owning root's global config) to
// leave, and one still there after it asked something new and is returned.
//
// A park restored by daemon recovery is the accepted exception: it is waited
// on with immediate=false, so its first resumer check is one
// gate_reconcile_interval away and the grace can expire before that park is
// released. An answer racing a daemon restart therefore gets the park it just
// answered back; reattach with `axi run` rather than answering again.
func followAnsweredReview(ctx context.Context, progress io.Writer, client *ipc.Client, socketPath, runID string, resumed bool, preAnswer *gateIdentity, grace time.Duration) (*ipc.RunInfo, bool, error) {
	review := string(types.StepReview)
	if resumed {
		gate := preAnswer
		if gate == nil {
			// The review step was not parked when the answer was sent, so the
			// park it released can only be read now.
			run, err := getRunInfo(ctx, socketPath, runID)
			if err != nil {
				return nil, false, err
			}
			if observed, ok := reviewGate(run); ok {
				id := observed.identity()
				gate = &id
			}
		}
		if gate != nil {
			if err := waitStepLeavesGate(ctx, socketPath, runID, review, *gate); err != nil {
				return nil, false, err
			}
		}
	}
	for {
		final, ciReady, err := driveRun(ctx, progress, client, socketPath, runID, false)
		if err != nil {
			return final, ciReady, err
		}
		gate, ok := reviewGate(final)
		if !ok || !pipeline.HasUnansweredReviewQuestion(gate.FindingsJSON) {
			return final, ciReady, nil
		}
		graceCtx, cancel := context.WithTimeout(ctx, grace)
		err = waitStepLeavesGate(graceCtx, socketPath, runID, review, gate.identity())
		cancel()
		if err != nil {
			if graceExpired(ctx, err) {
				return final, ciReady, nil
			}
			return nil, false, err
		}
	}
}

// graceExpired reports whether err ended a question park's grace on its own
// rather than the outer --wait running out underneath it. ctx.Err() cannot
// decide that: a run-state read reports a passed deadline before its context's
// timer fires (deadlinePassedErr), and whenever the remaining --wait is shorter
// than the grace, context.WithTimeout hands back a timerless cancel-child of
// that same deadline, so an elapsed --wait arrives here as DeadlineExceeded
// with a nil Err(). Credited to the grace, that would return the park this
// answer just closed as the run's next gate at exit 0, instead of the elapsed
// wait isAxiWaitElapsed classifies from the same deadline.
func graceExpired(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && !deadlinePassed(ctx)
}

// reviewGate returns the review step's gate when run is parked at it, whichever
// park status the executor picked: awaiting_approval for a review pass of its
// own, fix_review for the rereview inside a fix round, which a question can be
// asked by just the same. Every wait below is told the identity this saw, since
// waitStepLeavesGate reads any other identity as a gate already left - so the
// two comparisons can never drift apart.
func reviewGate(run *ipc.RunInfo) (stepView, bool) {
	if run == nil {
		return stepView{}, false
	}
	gate, ok := runViewFromIPC(run).awaitingStep()
	if !ok || gate.Name != string(types.StepReview) {
		return stepView{}, false
	}
	return gate, true
}
