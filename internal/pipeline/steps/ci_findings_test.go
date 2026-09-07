package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// driveCI executes the CI step the way the executor does: an observation,
// then one fix round per auto-fix observation while auto_fix.ci allows.
func driveCI(t *testing.T, step *CIStep, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	t.Helper()
	return stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
}

// ciGateFindingsJSON is the findings a human's fix selection hands back for
// the named failing checks.
func ciGateFindingsJSON(names ...string) string {
	return stepstest.CIGateFindingsJSON(names...)
}

// ciTargetsFor builds the fix targets a CI fix round would receive for the
// named failing checks and an optional merge conflict, the way the executor
// hands them over as the previous observation's auto-fix findings.
func ciTargetsFor(names []string, mergeConflict bool) ciFixTargets {
	findings := Findings{Summary: "CI issues"}
	for i, name := range names {
		findings.Items = append(findings.Items, Finding{
			ID:          "ci-" + itoaTest(i+1),
			Severity:    types.FindingSeverityError,
			Action:      types.ActionAutoFix,
			Category:    types.FindingCategoryCICheck,
			Check:       name,
			Description: "CI check failing: " + name,
		})
	}
	if mergeConflict {
		findings.Items = append(findings.Items, Finding{
			ID:          "ci-conflict",
			Severity:    types.FindingSeverityError,
			Action:      types.ActionAutoFix,
			Category:    types.FindingCategoryCIMergeConflict,
			Description: "PR has merge conflicts with the base branch",
		})
	}
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		panic(err)
	}
	targets, err := parseCIFixTargets(encoded)
	if err != nil {
		panic(err)
	}
	return targets
}

// The classifier reads provider structure only. Every row here is one kind of
// settled issue and the action the executor's shared machinery must see.
func TestCIObservationFindings_ClassifiesEachIssueByProviderStructure(t *testing.T) {
	t.Parallel()
	findings := ciObservationFindings(ciIssues{
		checks: []scm.Check{
			{Name: "test", Bucket: scm.CheckBucketFail, State: "FAILURE", App: "github-actions", Link: "https://github.com/test/repo/actions/runs/1/job/2"},
			{Name: "ci/external", Bucket: scm.CheckBucketFail, State: "FAILURE", Kind: scm.CheckKindStatus},
			{Name: "Greptile Review", Bucket: scm.CheckBucketFail, State: "FAILURE", App: "greptile-apps", Link: "https://greptile.com/"},
			{Name: "flaky", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
		},
		failing:             []string{"Greptile Review", "ci/external", "test"},
		unresolvedCancelled: []string{"flaky"},
		mergeConflict:       true,
		reruns:              func(string) int { return 0 },
		botComments: []scm.ReviewComment{
			{Author: "greptile-apps[bot]", Path: "internal/a.go", Line: 12, Body: "nil pointer on the error path"},
			{Author: "octocat", Path: "internal/b.go", Line: 3, Body: "a human comment the bot did not write"},
		},
	})

	byKey := map[string]Finding{}
	for _, item := range findings.Items {
		byKey[item.Category+"/"+item.Check] = item
	}
	for _, tc := range []struct {
		key      string
		action   string
		severity string
	}{
		{key: types.FindingCategoryCICheck + "/test", action: types.ActionAutoFix, severity: types.FindingSeverityError},
		{key: types.FindingCategoryCICheck + "/ci/external", action: types.ActionAutoFix, severity: types.FindingSeverityError},
		{key: types.FindingCategoryCIMergeConflict + "/", action: types.ActionAutoFix, severity: types.FindingSeverityError},
		{key: types.FindingCategoryCIReviewBot + "/Greptile Review", action: types.ActionAskUser, severity: types.FindingSeverityWarning},
		{key: types.FindingCategoryCITransient + "/flaky", action: types.ActionAskUser, severity: types.FindingSeverityWarning},
	} {
		item, ok := byKey[tc.key]
		if !ok {
			t.Fatalf("no finding for %s in %+v", tc.key, findings.Items)
		}
		if item.Action != tc.action || item.Severity != tc.severity {
			t.Fatalf("%s = %+v, want action %s severity %s", tc.key, item, tc.action, tc.severity)
		}
	}
	if len(findings.Items) != 5 {
		t.Fatalf("findings = %+v, want exactly one per issue (the human comment is not the bot's)", findings.Items)
	}
	bot := byKey[types.FindingCategoryCIReviewBot+"/Greptile Review"]
	if bot.File != "internal/a.go" || bot.Line != 12 || !strings.Contains(bot.Description, "nil pointer on the error path") {
		t.Fatalf("review bot finding = %+v, want the comment's file, line, and body", bot)
	}
	if !strings.Contains(byKey[types.FindingCategoryCICheck+"/test"].Description, "https://github.com/test/repo/actions/runs/1/job/2") {
		t.Fatalf("check finding = %+v, want the details link", byKey[types.FindingCategoryCICheck+"/test"])
	}
	for _, want := range []string{"2 CI checks failing", "PR has merge conflicts with the base branch", "review bot check Greptile Review needs a decision (1 finding)", "CI checks were cancelled without reporting a verdict"} {
		if !strings.Contains(findings.Summary, want) {
			t.Fatalf("summary = %q, want it to mention %q", findings.Summary, want)
		}
	}

	outcome := ciObservationOutcome(findings)
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want blocking and auto-fixable", outcome)
	}
	targets, err := parseCIFixTargets(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !targets.MergeConflict || strings.Join(targets.checkNames(), ",") != "Greptile Review,ci/external,flaky,test" {
		t.Fatalf("targets = %+v, want every check named once plus the conflict", targets)
	}
}

func TestCIObservationFindings_PreservesSameNamedCheckIdentityAndClassification(t *testing.T) {
	t.Parallel()
	findings := ciObservationFindings(ciIssues{
		checks: []scm.Check{
			{Name: "build", ProviderID: "github-check-run:41", Bucket: scm.CheckBucketFail, App: "github-actions"},
			{Name: "build", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, App: "greptile-apps"},
		},
		failing: []string{"build", "build"},
	})
	if len(findings.Items) != 2 {
		t.Fatalf("findings = %+v, want one finding per failed check", findings.Items)
	}
	if findings.Items[0].CheckID != "github-check-run:41" || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("first finding = %+v, want the Actions check identity and auto-fix", findings.Items[0])
	}
	if findings.Items[1].CheckID != "github-check-run:42" || findings.Items[1].Action != types.ActionAskUser {
		t.Fatalf("second finding = %+v, want the review-bot identity and ask-user", findings.Items[1])
	}

	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := parseCIFixTargets(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets.Checks) != 2 || targets.Checks[0].ProviderID != "github-check-run:41" || targets.Checks[1].ProviderID != "github-check-run:42" {
		t.Fatalf("targets = %+v, want both same-named provider identities", targets.Checks)
	}
}

