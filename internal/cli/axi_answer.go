package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
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
			"reviewer wrote them so the answer would be unambiguous.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-answer", "/axi/answer", nil, func() error {
				return runAxiAnswer(cmd, answerArgs{
					questionID: strings.TrimSpace(questionID),
					answer:     strings.TrimSpace(answer),
					answeredBy: strings.TrimSpace(answeredBy),
				})
			})
		},
	}
	cmd.Flags().StringVar(&questionID, "question", "", "question id, as carried by the review gate's `question-<id>` findings or named by the gate's omission notice (required)")
	cmd.Flags().StringVar(&answer, "answer", "", "the answer, ideally one of the question's stated options (required)")
	cmd.Flags().StringVar(&answeredBy, "by", "", "who answered, recorded on the PR and in the branch's settled questions")
	return cmd
}

type answerArgs struct {
	questionID string
	answer     string
	answeredBy string
}

func runAxiAnswer(cmd *cobra.Command, aa answerArgs) error {
	if aa.questionID == "" || aa.answer == "" {
		return emitError(cmd, 2, "--question and --answer are both required",
			`Run `+"`no-mistakes axi status`"+` to list the reviewer's open questions and their ids`)
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
