package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// cubic publishes its check under the GitHub app slug "cubic-dev-ai" and
// posts its review threads as "cubic-dev-ai[bot]"; GitHub's GraphQL
// review-thread query reports the same Bot actor by its bare login.
const (
	cubicAppSlug   = "cubic-dev-ai"
	cubicRESTLogin = "cubic-dev-ai[bot]"
	cubicBareLogin = "cubic-dev-ai"
)

// greenCubicChecksJSON is the shape cubic leaves on an anam-org PR: every
// job green and cubic's own check concluded success, with its review thread
// still unresolved.
const greenCubicChecksJSON = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"},{"name":"cubic","state":"SUCCESS","bucket":"pass","app":"` + cubicAppSlug + `","link":"https://cubic.dev/"}]`

func cubicCommentJSON(author string) string {
	return `FAKE_CLI_REVIEW_COMMENTS=[{"author":"` + author + `","path":"internal/session/token.go","line":57,"body":"The refresh token is logged at debug level"}]`
}

func parkedCubicFinding(t *testing.T, outcome *pipeline.StepOutcome, err error, wantCheck string) Finding {
	t.Helper()
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one per unresolved cubic comment", findings.Items)
	}
	item := findings.Items[0]
	if item.Action != types.ActionAskUser || item.Severity != types.FindingSeverityWarning || item.Category != types.FindingCategoryCIReviewBot || item.Check != wantCheck {
		t.Fatalf("finding = %+v, want an ask-user ci-review-bot warning for check %q", item, wantCheck)
	}
	if item.File != "internal/session/token.go" || item.Line != 57 || !strings.Contains(item.Description, "refresh token is logged") {
		t.Fatalf("finding = %+v, want the comment's file, line, and body", item)
	}
	return item
}

// Under ci.review_bot_comments: always, cubic's unresolved thread on a head
// where its own check concluded green parks the CI step as one ask-user
// finding, spends no fix round, and never reports checks-passed.
func TestCIStep_CubicGreenCheckCommentsParkUnderAlways(t *testing.T) {
	t.Parallel()
	env := append(fakeCIGH(t, "OPEN", greenCubicChecksJSON), cubicCommentJSON(cubicRESTLogin))
	sctx, ag, logs, everReady, headSHA := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("an unresolved cubic comment must park, not keep polling")
		return nil
	}}

	outcome, err := driveCI(t, step, sctx)
	item := parkedCubicFinding(t, outcome, err, "cubic")
	if !strings.HasPrefix(item.Description, cubicRESTLogin+": ") {
		t.Fatalf("finding = %+v, want the description attributed to %s", item, cubicRESTLogin)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent invocations = %d, want none: cubic's comment never spends an auto_fix.ci round", len(ag.calls))
	}
	if got := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("head moved to %s without a fix round", got)
	}
	joined := strings.Join(*logs, "\n")
	if strings.Contains(joined, ciChecksPassedMsg) || *everReady {
		t.Fatalf("logs = %v, checks-passed must not be reported over an unresolved cubic comment", *logs)
	}
	if !strings.Contains(joined, "issues detected: review bot check cubic needs a decision (1 finding)") {
		t.Fatalf("logs = %v, want the observation logged", *logs)
	}
}