// A red review-bot check with no unresolved comment still needs a decision; a
// bot that left more comments than one gate can carry is summarized.
func TestReviewBotFindings_BoundsAndEmptyCase(t *testing.T) {
	t.Parallel()
	check := scm.Check{Name: "Greptile Review", Bucket: scm.CheckBucketFail, App: "greptile-apps", Link: "https://greptile.com/"}
	bot, _ := scm.ReviewBotForApp(check.App)

	empty := reviewBotFindings(check, bot, nil)
	if len(empty) != 1 || empty[0].Action != types.ActionAskUser || empty[0].Check != check.Name || !strings.Contains(empty[0].Description, "no unresolved review comments") {
		t.Fatalf("empty-case findings = %+v, want one ask-user finding for the check", empty)
	}

	var many []scm.ReviewComment
	for i := 0; i < maxReviewBotCommentFindings+7; i++ {
		many = append(many, scm.ReviewComment{Author: "greptile-apps[bot]", Path: "a.go", Line: i + 1, Body: strings.Repeat("x", maxReviewBotCommentBytes+100)})
	}
	bounded := reviewBotFindings(check, bot, many)
	if len(bounded) != maxReviewBotCommentFindings+1 {
		t.Fatalf("got %d findings, want %d comment findings plus one omission notice", len(bounded), maxReviewBotCommentFindings+1)
	}
	if len(bounded[0].Description) > maxReviewBotCommentBytes+64 {
		t.Fatalf("comment description was not bounded: %d bytes", len(bounded[0].Description))
	}
	if !strings.Contains(bounded[len(bounded)-1].Description, "7 more unresolved review comments") {
		t.Fatalf("last finding = %+v, want the omission count", bounded[len(bounded)-1])
	}
}

