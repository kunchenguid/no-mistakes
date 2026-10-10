package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// greenGreptileChecksJSON is an all-green head on which Greptile concluded
// its own check success, the shape of kunchenguid/gh-axi#184 after the CI
// step's first repair: green, yet with an unresolved bot comment.
const greenGreptileChecksJSON = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"},{"name":"Greptile Review","state":"SUCCESS","bucket":"pass","app":"greptile-apps","link":"https://greptile.com/"}]`

const greenGreptileCommentJSON = `FAKE_CLI_REVIEW_COMMENTS=[{"author":"greptile-apps[bot]","path":"src/commands/api.test.ts","line":88,"body":"The test asserts a request GitHub would reject"}]`

func newGreenReviewBotContext(t *testing.T, policy string, env []string) (*pipeline.StepContext, *mockAgent, *[]string, *bool, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI.ReviewBotComments = policy
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	everReady := false
	sctx.CIReadinessChanged = func(ready, _ bool) { everReady = everReady || ready }
	return sctx, ag, &logs, &everReady, headSHA
}

// Under ci.review_bot_comments: always, an unresolved comment from a review
// bot whose check concluded green parks the step exactly as a red bot check
// does: one ask-user finding per comment, no fix round, and no checks-passed.
func TestCIStep_GreenReviewBotCommentsParkUnderAlways(t *testing.T) {
	t.Parallel()
	env := append(fakeCIGH(t, "OPEN", greenGreptileChecksJSON), greenGreptileCommentJSON)
	sctx, ag, logs, everReady, headSHA := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("an unresolved green review-bot comment must park, not keep polling")
		return nil
	}}

	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent invocations = %d, want none: a review bot's comment never spends an auto_fix.ci round", len(ag.calls))
	}
	if got := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("head moved to %s without a fix round", got)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one per unresolved Greptile comment", findings.Items)
	}
	item := findings.Items[0]
	if item.Action != types.ActionAskUser || item.Severity != types.FindingSeverityWarning || item.Category != types.FindingCategoryCIReviewBot || item.Check != "Greptile Review" {
		t.Fatalf("finding = %+v, want an ask-user ci-review-bot warning for Greptile Review", item)
	}
	if item.File != "src/commands/api.test.ts" || item.Line != 88 || !strings.Contains(item.Description, "GitHub would reject") {
		t.Fatalf("finding = %+v, want the comment's file, line, and body", item)
	}
	joined := strings.Join(*logs, "\n")
	if strings.Contains(joined, ciChecksPassedMsg) {
		t.Fatalf("logs = %v, checks-passed must not be reported over an unresolved bot comment", *logs)
	}
	if !strings.Contains(joined, "issues detected: review bot check Greptile Review needs a decision (1 finding)") {
		t.Fatalf("logs = %v, want the observation logged", *logs)
	}
	if *everReady {
		t.Fatal("the run was marked CI-ready over an unresolved bot comment")
	}
}

