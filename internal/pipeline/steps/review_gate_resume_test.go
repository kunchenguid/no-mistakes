package steps

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const openQuestionGateFindings = `{"findings":[{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: keep the legacy route?","action":"ask-user","category":"review-question"}],"summary":"one open question"}`

// TestReviewStep_ResumeApprovalGateOnlyWhenTheConversationIsSettled covers every
// condition the resumer has to get right before it may hand the executor an
// action, because each wrong answer has its own failure: resuming a gate parked
// on ordinary code findings steals the operator's verdict, resuming while a
// question is open defeats the park, and resuming with the conversation off
// changes behaviour for repositories that never opted in.
func TestReviewStep_ResumeApprovalGateOnlyWhenTheConversationIsSettled(t *testing.T) {
	for _, tc := range []struct {
		name         string
		conversation bool
		findings     string
		seed         func(t *testing.T, dir string)
		wantResume   bool
	}{
		{
			name:         "settled question resumes the reviewer",
			conversation: true,
			findings:     openQuestionGateFindings,
			seed: func(t *testing.T, dir string) {
				appendQA(t, dir, "q1", "keep")
			},
			wantResume: true,
		},
		{
			name:         "a still-open question keeps the gate parked",
			conversation: true,
			findings:     openQuestionGateFindings,
			seed: func(t *testing.T, dir string) {
				if err := appendAgentQuestionLine(dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The same defect class as the answer handler's orphan release: a
			// gate parked on code findings is the operator's to answer.
			name:         "a gate with no question findings is left alone",
			conversation: true,
			findings:     `{"findings":[{"id":"f1","severity":"warning","description":"ordinary finding","action":"ask-user"}],"summary":"one issue"}`,
			seed:         func(t *testing.T, dir string) {},
		},
		{
			// A repository that never opted in cannot have a conversation on
			// disk, so this is the real off state and it must behave exactly
			// as it did before the feature existed.
			name:         "the conversation being off with nothing on disk resumes nothing",
			conversation: false,
			findings:     openQuestionGateFindings,
			seed:         func(t *testing.T, dir string) {},
		},
		{
			// The conversation was ON when the reviewer asked - the files on
			// disk are the proof - and review.conversation was turned off (or
			// a trusted-config fetch failed, which resolves the same way)
			// before the answer landed. The questions are answerable and the
			// finalize turn reads them from disk, so the resumer must fire or
			// the race it exists to close strands exactly the run it was
			// added for.
			name:         "an answered conversation on disk resumes even with the setting off",
			conversation: false,
			findings:     openQuestionGateFindings,
			seed: func(t *testing.T, dir string) {
				appendQA(t, dir, "q1", "keep")
			},
			wantResume: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, newStaticReviewAgent(cleanReviewJSON), dir, baseSHA, headSHA, config.Commands{})
			if tc.conversation {
				enableReviewConversation(sctx.Config)
			}
			// Seeded at the path the enabled feature uses, so an off case that
			// seeds nothing is genuinely empty and one that seeds is genuinely
			// a conversation an earlier, enabled turn left behind.
			tc.seed(t, reviewqa.Dir(sctx.EvidenceDir))

			action, resume, err := (&ReviewStep{}).ResumeApprovalGate(sctx, tc.findings)
			if err != nil {
				t.Fatalf("resume check: %v", err)
			}
			if resume != tc.wantResume {
				t.Fatalf("resume = %v, want %v", resume, tc.wantResume)
			}
			if resume && action != types.ActionAnswer {
				t.Fatalf("action = %q, want %q - the step must be re-entered, never completed", action, types.ActionAnswer)
			}
			if !resume && action != "" {
				t.Fatalf("action = %q with resume false", action)
			}
		})
	}
}