// cubic often comments after CI is already green, before (or without) a
// check of its own on the head. Under always the thread still parks; under
// the default policy the green head is reported checks-passed as before.
func TestCIStep_CubicCommentsAfterGreenCIFollowThePolicy(t *testing.T) {
	t.Parallel()
	const fastChecksJSON = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}]`
	t.Run("always", func(t *testing.T) {
		t.Parallel()
		env := append(fakeCIGH(t, "OPEN", fastChecksJSON), cubicCommentJSON(cubicRESTLogin))
		sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
		step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
			t.Fatal("an unresolved cubic comment must park, not keep polling")
			return nil
		}}

		outcome, err := driveCI(t, step, sctx)
		parkedCubicFinding(t, outcome, err, "")
		if len(ag.calls) != 0 {
			t.Fatalf("agent invocations = %d, want none", len(ag.calls))
		}
		joined := strings.Join(*logs, "\n")
		if strings.Contains(joined, ciChecksPassedMsg) || *everReady {
			t.Fatalf("logs = %v, checks-passed must not be reported over an unresolved cubic comment", *logs)
		}
		if !strings.Contains(joined, "issues detected: review bot comments need a decision (1 finding)") {
			t.Fatalf("logs = %v, want the observation logged", *logs)
		}
	})
	t.Run("on_failure", func(t *testing.T) {
		t.Parallel()
		env := append(fakeCIGH(t, "OPEN", fastChecksJSON), cubicCommentJSON(cubicRESTLogin))
		sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
		step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}

		outcome, err := step.Execute(sctx)
		if err == nil || outcome != nil {
			t.Fatalf("outcome = %#v, err = %v, want monitoring to continue until the poll is cancelled", outcome, err)
		}
		if len(ag.calls) != 0 {
			t.Fatalf("agent invocations = %d, want none", len(ag.calls))
		}
		if !strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) || !*everReady {
			t.Fatalf("logs = %v, want checks-passed reported under the default policy", *logs)
		}
	})
}

// GitHub's GraphQL review-thread query names a Bot actor by its bare login,
// so a thread reported as "cubic-dev-ai" is cubic's and parks like the
// "[bot]" form.
func TestCIStep_CubicBareGraphQLLoginIsAttributedToCubic(t *testing.T) {
	t.Parallel()
	env := append(fakeCIGH(t, "OPEN", greenCubicChecksJSON), cubicCommentJSON(cubicBareLogin))
	sctx, ag, _, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("an unresolved cubic comment must park, not keep polling")
		return nil
	}}

	outcome, err := driveCI(t, step, sctx)
	item := parkedCubicFinding(t, outcome, err, "cubic")
	if !strings.HasPrefix(item.Description, cubicBareLogin+": ") {
		t.Fatalf("finding = %+v, want the description attributed to %s", item, cubicBareLogin)
	}
	if len(ag.calls) != 0 || *everReady {
		t.Fatalf("agent invocations = %d, ready = %v; want no fix round and no readiness", len(ag.calls), *everReady)
	}
}

// A red cubic check is cubic's opinion, not a job verdict: under the default
// policy it parks for a decision carrying the unresolved thread, and never
// spends an auto_fix.ci round on it.
func TestCIStep_CubicRedCheckParksWithoutAFixRound(t *testing.T) {
	t.Parallel()
	const redCubicChecksJSON = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"},{"name":"cubic","state":"FAILURE","bucket":"fail","app":"` + cubicAppSlug + `","link":"https://cubic.dev/"}]`
	env := append(fakeCIGH(t, "OPEN", redCubicChecksJSON), cubicCommentJSON(cubicRESTLogin))
	sctx, ag, logs, everReady, headSHA := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("a red cubic check must park, not keep polling")
		return nil
	}}

	outcome, err := driveCI(t, step, sctx)
	parkedCubicFinding(t, outcome, err, "cubic")
	if len(ag.calls) != 0 {
		t.Fatalf("agent invocations = %d, want none: a review bot's red check never spends an auto_fix.ci round", len(ag.calls))
	}
	if got := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("head moved to %s without a fix round", got)
	}
	if strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) || *everReady {
		t.Fatalf("logs = %v, checks-passed must not be reported over a red cubic check", *logs)
	}
}

// Registering cubic widens nothing else: under always, threads from an
// unregistered bot or a human on a green head are not review-bot findings,
// and the head is reported checks-passed.
func TestCIStep_UnregisteredAuthorsNeverParkUnderAlways(t *testing.T) {
	t.Parallel()
	env := append(fakeCIGH(t, "OPEN", greenCubicChecksJSON),
		`FAKE_CLI_REVIEW_COMMENTS=[{"author":"dependabot[bot]","path":"go.mod","line":3,"body":"bump"},{"author":"octocat","path":"main.go","line":1,"body":"nit"},{"author":"cubic-dev-ai-impostor[bot]","path":"main.go","line":2,"body":"not cubic"}]`)
	sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}

	outcome, err := step.Execute(sctx)
	if err == nil || outcome != nil {
		t.Fatalf("outcome = %#v, err = %v, want monitoring to continue until the poll is cancelled", outcome, err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent invocations = %d, want none", len(ag.calls))
	}
	if !strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) || !*everReady {
		t.Fatalf("logs = %v, want checks-passed over threads no registered bot posted", *logs)
	}
}