// Fast checks can finish green on a new head before the review bot has
// registered its check there, while its threads from an earlier head stay
// unresolved. Under always those comments are still read, withhold
// checks-passed, and park as ask-user findings; under the default the head is
// green as before.
func TestCIStep_UnregisteredReviewBotCommentsFollowThePolicy(t *testing.T) {
	t.Parallel()
	const fastChecksJSON = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}]`
	t.Run("always", func(t *testing.T) {
		t.Parallel()
		env := append(fakeCIGH(t, "OPEN", fastChecksJSON), greenGreptileCommentJSON)
		sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
		step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
			t.Fatal("an unresolved review-bot comment must park, not keep polling")
			return nil
		}}

		outcome, err := driveCI(t, step, sctx)
		if err != nil {
			t.Fatalf("CI step returned error: %v", err)
		}
		if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
			t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
		}
		if len(ag.calls) != 0 {
			t.Fatalf("agent invocations = %d, want none", len(ag.calls))
		}
		findings, err := types.ParseFindingsJSON(outcome.Findings)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings.Items) != 1 {
			t.Fatalf("findings = %+v, want one per unresolved Greptile comment", findings.Items)
		}
		item := findings.Items[0]
		if item.Action != types.ActionAskUser || item.Category != types.FindingCategoryCIReviewBot || item.File != "src/commands/api.test.ts" || item.Line != 88 {
			t.Fatalf("finding = %+v, want an ask-user ci-review-bot warning anchored to the comment", item)
		}
		joined := strings.Join(*logs, "\n")
		if strings.Contains(joined, ciChecksPassedMsg) || *everReady {
			t.Fatalf("logs = %v, checks-passed must not be reported over an unresolved bot comment", *logs)
		}
		if !strings.Contains(joined, "issues detected: review bot comments need a decision (1 finding)") {
			t.Fatalf("logs = %v, want the observation logged", *logs)
		}
	})
	t.Run("on_failure", func(t *testing.T) {
		t.Parallel()
		env := append(fakeCIGH(t, "OPEN", fastChecksJSON), greenGreptileCommentJSON)
		sctx, _, logs, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
		step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}

		outcome, err := step.Execute(sctx)
		if err == nil || outcome != nil {
			t.Fatalf("outcome = %#v, err = %v, want monitoring to continue until the poll is cancelled", outcome, err)
		}
		if !strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) {
			t.Fatalf("logs = %v, want checks-passed reported", *logs)
		}
	})
}

// A trusted no_ci head with no checks at all is ready only after the same
// comment read: under always an unresolved review-bot comment withholds the
// no-checks readiness and parks as an ask-user finding; under the default the
// declared no-CI head is ready as before.
func TestCIStep_NoCIZeroChecksReviewBotCommentsFollowThePolicy(t *testing.T) {
	t.Parallel()
	t.Run("always", func(t *testing.T) {
		t.Parallel()
		env := append(fakeCIGH(t, "OPEN", "[]"), greenGreptileCommentJSON)
		sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
		sctx.Config.NoCI = true
		step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
			t.Fatal("an unresolved review-bot comment must park, not keep polling")
			return nil
		}}

		outcome, err := driveCI(t, step, sctx)
		if err != nil {
			t.Fatalf("CI step returned error: %v", err)
		}
		if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
			t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
		}
		if len(ag.calls) != 0 {
			t.Fatalf("agent invocations = %d, want none", len(ag.calls))
		}
		findings, err := types.ParseFindingsJSON(outcome.Findings)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings.Items) != 1 {
			t.Fatalf("findings = %+v, want one per unresolved Greptile comment", findings.Items)
		}
		item := findings.Items[0]
		if item.Action != types.ActionAskUser || item.Category != types.FindingCategoryCIReviewBot || item.File != "src/commands/api.test.ts" || item.Line != 88 {
			t.Fatalf("finding = %+v, want an ask-user ci-review-bot warning anchored to the comment", item)
		}
		if strings.Contains(strings.Join(*logs, "\n"), ciNoChecksPassedMsg) || *everReady {
			t.Fatalf("logs = %v, no-checks readiness must not be reported over an unresolved bot comment", *logs)
		}
	})
	t.Run("on_failure", func(t *testing.T) {
		t.Parallel()
		env := append(fakeCIGH(t, "OPEN", "[]"), greenGreptileCommentJSON)
		sctx, _, logs, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
		sctx.Config.NoCI = true
		step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}

		outcome, err := step.Execute(sctx)
		if err == nil || outcome != nil {
			t.Fatalf("outcome = %#v, err = %v, want monitoring to continue until the poll is cancelled", outcome, err)
		}
		if !strings.Contains(strings.Join(*logs, "\n"), ciNoChecksPassedMsg) {
			t.Fatalf("logs = %v, want the no-checks readiness reported", *logs)
		}
	})
}

// The default (on_failure, or the key unset) keeps today's behavior: a green
// review-bot check is green, whatever comments it left.
func TestCIStep_GreenReviewBotCommentsIgnoredByDefault(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"", config.CIReviewBotCommentsOnFailure} {
		t.Run("policy="+policy, func(t *testing.T) {
			t.Parallel()
			env := append(fakeCIGH(t, "OPEN", greenGreptileChecksJSON), greenGreptileCommentJSON)
			sctx, ag, logs, _, _ := newGreenReviewBotContext(t, policy, env)
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}

			outcome, err := step.Execute(sctx)
			if err == nil || outcome != nil {
				t.Fatalf("outcome = %#v, err = %v, want monitoring to continue until the poll is cancelled", outcome, err)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("agent invocations = %d, want none", len(ag.calls))
			}
			if !strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) {
				t.Fatalf("logs = %v, want checks-passed reported", *logs)
			}
		})
	}
}

// A green review bot that left nothing unresolved is green under always too.
func TestCIStep_GreenReviewBotWithoutCommentsPassesUnderAlways(t *testing.T) {
	t.Parallel()
	env := fakeCIGH(t, "OPEN", greenGreptileChecksJSON)
	sctx, _, logs, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}

	outcome, err := step.Execute(sctx)
	if err == nil || outcome != nil {
		t.Fatalf("outcome = %#v, err = %v, want monitoring to continue until the poll is cancelled", outcome, err)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) {
		t.Fatalf("logs = %v, want checks-passed reported", *logs)
	}
}

// Under always, a comment list that cannot be read is not evidence of no
// comments: checks-passed is withheld, and a read that keeps failing parks for
// a decision instead of spinning until ci_timeout.
func TestCIStep_GreenReviewBotUnreadableCommentsParkUnderAlways(t *testing.T) {
	t.Parallel()
	env := append(fakeCIGH(t, "OPEN", greenGreptileChecksJSON), `FAKE_CLI_REVIEW_COMMENTS=not-json`)
	sctx, ag, logs, everReady, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	polls := 0
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		polls++
		if polls > consecutiveCheckErrorLimit {
			t.Fatal("an unreadable comment list must park after the consecutive-error limit")
		}
		return nil
	}}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent invocations = %d, want none", len(ag.calls))
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, "could not be read") {
		t.Fatalf("findings = %+v, want one ask-user read-failure finding", findings.Items)
	}
	if strings.Contains(strings.Join(*logs, "\n"), ciChecksPassedMsg) || *everReady {
		t.Fatalf("logs = %v, checks-passed must not be reported over comments that were never read", *logs)
	}
}

// When a job failure and a green review bot share a settled observation, the
// bot's unresolved comments ride along as ask-user findings under always, and
// are left out under the default.
func TestCIObservationFindings_GreenReviewBotCommentsFollowThePolicy(t *testing.T) {
	t.Parallel()
	issues := ciIssues{
		provider: scm.ProviderGitHub,
		checks: []scm.Check{
			{Name: "test", Bucket: scm.CheckBucketFail, State: "FAILURE", App: "github-actions"},
			{Name: "Greptile Review", Bucket: scm.CheckBucketPass, State: "SUCCESS", App: "greptile-apps"},
		},
		failing: []string{"test"},
		reruns:  func(string) int { return 0 },
		botComments: []scm.ReviewComment{
			{ID: "1", Author: "greptile-apps[bot]", Path: "a.go", Line: 3, Body: "off by one"},
		},
	}

	off := ciObservationFindings(issues)
	if len(off.Items) != 1 || off.Items[0].Category != types.FindingCategoryCICheck {
		t.Fatalf("on_failure findings = %+v, want only the job failure", off.Items)
	}
	if strings.Contains(off.Summary, "review bot") {
		t.Fatalf("on_failure summary = %q, want no review-bot part", off.Summary)
	}

	issues.greenBots = true
	on := ciObservationFindings(issues)
	if len(on.Items) != 2 {
		t.Fatalf("always findings = %+v, want the job failure and the bot comment", on.Items)
	}
	bot := on.Items[1]
	if bot.Category != types.FindingCategoryCIReviewBot || bot.Action != types.ActionAskUser || bot.File != "a.go" || bot.Line != 3 {
		t.Fatalf("bot finding = %+v, want an ask-user finding anchored to the comment", bot)
	}
	outcome := ciObservationOutcome(on)
	if !outcome.AutoFixable || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the job failure still auto-fixable", outcome)
	}

	// A green bot with nothing unresolved produces nothing at all, unlike a
	// red one, which always needs a decision.
	issues.botComments = nil
	quiet := ciObservationFindings(issues)
	if len(quiet.Items) != 1 || strings.Contains(quiet.Summary, "review bot") {
		t.Fatalf("findings = %+v summary = %q, want only the job failure", quiet.Items, quiet.Summary)
	}
}

// A still-running review bot is never read under always: its review may still
// be posting, so only a completed check counts.
func TestGreenReviewBotChecksExcludesPendingAndFailing(t *testing.T) {
	t.Parallel()
	got := greenReviewBotChecks([]scm.Check{
		{Name: "Greptile pending", Bucket: scm.CheckBucketPending, App: "greptile-apps"},
		{Name: "Greptile red", Bucket: scm.CheckBucketFail, App: "greptile-apps"},
		{Name: "Greptile cancelled", Bucket: scm.CheckBucketCancel, App: "greptile-apps"},
		{Name: "build", Bucket: scm.CheckBucketPass, App: "github-actions"},
		{Name: "Greptile green", Bucket: scm.CheckBucketPass, App: "greptile-apps"},
		{Name: "Greptile neutral", Bucket: scm.CheckBucketSkip, App: "greptile-apps"},
	})
	var names []string
	for _, check := range got {
		names = append(names, check.check.Name)
	}
	if strings.Join(names, ",") != "Greptile green,Greptile neutral" {
		t.Fatalf("green review-bot checks = %v", names)
	}
}

// Approving a CI gate over a green bot's unresolved comments is an override
// under always, never a silent clean pass; under the default it is a clean
// pass as before.
func TestCIStep_VerifyApprovalOverride_GreenReviewBotComments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		policy         string
		wantUnresolved bool
	}{
		{policy: config.CIReviewBotCommentsAlways, wantUnresolved: true},
		{policy: config.CIReviewBotCommentsOnFailure, wantUnresolved: false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			t.Parallel()
			env := append(fakeCIGH(t, "OPEN", greenGreptileChecksJSON), greenGreptileCommentJSON)
			sctx, _, _, _, _ := newGreenReviewBotContext(t, tc.policy, env)

			unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantUnresolved && !strings.Contains(unresolved, "Greptile Review") {
				t.Fatalf("unresolved = %q, want a reason naming the review bot check", unresolved)
			}
			if !tc.wantUnresolved && unresolved != "" {
				t.Fatalf("unresolved = %q, want a clean pass", unresolved)
			}
		})
	}
}

// A job failure observed next to a green review bot: under always the step's
// settled observation reads the bot's comments and carries them as ask-user
// findings beside the auto-fix job failure; under the default it carries only
// the job failure.
func TestCIStep_JobFailureWithGreenReviewBotFollowsThePolicy(t *testing.T) {
	t.Parallel()
	const checksJSON = `[{"name":"build","state":"FAILURE","bucket":"fail","app":"github-actions"},{"name":"Greptile Review","state":"SUCCESS","bucket":"pass","app":"greptile-apps","link":"https://greptile.com/"}]`
	for _, tc := range []struct {
		policy   string
		wantBots int
	}{
		{policy: config.CIReviewBotCommentsAlways, wantBots: 1},
		{policy: config.CIReviewBotCommentsOnFailure, wantBots: 0},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			t.Parallel()
			env := append(fakeCIGH(t, "OPEN", checksJSON), greenGreptileCommentJSON)
			sctx, ag, _, _, _ := newGreenReviewBotContext(t, tc.policy, env)
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}

			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatalf("observation error: %v", err)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("agent invocations = %d, want none on the observation", len(ag.calls))
			}
			if outcome == nil || !outcome.AutoFixable {
				t.Fatalf("outcome = %#v, want the job failure auto-fixable", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			bots := 0
			for _, item := range findings.Items {
				if item.Category == types.FindingCategoryCIReviewBot {
					bots++
					if item.Action != types.ActionAskUser || item.File != "src/commands/api.test.ts" || item.Line != 88 {
						t.Fatalf("bot finding = %+v, want an ask-user finding anchored to the comment", item)
					}
				}
			}
			if bots != tc.wantBots || len(findings.Items) != 1+tc.wantBots {
				t.Fatalf("findings = %+v, want the job failure and %d review-bot finding(s)", findings.Items, tc.wantBots)
			}
		})
	}
}

// gitlabReviewCommentHost stands in for the GitLab adapter's read half: it
// declares the capability GitLab now does and answers with the login the bot
// comments under there.
type gitlabReviewCommentHost struct {
	scm.Host
	calls    int
	comments []scm.ReviewComment
}

func (h *gitlabReviewCommentHost) Provider() scm.Provider { return scm.ProviderGitLab }

func (h *gitlabReviewCommentHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true, ReviewComments: true}
}

func (h *gitlabReviewCommentHost) GetReviewComments(context.Context, *scm.PR) ([]scm.ReviewComment, error) {
	h.calls++
	return h.comments, nil
}

// On GitLab a review bot is identified by the login it comments as, and only
// the comment half exists: no job names a publishing application, so no check
// can ever be attributed to a bot. ci.review_bot_comments: always therefore
// reads the bot's unresolved discussions through the green/not-yet-registered
// path, and on_failure never consults the reader at all.
func TestReviewBotCommentsOnGitLabFollowThePolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		policy   string
		wantRead bool
	}{
		{name: "always reads the bot's discussions", policy: config.CIReviewBotCommentsAlways, wantRead: true},
		{name: "on_failure has no bot check to attribute", policy: config.CIReviewBotCommentsOnFailure, wantRead: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx := &pipeline.StepContext{
				Ctx:    context.Background(),
				Config: &config.Config{CI: config.CI{ReviewBotComments: tc.policy}},
				Log:    func(string) {},
			}
			host := &gitlabReviewCommentHost{comments: []scm.ReviewComment{{
				ID:     "1126",
				Author: "greptileai",
				Path:   "internal/app.go",
				Line:   42,
				Body:   "This retry loop can spin forever",
			}}}
			pr := &scm.PR{Number: "42", URL: "https://gitlab.com/test/repo/-/merge_requests/42"}
			checks := []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, ProviderID: "gitlab-job:1"}}

			findings, err := (&CIStep{}).greenReviewBotFindings(sctx, host, pr, checks)
			if err != nil {
				t.Fatalf("greenReviewBotFindings() error = %v", err)
			}
			if tc.wantRead {
				if host.calls == 0 {
					t.Fatal("the review-comment reader was never consulted under ci.review_bot_comments: always")
				}
				if len(findings.Items) != 1 {
					t.Fatalf("findings = %+v, want one per unresolved bot discussion note", findings.Items)
				}
				item := findings.Items[0]
				if item.Category != types.FindingCategoryCIReviewBot || item.Action != types.ActionAskUser || item.File != "internal/app.go" || item.Line != 42 {
					t.Fatalf("finding = %+v, want an ask-user ci-review-bot finding anchored to the note", item)
				}
				if !strings.Contains(item.Description, "greptileai") {
					t.Fatalf("finding = %+v, want the GitLab comment-author login", item)
				}
				return
			}
			if host.calls != 0 {
				t.Fatalf("reader calls = %d, want none: on_failure has no GitLab bot check to attribute comments to", host.calls)
			}
			if len(findings.Items) != 0 {
				t.Fatalf("findings = %+v, want none", findings.Items)
			}
		})
	}
}