// Unreadable findings leave the gate parked rather than resuming it, which is
// the fail-closed direction the interface requires.
func TestReviewStep_ResumeApprovalGateFailsClosedOnUnreadableFindings(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := withReviewConversation(newTestContextWithDBRecords(t, newStaticReviewAgent(cleanReviewJSON), dir, baseSHA, headSHA, config.Commands{}))
	if _, resume, err := (&ReviewStep{}).ResumeApprovalGate(sctx, "{not json"); err == nil || resume {
		t.Fatalf("unreadable findings must fail closed: resume=%v err=%v", resume, err)
	}
}

// TestReviewStep_AnswerRacingTheParkStillReachesTheReviewer drives the state the
// race leaves behind, through the real executor.
//
// ReviewStep.Execute loads the conversation, builds a finding for the open
// question and returns; only afterwards does the executor write the step rows
// and register the gate as waiting. An answer landing in that window is appended
// to disk, so the daemon's answer handler sees nothing open, calls Respond, gets
// "no step awaiting approval" and reports the answer recorded - truthfully, but
// no reviewer is left to read it. The resulting state is exactly what this test
// sets up: a gate parked on a question finding, nothing open on disk, and no
// response coming. ReviewStep implemented no gate re-check at all, so
// waitForApprovalOrReconcile blocked on approvalCh alone and the run parked
// forever.
//
// Nothing here calls Respond. The run has to move on its own, or it does not
// move.
func TestReviewStep_AnswerRacingTheParkStillReachesTheReviewer(t *testing.T) {
	mock := &sessionMockAgent{}
	var convDir string
	turns := make(chan int, 4)
	turn := 0
	mock.respond = func(agent.RunOpts) *agent.Result {
		turn++
		if turn == 1 {
			// The asking turn emits a question and leaves it OPEN, so the step
			// genuinely parks on it.
			if err := appendAgentQuestionLine(convDir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`); err != nil {
				t.Errorf("append question: %v", err)
			}
		}
		turns <- turn
		return &agent.Result{Output: []byte(cleanReviewJSON)}
	}
	exec, database, run, repo, workDir := reviewSessionHarness(t, mock, []pipeline.Step{&ReviewStep{}}, enableReviewConversation)
	convDir = exec.ReviewConversationDir(run.ID)
	// Poll fast so the test does not wait out the production interval.
	exec.SetGateReconcileTimings(20*time.Millisecond, 5*time.Second)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForReviewStatus(t, database, run.ID, types.StepStatusAwaitingApproval)

	// The answer the race stranded: on disk, with no gate release behind it.
	if err := reviewqa.AppendAnswer(convDir, reviewqa.Answer{ID: "q1", Answer: "keep", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatalf("append the stranded answer: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run never left the review gate; the answer that raced the park was lost")
	}

	if turn < 2 {
		t.Fatalf("the reviewer ran %d turn(s); the finalize turn never received the answer", turn)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.StepName == types.StepReview && s.Status != types.StepStatusCompleted {
			t.Fatalf("review status = %q, want completed", s.Status)
		}
	}
	// The finalize round is an answer, not a fix and not a second initial pass.
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) < 2 || rounds[len(rounds)-1].Trigger != "answer" {
		t.Fatalf("rounds = %d, last trigger = %q, want an \"answer\" round", len(rounds), rounds[len(rounds)-1].Trigger)
	}
}

func appendQA(t *testing.T, dir, id, answer string) {
	t.Helper()
	// No kind and no weight, the shape a model that trimmed the prompt's
	// worked example produces; the reader must still take it as a major
	// question.
	if err := appendAgentQuestionLine(dir, fmt.Sprintf(`{"id":%q,"question":"keep the legacy route?","options":["keep","remove"]}`, id)); err != nil {
		t.Fatal(err)
	}
	if err := reviewqa.AppendAnswer(dir, reviewqa.Answer{ID: id, Answer: answer, AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	// Assert the seed TOOK. "Nothing is open" is also true of a conversation
	// the reader threw away, so without this a subtest expecting a settled
	// question would pass on a dropped one.
	conv, err := reviewqa.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Answered()) != 1 || conv.Answered()[0].ID != id {
		t.Fatalf("seeded question %q did not read back as answered: %+v", id, conv)
	}
}