func TestCIRepairParkOutcome_RelabelsEveryFindingAskUser(t *testing.T) {
	t.Parallel()
	targets := ciTargetsFor([]string{"test"}, true)
	outcome := ciRepairParkOutcome(targets.Findings, "attestation failure is external to the PR code")
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a park the auto-fix loop cannot re-enter", outcome)
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Summary != "attestation failure is external to the PR code" || len(parsed.Items) != 2 {
		t.Fatalf("parked = %+v, want the summary and every selected finding", parsed)
	}
	for _, item := range parsed.Items {
		if item.Action != types.ActionAskUser || item.ID == "" {
			t.Fatalf("parked item = %+v, want ask-user with its id kept", item)
		}
	}
	if types.HasAskUserFindings(parsed) != true || len(types.AutoFixableFindings(parsed).Items) != 0 {
		t.Fatal("parked findings must be ask-user only")
	}
}

// The first concrete case of the findings model: a red Greptile check is the
// bot's opinion, not a verdict, so it parks as ask-user findings anchored to
// each comment and never spends an auto_fix.ci round, even with budget left.
func TestCIStep_GreptileOnlyRedParksAsAskUserAndSpendsNoAutoFixRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"},{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps","link":"https://greptile.com/"}]`
	env := append(fakeCIGH(t, "OPEN", checksJSON),
		`FAKE_CLI_REVIEW_COMMENTS=[{"author":"greptile-apps[bot]","path":"internal/pipeline/steps/push.go","line":155,"body":"Missing mirror reports success"},{"author":"greptile-apps[bot]","path":"internal/pipeline/steps/pr.go","line":40,"body":"Body is replaced instead of merged"}]`)

	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("a settled review-bot red must park, not keep polling")
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
		t.Fatalf("agent invocations = %d, want none: a review bot's verdict never spends an auto_fix.ci round", len(ag.calls))
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("head moved to %s without a fix round", got)
	}
	if step.TransientRerunRecorded("Greptile Review") {
		t.Fatal("a review bot check must not enter the transient rerun path")
	}

	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 2 {
		t.Fatalf("findings = %+v, want one per unresolved Greptile comment", findings.Items)
	}
	wantFiles := map[string]int{"internal/pipeline/steps/push.go": 155, "internal/pipeline/steps/pr.go": 40}
	for _, item := range findings.Items {
		if item.Action != types.ActionAskUser || item.Category != types.FindingCategoryCIReviewBot || item.Check != "Greptile Review" {
			t.Fatalf("finding = %+v, want an ask-user ci-review-bot finding for Greptile Review", item)
		}
		line, ok := wantFiles[item.File]
		if !ok || item.Line != line {
			t.Fatalf("finding = %+v, want it anchored to the comment's file and line", item)
		}
		if !strings.Contains(item.Description, "greptile-apps[bot]") {
			t.Fatalf("finding = %+v, want the bot named as the comment's author", item)
		}
	}
	if !strings.Contains(findings.Summary, "review bot check Greptile Review needs a decision (2 findings)") {
		t.Fatalf("summary = %q", findings.Summary)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "issues detected: review bot check Greptile Review needs a decision") {
		t.Fatalf("logs = %v, want the observation logged", logs)
	}
}

// A real job failure next to a red review-bot check: only the job failure is
// handed to the fix round, and the round still sees the bot's comments as
// untrusted context, exactly as before.
func TestCIStep_MixedGreptileAndTestFailureRoutesOnlyTheTestToAutoFix(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","state":"FAILURE","bucket":"fail","app":"github-actions","link":"https://github.com/test/repo/actions/runs/1/job/2"},{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps","link":"https://greptile.com/"}]`
	env := append(fakeCIGH(t, "OPEN", checksJSON),
		`FAKE_CLI_REVIEW_COMMENTS=[{"author":"greptile-apps[bot]","path":"feature.txt","line":1,"body":"Missing mirror reports success"}]`)

	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair the failing test","code_change_needed":true}`)}, nil
	}}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI.RevalidateRepairs = true
	sctx.Log = func(string) {}

	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}
	first, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("observation error: %v", err)
	}
	parsed, err := types.ParseFindingsJSON(first.Findings)
	if err != nil {
		t.Fatal(err)
	}
	fixable := types.AutoFixableFindings(parsed)
	if len(fixable.Items) != 1 || fixable.Items[0].Check != "test" || len(parsed.Items) != 2 {
		t.Fatalf("observation = %+v, want the test failure auto-fix and the Greptile comment ask-user", parsed.Items)
	}

	outcome, err := driveCI(t, step, sctx)
	assertCIRestartsValidation(t, outcome, err)
	if len(prompts) != 1 {
		t.Fatalf("agent invocations = %d, want exactly one round for the test failure", len(prompts))
	}
	if !strings.Contains(prompts[0], "- failing checks: test\n") {
		t.Fatalf("prompt names %q as failing checks, want only test:\n%s", "failing checks", prompts[0])
	}
	if strings.Contains(prompts[0], "Missing mirror reports success") {
		t.Fatalf("prompt contains the unselected review-bot finding:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[0], "Findings to address") || !strings.Contains(prompts[0], `"check":"test"`) {
		t.Fatalf("prompt lost the selected findings:\n%s", prompts[0])
	}
	if outcome.FixSummary != "repair the failing test" {
		t.Fatalf("FixSummary = %q, want the agent's summary carried on the round", outcome.FixSummary)
	}
}

