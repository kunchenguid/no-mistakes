package steps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	slowCheckPending  = `{"name":"test (windows-core)","state":"IN_PROGRESS","bucket":"pending","app":"github-actions"}`
	slowCheckFailed   = `{"name":"test (windows-core)","state":"FAILURE","bucket":"fail","app":"github-actions"}`
	greptilePassed    = `{"name":"Greptile Review","state":"SUCCESS","bucket":"pass","app":"greptile-apps","link":"https://greptile.com/"}`
	greptileFailed    = `{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps","link":"https://greptile.com/"}`
	greptilePending   = `{"name":"Greptile Review","state":"IN_PROGRESS","bucket":"pending","app":"greptile-apps","link":"https://greptile.com/"}`
	earlyBuildPassed  = `{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}`
	errStopPollingEnd = "stop polling"
)

func checksJSON(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

type earlyRun struct {
	outcomeErr error
	outcomeNil bool
	findings   types.Findings
	polls      int
	logs       string
	agentCalls int
	everReady  bool
}

// runEarlyCI executes the CI step against the given checks until it returns an
// outcome or its first wait for the next poll, which stands for "the step kept
// monitoring".
func runEarlyCI(t *testing.T, policy string, early bool, checks string) earlyRun {
	t.Helper()
	env := append(fakeCIGH(t, "OPEN", checks), greenGreptileCommentJSON)
	sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, policy, env)
	sctx.Config.CI.ReviewBotCommentsEarly = early
	var run earlyRun
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		run.polls++
		return errors.New(errStopPollingEnd)
	}}
	outcome, err := step.Execute(sctx)
	run.outcomeErr = err
	run.outcomeNil = outcome == nil
	if outcome != nil {
		parsed, perr := types.ParseFindingsJSON(outcome.Findings)
		if perr != nil {
			t.Fatal(perr)
		}
		run.findings = parsed
		if !outcome.NeedsApproval || outcome.AutoFixable {
			t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
		}
	}
	run.logs = strings.Join(*logs, "\n")
	run.agentCalls = len(ag.calls)
	run.everReady = *everReady
	return run
}

func (r earlyRun) keptMonitoring() bool {
	return r.outcomeNil && r.outcomeErr != nil && strings.Contains(r.outcomeErr.Error(), errStopPollingEnd) && r.polls == 1
}

// With the setting off, a completed review bot's comments wait for every other
// check exactly as before, whichever policy is configured.
func TestCIStep_ReviewBotEarlyOffWaitsForEveryCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, policy, checks string }{
		{"always, green bot", config.CIReviewBotCommentsAlways, checksJSON(earlyBuildPassed, slowCheckPending, greptilePassed)},
		{"on_failure, red bot", config.CIReviewBotCommentsOnFailure, checksJSON(earlyBuildPassed, slowCheckPending, greptileFailed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runEarlyCI(t, tc.policy, false, tc.checks)
			if !run.keptMonitoring() {
				t.Fatalf("outcome nil=%v err=%v polls=%d, want the monitor to keep waiting for the pending check", run.outcomeNil, run.outcomeErr, run.polls)
			}
			if strings.Contains(run.logs, "before all checks completed") {
				t.Fatalf("logs = %q, no early observation with the setting off", run.logs)
			}
		})
	}
}

// With the setting on, the bot's comments become the usual ask-user findings
// while another check is still running, and no round, push or readiness is
// recorded along the way.
func TestCIStep_ReviewBotEarlyParksBeforeRemainingChecksFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, policy, checks string }{
		{"always, green bot", config.CIReviewBotCommentsAlways, checksJSON(earlyBuildPassed, slowCheckPending, greptilePassed)},
		{"on_failure, red bot", config.CIReviewBotCommentsOnFailure, checksJSON(earlyBuildPassed, slowCheckPending, greptileFailed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runEarlyCI(t, tc.policy, true, tc.checks)
			if run.outcomeErr != nil || run.outcomeNil || run.polls != 0 {
				t.Fatalf("err=%v nil=%v polls=%d, want a park on the first poll", run.outcomeErr, run.outcomeNil, run.polls)
			}
			if len(run.findings.Items) != 1 {
				t.Fatalf("findings = %+v, want the one unresolved Greptile comment", run.findings.Items)
			}
			item := run.findings.Items[0]
			if item.Action != types.ActionAskUser || item.Category != types.FindingCategoryCIReviewBot || item.Check != "Greptile Review" || item.File != "src/commands/api.test.ts" || item.Line != 88 {
				t.Fatalf("finding = %+v, want an ask-user Greptile finding anchored to the comment", item)
			}
			if run.agentCalls != 0 || run.everReady {
				t.Fatalf("agent calls = %d, ready = %v, want neither", run.agentCalls, run.everReady)
			}
			if !strings.Contains(run.logs, "issues detected before all checks completed") {
				t.Fatalf("logs = %q, want the early observation logged", run.logs)
			}
		})
	}
}