// A published repair keeps every cost guardrail: one agent round, one push,
// the step reports it is monitoring again, and the next poll that still
// shows the repaired check waits for the provider instead of re-escalating.
func TestCIStep_PublishedRepairPropagatesMarkRunningFailure(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	markErr := errors.New("persist running status")
	f.sctx.MarkRunning = func() error { return markErr }

	outcome, err := f.run(t)
	if outcome != nil || !errors.Is(err, markErr) {
		t.Fatalf("outcome = %#v err = %v, want MarkRunning failure", outcome, err)
	}
}

func TestCIStep_PublishedRepairResumesMonitoringAndWaitsForTheRerun(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	marked := 0
	f.sctx.MarkRunning = func() error { marked++; return nil }

	outcome, err := f.run(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome = %#v err = %v, want the monitor still polling when the poll budget cancels it\nlog:\n%s", outcome, err, f.log())
	}
	if marked != 1 {
		t.Fatalf("MarkRunning calls = %d, want exactly one after the repair was published", marked)
	}
	if f.remoteHead(t) == f.headSHA || f.remoteHead(t) != f.localHead(t) {
		t.Fatalf("repair not published: local=%s remote=%s original=%s", f.localHead(t), f.remoteHead(t), f.headSHA)
	}
	if calls := len(f.sctx.Agent.(*mockAgent).calls); calls != 1 {
		t.Fatalf("agent invocations = %d, want one", calls)
	}
	if !strings.Contains(f.log(), "fix already attempted for these issues, waiting for CI re-run") {
		t.Fatalf("the poll after the push must wait for the provider, log:\n%s", f.log())
	}
	if strings.Count(f.log(), "issues detected:") != 1 {
		t.Fatalf("a stale poll must not be re-escalated as a new observation, log:\n%s", f.log())
	}
	if f.sctx.Fixing != true {
		t.Fatal("the driver must have entered a fix round")
	}
}

// A transient check parks as ask-user, and when the human selects it and
// answers fix, the round is told about that check.
func TestCIStep_UserFixOfTransientFindingIsHonored(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	checksJSON := `[{"name":"flaky","state":"CANCELLED","bucket":"cancel","app":"github-actions"}]`
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{Output: json.RawMessage(`{"summary":"nothing to change","code_change_needed":false}`)}, nil
	}}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeCIGH(t, "OPEN", checksJSON)
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Log = func(string) {}
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}

	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || len(ag.calls) != 0 {
		t.Fatalf("outcome = %#v calls = %d, want an ask-user park with no round spent", outcome, len(ag.calls))
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].Category != types.FindingCategoryCITransient || parsed.Items[0].Check != "flaky" {
		t.Fatalf("findings = %+v, want one ci-transient finding for flaky", parsed.Items)
	}

	// The human answers fix, selecting the transient finding.
	sctx.Fixing = true
	sctx.PreviousFindings = outcome.Findings
	parked, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "- failing checks: flaky\n") {
		t.Fatalf("prompts = %d, want the selected transient check named in one fix round: %v", len(prompts), prompts)
	}
	if parked == nil || !parked.NeedsApproval || parked.AutoFixable {
		t.Fatalf("outcome = %#v, want the no-change conclusion parked", parked)
	}
}

func itoaTest(v int) string {
	digits := "0123456789"
	if v == 0 {
		return "0"
	}
	var out []byte
	for v > 0 {
		out = append([]byte{digits[v%10]}, out...)
		v /= 10
	}
	return string(out)
}