// An early round belongs to the head the bot reviewed: a bot that has not
// finished its check on this head, or has not registered one, is never read,
// even though threads from an earlier head are still unresolved. A green bot
// under the default policy stays unread as well.
func TestCIStep_ReviewBotEarlyIgnoresABotThatHasNotReviewedTheHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, policy, checks string }{
		{"bot check still running", config.CIReviewBotCommentsAlways, checksJSON(earlyBuildPassed, slowCheckPending, greptilePending)},
		{"bot not registered on the head", config.CIReviewBotCommentsAlways, checksJSON(earlyBuildPassed, slowCheckPending)},
		{"green bot under on_failure", config.CIReviewBotCommentsOnFailure, checksJSON(earlyBuildPassed, slowCheckPending, greptilePassed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runEarlyCI(t, tc.policy, true, tc.checks)
			if !run.keptMonitoring() {
				t.Fatalf("outcome nil=%v err=%v polls=%d, want the monitor to keep waiting", run.outcomeNil, run.outcomeErr, run.polls)
			}
		})
	}
}

// The early observation carries only the bot's findings. A check that is
// already red keeps waiting for the rest of the head, and gets its own round
// from the settled observation.
func TestCIStep_ReviewBotEarlyLeavesOtherChecksToTheirOwnRound(t *testing.T) {
	t.Parallel()
	run := runEarlyCI(t, config.CIReviewBotCommentsAlways, true, checksJSON(slowCheckFailed, earlyBuildPassed, slowCheckPending, greptilePassed))
	if run.outcomeErr != nil || run.outcomeNil {
		t.Fatalf("err=%v nil=%v, want the early park", run.outcomeErr, run.outcomeNil)
	}
	for _, item := range run.findings.Items {
		if item.Category != types.FindingCategoryCIReviewBot {
			t.Fatalf("finding = %+v, want only review-bot findings in the early observation", item)
		}
	}

	settled := runEarlyCIFinished(t, checksJSON(slowCheckFailed, earlyBuildPassed, greptilePassed))
	var sawCheck, sawBot bool
	for _, item := range settled.Items {
		sawCheck = sawCheck || item.Category == types.FindingCategoryCICheck
		sawBot = sawBot || item.Category == types.FindingCategoryCIReviewBot
	}
	if !sawCheck || !sawBot {
		t.Fatalf("settled findings = %+v, want the red check and the bot comment once every check has completed", settled.Items)
	}
}

func runEarlyCIFinished(t *testing.T, checks string) types.Findings {
	t.Helper()
	env := append(fakeCIGH(t, "OPEN", checks), greenGreptileCommentJSON)
	sctx, _, _, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	sctx.Config.CI.ReviewBotCommentsEarly = true
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil {
		t.Fatalf("outcome = %#v, err = %v, want the settled observation", outcome, err)
	}
	parsed, perr := types.ParseFindingsJSON(outcome.Findings)
	if perr != nil {
		t.Fatal(perr)
	}
	return parsed
}

// An unreadable comment list never parks the early observation: the monitor
// keeps waiting and the settled observation reads the list again.
func TestCIStep_ReviewBotEarlyReadFailureKeepsMonitoring(t *testing.T) {
	t.Parallel()
	env := append(fakeCIGH(t, "OPEN", checksJSON(earlyBuildPassed, slowCheckPending, greptilePassed)), `FAKE_CLI_REVIEW_COMMENTS=not-json`)
	sctx, _, logs, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	sctx.Config.CI.ReviewBotCommentsEarly = true
	polls := 0
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		polls++
		return errors.New(errStopPollingEnd)
	}}
	outcome, err := step.Execute(sctx)
	if outcome != nil || err == nil || polls != 1 {
		t.Fatalf("outcome = %#v, err = %v, polls = %d, want the monitor to keep waiting", outcome, err, polls)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "could not read review bot comments early") {
		t.Fatalf("logs = %v, want the read failure logged", *logs)
	}
}

// A second registered bot that has no check on the head is dropped from an
// early observation: its unresolved threads can belong to an earlier head.
func TestCIObservationFindings_RegisteredBotsOnlyDropsABotWithoutACheck(t *testing.T) {
	t.Parallel()
	issues := ciIssues{
		provider:        scm.ProviderGitHub,
		requiredReviews: []string{"coderabbit"},
		checks:          []scm.Check{{Name: "Greptile Review", Bucket: scm.CheckBucketPass, App: "greptile-apps"}},
		botComments: []scm.ReviewComment{
			{ID: "old", Author: "coderabbitai[bot]", AuthorID: scm.CodeRabbitAuthorID, Path: "a.go", Line: 1, Body: "from an earlier head"},
		},
		greenBots: true,
	}
	if got := ciObservationFindings(issues); len(got.Items) != 1 {
		t.Fatalf("findings = %+v, want the settled observation to read the unregistered bot", got.Items)
	}
	issues.registeredBotsOnly = true
	if got := ciObservationFindings(issues); len(got.Items) != 0 {
		t.Fatalf("findings = %+v, want none: the bot has no check on this head", got.Items)
	}
}

func earlyParkedStep(t *testing.T, checks string) (*CIStep, *pipeline.StepContext, *pipeline.StepOutcome, *int) {
	t.Helper()
	env := append(fakeCIGH(t, "OPEN", checks), greenGreptileCommentJSON)
	sctx, _, _, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	sctx.Config.CI.ReviewBotCommentsEarly = true
	polls := new(int)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		*polls++
		return errors.New(errStopPollingEnd)
	}}
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, err = %v, want the early park", outcome, err)
	}
	return step, sctx, outcome, polls
}

// Approving or skipping the early round while a check still runs dismisses
// only those comments: the step keeps monitoring, and the check that turns red
// later gets its own round.
func TestCIStep_ReviewBotEarlyApprovalKeepsMonitoringForALaterRedCheck(t *testing.T) {
	t.Parallel()
	step, sctx, parked, polls := earlyParkedStep(t, checksJSON(earlyBuildPassed, slowCheckPending, greptilePassed))
	if !step.ContinueAfterApproval(sctx, parked.Findings) {
		t.Fatal("ContinueAfterApproval = false, want the step to keep monitoring while a check runs")
	}

	outcome, err := step.Execute(sctx)
	if outcome != nil || err == nil || *polls != 1 {
		t.Fatalf("after approval: outcome = %#v, err = %v, polls = %d, want the monitor to keep waiting without re-parking on the dismissed comment", outcome, err, *polls)
	}

	sctx.Env = append(fakeCIGH(t, "OPEN", checksJSON(slowCheckFailed, earlyBuildPassed, greptilePassed)), greenGreptileCommentJSON)
	step.waitForNextPoll = func(context.Context, time.Duration) error { return nil }
	outcome, err = step.Execute(sctx)
	if err != nil || outcome == nil {
		t.Fatalf("outcome = %#v, err = %v, want the red check's own round", outcome, err)
	}
	parsed, perr := types.ParseFindingsJSON(outcome.Findings)
	if perr != nil {
		t.Fatal(perr)
	}
	var sawRedCheck bool
	for _, item := range parsed.Items {
		sawRedCheck = sawRedCheck || item.Category == types.FindingCategoryCICheck && item.Check == "test (windows-core)"
	}
	if !sawRedCheck {
		t.Fatalf("findings = %+v, want a finding for the check that failed after the approval", parsed.Items)
	}
}

// Once every check has completed there is nothing left to monitor, so the
// answer completes the step as it always did.
func TestCIStep_ReviewBotEarlyApprovalCompletesWhenNoCheckIsRunning(t *testing.T) {
	t.Parallel()
	step, sctx, parked, _ := earlyParkedStep(t, checksJSON(earlyBuildPassed, slowCheckPending, greptilePassed))
	sctx.Env = append(fakeCIGH(t, "OPEN", checksJSON(earlyBuildPassed, greptilePassed)), greenGreptileCommentJSON)
	if step.ContinueAfterApproval(sctx, parked.Findings) {
		t.Fatal("ContinueAfterApproval = true with every check completed, want an ordinary approval")
	}
	if step.ContinueAfterApproval(sctx, `{"findings":[{"id":"x","severity":"error","category":"ci-check","description":"red"}]}`) {
		t.Fatal("ContinueAfterApproval = true for a gate that is not review-bot findings only")
	}
}
