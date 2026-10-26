package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		provider: scm.ProviderGitHub,
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
		provider: scm.ProviderGitHub,
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
	bot, _ := scm.ReviewBotForCheck(scm.ReviewBotsFor(nil), check)

	empty := reviewBotFindings(scm.ProviderGitHub, []reviewBotCheck{{check: check, bot: bot}}, nil)
	if len(empty) != 1 || empty[0].Action != types.ActionAskUser || empty[0].Check != check.Name || !strings.Contains(empty[0].Description, "no unresolved review comments") {
		t.Fatalf("empty-case findings = %+v, want one ask-user finding for the check", empty)
	}

	var many []scm.ReviewComment
	for i := 0; i < maxReviewBotCommentFindings+7; i++ {
		many = append(many, scm.ReviewComment{Author: "greptile-apps[bot]", Path: "a.go", Line: i + 1, Body: strings.Repeat("x", maxReviewBotCommentBytes+100)})
	}
	bounded := reviewBotFindings(scm.ProviderGitHub, []reviewBotCheck{{check: check, bot: bot}}, many)
	if len(bounded) > maxReviewBotCommentFindings {
		t.Fatalf("got %d findings, want at most %d including the omission notice", len(bounded), maxReviewBotCommentFindings)
	}
	if len(bounded[0].Description) > maxReviewBotCommentBytes+64 {
		t.Fatalf("comment description was not bounded: %d bytes", len(bounded[0].Description))
	}
	totalBytes := 0
	for _, finding := range bounded {
		raw, _ := json.Marshal(finding)
		totalBytes += len(raw)
	}
	if totalBytes > maxReviewBotObservationBytes {
		t.Fatalf("descriptions use %d bytes, want at most %d", totalBytes, maxReviewBotObservationBytes)
	}
	if !strings.Contains(bounded[len(bounded)-1].Description, "review-bot findings were omitted") {
		t.Fatalf("last finding = %+v, want the omission count", bounded[len(bounded)-1])
	}
}

func TestReviewBotFindings_BoundsEntireObservationAcrossRepeatedChecks(t *testing.T) {
	t.Parallel()
	bot, _ := scm.ReviewBotForCheck(scm.ReviewBotsFor(nil), scm.Check{App: "greptile-apps"})
	checks := make([]reviewBotCheck, 100)
	for i := range checks {
		checks[i] = reviewBotCheck{check: scm.Check{Name: "Greptile Review", ProviderID: fmt.Sprintf("github-check-run:%d", i+1)}, bot: bot}
	}
	comments := make([]scm.ReviewComment, 50)
	for i := range comments {
		comments[i] = scm.ReviewComment{ID: fmt.Sprintf("comment-%d", i), Author: "greptile-apps[bot]", Body: fmt.Sprintf("finding-%d", i)}
	}

	findings := reviewBotFindings(scm.ProviderGitHub, checks, comments)
	if len(findings) > maxReviewBotCommentFindings {
		t.Fatalf("got %d findings across repeated checks, want at most %d", len(findings), maxReviewBotCommentFindings)
	}
	totalBytes := 0
	seen := map[string]bool{}
	for _, finding := range findings {
		raw, _ := json.Marshal(finding)
		totalBytes += len(raw)
		if strings.HasPrefix(finding.Description, "greptile-apps[bot]: finding-") {
			if seen[finding.Description] {
				t.Fatalf("duplicated comment finding %q", finding.Description)
			}
			seen[finding.Description] = true
		}
	}
	if totalBytes > maxReviewBotObservationBytes {
		t.Fatalf("descriptions use %d bytes, want at most %d", totalBytes, maxReviewBotObservationBytes)
	}
	if !strings.Contains(findings[len(findings)-1].Description, "review-bot findings were omitted") {
		t.Fatalf("last finding = %+v, want one aggregate omission notice", findings[len(findings)-1])
	}
}

func TestCIRepairParkOutcome_PreservesDeferredAskUserFindings(t *testing.T) {
	t.Parallel()
	selected := ciTargetsFor([]string{"test"}, false)
	deferred, err := types.MarshalFindingsJSON(Findings{Items: []Finding{{
		ID:          "ci-2",
		Severity:    types.FindingSeverityWarning,
		Action:      types.ActionAskUser,
		Category:    types.FindingCategoryCIReviewBot,
		Check:       "Greptile Review",
		Description: "Greptile review needs a decision",
	}}})
	if err != nil {
		t.Fatal(err)
	}

	outcome := ciRepairParkOutcome(selected.Findings, deferred, "nothing to change")
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 2 || parsed.Items[0].Check != "test" || parsed.Items[1].Check != "Greptile Review" {
		t.Fatalf("parked findings = %+v, want selected and deferred findings", parsed.Items)
	}
	for _, item := range parsed.Items {
		if item.Action != types.ActionAskUser {
			t.Fatalf("parked finding = %+v, want ask-user", item)
		}
	}
}

func TestCIRepairParkOutcome_RelabelsEveryFindingAskUser(t *testing.T) {
	t.Parallel()
	targets := ciTargetsFor([]string{"test"}, true)
	outcome := ciRepairParkOutcome(targets.Findings, "", "attestation failure is external to the PR code")
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
	deferred, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(deferred) {
		t.Fatalf("outcome = %+v, want the deferred Greptile decision to block before restart", outcome)
	}
	if len(deferred.Items) != 1 || deferred.Items[0].Check != "Greptile Review" || deferred.Items[0].Action != types.ActionAskUser {
		t.Fatalf("deferred findings = %+v, want only the Greptile ask-user finding", deferred.Items)
	}
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
	if !outcome.RepairPublished {
		t.Fatal("published repair outcome did not carry durable repair evidence")
	}
}

// A published repair keeps every cost guardrail: one agent round, one push,
// the step reports it is monitoring again, and the next poll that still
// shows the repaired check waits for the provider instead of re-escalating.
func TestCIResumedRepairSnapshotsSelectedCheckCompletion(t *testing.T) {
	t.Parallel()

	dir, baseSHA, headSHA := setupGitRepo(t)
	completed := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"no code change","code_change_needed":false}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"}]}`
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: completed}}}
	step := &CIStep{}

	outcome, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("repair outcome = %#v, err = %v", outcome, err)
	}
	if host.headSHA != headSHA {
		t.Fatalf("snapshot PR head = %q, want expected run head %q", host.headSHA, headSHA)
	}
	if got := step.observedCompletedAt["id:github-check-run:42"].CompletedAt; !got.Equal(completed) {
		t.Fatalf("snapshotted completion = %v, want %v", got, completed)
	}
}

func TestCIResumedRepairEmptySnapshotSkipsRepairAndPreservesCompletionMap(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("fix agent ran without a selected failing check in the fresh snapshot")
		return nil, errors.New("unexpected fixer invocation")
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"}]}`
	prior := map[string]checkFreshness{"id:github-check-run:42": {CompletedAt: time.Now()}}
	step := &CIStep{observedCompletedAt: prior}
	host := &completionSnapshotHost{}
	outcome, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if err != nil || outcome != nil {
		t.Fatalf("repair = %#v, %v; want monitoring to resume", outcome, err)
	}
	if host.headSHA != headSHA {
		t.Fatalf("snapshot head = %q, want current head %q", host.headSHA, headSHA)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want none", len(ag.calls))
	}
	if got := step.observedCompletedAt["id:github-check-run:42"]; !got.CompletedAt.Equal(prior["id:github-check-run:42"].CompletedAt) {
		t.Fatalf("completion map entry = %+v, want prior value preserved", got)
	}
}

func TestCIResumedRepairPartialSnapshotSkipsSelectedChecks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("fix agent ran with only a subset of the selected checks present")
		return nil, errors.New("unexpected fixer invocation")
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"},{"id":"ci-2","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"lint","check_id":"github-check-run:43"}]}`
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail}}}
	step := &CIStep{}
	outcome, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if err != nil || outcome != nil {
		t.Fatalf("repair = %#v, %v; want monitoring to resume", outcome, err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want none until all selected failures are confirmed", len(ag.calls))
	}
}

func TestCIResumedRepairAdapterObservedStaleHeadWaitsInsteadOfRepairing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("fix agent ran against a snapshot of another head")
		return nil, errors.New("unexpected fixer invocation")
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
	host := &completionSnapshotHost{observedHead: "other-head", checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}}
	outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if err != nil || outcome != nil {
		t.Fatalf("repair = %#v, %v; want monitoring to resume", outcome, err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want none", len(ag.calls))
	}
}

func TestCIResumedRepairStaleHeadErrorWaitsInsteadOfFailing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
	host := &completionSnapshotHost{err: scm.ErrHeadChanged}
	outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if err != nil || outcome != nil || len(ag.calls) != 0 {
		t.Fatalf("repair = %#v, %v, agent calls %d; want a wait", outcome, err, len(ag.calls))
	}
}

// A merge conflict is observed from mergeability, not from checks, so an empty
// check snapshot must not stop its repair.
func TestCIResumedRepairRejectedChecksStillRepairsRebase(t *testing.T) {
	t.Parallel()
	for _, category := range []string{types.FindingCategoryCIMergeConflict, types.FindingCategoryCIBehindBase} {
		for _, snapshot := range []string{"empty", "partial", "stale-head", "head-error"} {
			t.Run(category+"/"+snapshot, func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, headSHA := setupGitRepo(t)
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(`{"summary":"no code change","code_change_needed":false}`)}, nil
				}}
				sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
				selected := Findings{Summary: "rejected-check-marker", Items: []Finding{
					{ID: "check-1", Severity: types.FindingSeverityError, Category: types.FindingCategoryCICheck, Check: "test", Description: "rejected-check-marker"},
					{ID: "check-2", Severity: types.FindingSeverityError, Category: types.FindingCategoryCICheck, Check: "lint", Description: "rejected-check-marker"},
					{ID: "rebase", Severity: types.FindingSeverityError, Category: category, Description: "selected-rebase-marker"},
				}}
				encoded, err := json.Marshal(selected)
				if err != nil {
					t.Fatal(err)
				}
				sctx.PreviousFindings = string(encoded)
				host := &completionSnapshotHost{}
				switch snapshot {
				case "partial":
					host.checks = []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}
				case "stale-head":
					host.observedHead = "other-head"
				case "head-error":
					host.err = scm.ErrHeadChanged
				}
				outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "42"})
				if err != nil {
					t.Fatal(err)
				}
				if len(ag.calls) != 1 {
					t.Fatalf("agent calls = %d, want one rebase repair", len(ag.calls))
				}
				prompt := ag.calls[0].Prompt
				if strings.Contains(prompt, "rejected-check-marker") || strings.Contains(prompt, `"check":"test"`) || strings.Contains(prompt, `"check":"lint"`) {
					t.Fatalf("prompt includes rejected check findings:\n%s", prompt)
				}
				if !strings.Contains(prompt, "Findings to address") || !strings.Contains(prompt, "selected-rebase-marker") {
					t.Fatalf("prompt lost selected rebase finding:\n%s", prompt)
				}
				if outcome != nil && strings.Contains(outcome.Findings, "rejected-check-marker") {
					t.Fatalf("repair outcome retained rejected check: %#v", outcome)
				}
			})
		}
	}
}

func TestCIResumedRepairGreenReviewBotCommentReachesFixer(t *testing.T) {
	t.Parallel()
	for _, withRejectedCheck := range []bool{false, true} {
		t.Run(fmt.Sprint(withRejectedCheck), func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"summary":"human decision needed","code_change_needed":false}`)}, nil
			}}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			checks := []scm.Check{{Name: "Greptile Review", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketPass, App: "greptile-apps"}}
			selected := ciObservationFindings(ciIssues{provider: scm.ProviderGitHub, checks: checks, greenBots: true, botComments: []scm.ReviewComment{
				{Author: "greptile-apps[bot]", Path: "feature.txt", Line: 1, Body: "selected-bot-comment"},
			}})
			if len(selected.Items) != 1 || selected.Items[0].CheckID != "github-check-run:42" {
				t.Fatalf("green comment finding = %+v", selected.Items)
			}
			if withRejectedCheck {
				selected.Items = append(selected.Items, Finding{ID: "failed", Category: types.FindingCategoryCICheck, Check: "test", Description: "rejected-check-marker"})
			}
			encoded, err := json.Marshal(selected)
			if err != nil {
				t.Fatal(err)
			}
			sctx.PreviousFindings = string(encoded)
			host := &completionSnapshotHost{checks: checks}
			outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "42"})
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want selected green bot comment repaired", len(ag.calls))
			}
			if !strings.Contains(ag.calls[0].Prompt, "selected-bot-comment") || strings.Contains(ag.calls[0].Prompt, "rejected-check-marker") {
				t.Fatalf("incorrect selected findings in prompt:\n%s", ag.calls[0].Prompt)
			}
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("outcome = %#v, want no-change decision parked", outcome)
			}
		})
	}
}

type completionSnapshotHost struct {
	scm.Host
	checks       []scm.Check
	headSHA      string
	observedHead string
	err          error
}

func (h *completionSnapshotHost) Capabilities() scm.Capabilities { return scm.Capabilities{} }
func (h *completionSnapshotHost) GetChecks(_ context.Context, pr *scm.PR) ([]scm.Check, error) {
	h.headSHA = pr.HeadSHA
	if h.err != nil {
		return nil, h.err
	}
	if h.observedHead != "" {
		pr.ObservedHeadSHA = h.observedHead
	}
	return h.checks, nil
}

func TestCIStep_PublishedRepairPropagatesMarkRunningFailure(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, writeCIFix)
	markErr := errors.New("persist running status")
	f.sctx.MarkRunning = func() error { return markErr }

	outcome, err := f.run(t)
	if outcome != nil || !errors.Is(err, markErr) {
		t.Fatalf("outcome = %#v err = %v, want MarkRunning failure", outcome, err)
	}
}

func TestCIStep_PublishedRepairResumesMonitoringAndWaitsForTheRerun(t *testing.T) {
	t.Parallel()
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

func TestReviewBotFindingsKeepProviderIdentity(t *testing.T) {
	for _, tc := range []struct {
		provider scm.Provider
		want     string
	}{
		{scm.ProviderGitHub, "github"}, {scm.ProviderGitLab, "gitlab"},
	} {
		got := ciObservationFindings(ciIssues{
			provider:  tc.provider,
			greenBots: true,
			botComments: []scm.ReviewComment{
				{ID: "1", Author: "greptile-apps[bot]", Body: "github"},
				{ID: "2", Author: "greptileai", Body: "gitlab"},
			},
		})
		if len(got.Items) != 1 || !strings.HasSuffix(got.Items[0].Description, ": "+tc.want) {
			t.Fatalf("provider %s findings = %+v", tc.provider, got)
		}
	}
}

func TestCIResumedRepairGitHubMovedHeadReadErrorWaits(t *testing.T) {
	t.Parallel()
	for _, read := range []string{"commit", "workflow", "final-head", "back-to-requested"} {
		t.Run(read, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Error("fixer invoked for another head")
				return nil, errors.New("unexpected fixer")
			}}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.PreviousFindings = `{"findings":[{"id":"check","category":"ci-check","check":"test","description":"failed"}]}`
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","bucket":"fail","state":"FAILURE"}]`)
			for i, entry := range sctx.Env {
				if strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
					sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=head-B"
				}
			}
			switch read {
			case "commit":
				sctx.Env = append(sctx.Env, "FAKE_CLI_CHECKS_ERR=read failed")
			case "workflow":
				sctx.Env = append(sctx.Env, "FAKE_CLI_WORKFLOW_RUNS=invalid-json")
			case "final-head", "back-to-requested":
				heads := []string{headSHA, "head-B"}
				if read == "back-to-requested" {
					heads = []string{"head-B", headSHA}
				}
				sequence, err := json.Marshal(heads)
				if err != nil {
					t.Fatal(err)
				}
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SEQUENCE="+string(sequence), "FAKE_CLI_PR_HEAD_INDEX="+filepath.Join(t.TempDir(), "head-index"))
			}
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			host, reason := buildHost(sctx, scm.ProviderGitHub)
			if host == nil {
				t.Fatal(reason)
			}
			prior := checkFreshness{CompletedAt: time.Unix(1700000000, 0)}
			step := &CIStep{observedCompletedAt: map[string]checkFreshness{"test": prior}}
			outcome, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42", URL: prURL})
			if err != nil || outcome != nil || len(ag.calls) != 0 {
				t.Fatalf("repair = %#v, %v; calls = %d, want wait", outcome, err, len(ag.calls))
			}
			if step.observedCompletedAt["test"] != prior {
				t.Fatal("stale snapshot changed completion evidence")
			}
		})
	}
}

// A required external review that is not complete at the head must name its
// completion state even when stale unresolved comments from the same bot
// already fill the gate.
func TestReviewBotFindings_IncompleteExternalReviewSurvivesStaleComments(t *testing.T) {
	t.Parallel()
	comments := []scm.ReviewComment{
		{ID: "old-1", Author: "coderabbitai[bot]", AuthorID: scm.CodeRabbitAuthorID, Path: "a.go", Line: 1, Body: "stale"},
	}
	for _, state := range []string{"RATE_LIMITED", "INCOMPLETE", "PENDING", "UNRESOLVED_FINDINGS"} {
		check := scm.Check{Name: "External review: coderabbit", ProviderID: "external-review:coderabbit", ExternalReview: "coderabbit", AppID: scm.CodeRabbitAppID, AppHost: "github.com", Bucket: scm.CheckBucketFail, State: state}
		bot, ok := scm.ReviewBotForCheck(scm.ReviewBotsFor([]string{"coderabbit"}), check)
		if !ok {
			t.Fatal("synthetic CodeRabbit check did not resolve to the CodeRabbit bot")
		}
		findings := reviewBotFindings(scm.ProviderGitHub, []reviewBotCheck{{check: check, bot: bot}}, comments)
		named := false
		for _, finding := range findings {
			if strings.Contains(finding.Description, "Required external review coderabbit is "+state) {
				named = true
			}
		}
		if wantNamed := state != "UNRESOLVED_FINDINGS"; named != wantNamed || len(findings) != 1+boolToInt(wantNamed) {
			t.Fatalf("state %s findings = %+v, want the stale comment plus a completion-state finding: %v", state, findings, wantNamed)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CodeRabbit is a review bot only when trusted ci.required_reviews opts in.
// Without it, an installed CodeRabbit's red check is an ordinary failing
// check and its unresolved threads are not findings, exactly as before
// CodeRabbit support existed; Greptile is unaffected either way.
func TestCIObservationFindings_CodeRabbitIsRecognizedOnlyWhenOptedIn(t *testing.T) {
	t.Parallel()
	comments := []scm.ReviewComment{
		{ID: "1", Author: "coderabbitai[bot]", AuthorID: scm.CodeRabbitAuthorID, Path: "a.go", Line: 3, Body: "nit"},
		{ID: "2", Author: "greptile-apps[bot]", Path: "b.go", Line: 4, Body: "off by one"},
	}
	red := scm.Check{Name: "CodeRabbit", ProviderID: "github-check-run:7", Bucket: scm.CheckBucketFail, State: "FAILURE", App: "coderabbitai", AppID: scm.CodeRabbitAppID, AppHost: "github.com"}
	greenGreptile := scm.Check{Name: "Greptile Review", Bucket: scm.CheckBucketPass, State: "SUCCESS", App: "greptile-apps"}

	unconfigured := ciObservationFindings(ciIssues{
		provider:    scm.ProviderGitHub,
		checks:      []scm.Check{red, greenGreptile},
		failing:     []string{"CodeRabbit"},
		reruns:      func(string) int { return 0 },
		botComments: comments,
		greenBots:   true,
	})
	if len(unconfigured.Items) != 2 {
		t.Fatalf("unconfigured findings = %+v, want the ordinary CodeRabbit check and the Greptile comment", unconfigured.Items)
	}
	if got := unconfigured.Items[0]; got.Category != types.FindingCategoryCICheck || got.Action != types.ActionAutoFix || got.Check != "CodeRabbit" {
		t.Fatalf("CodeRabbit finding = %+v, want an ordinary auto-fix CI check", got)
	}
	if got := unconfigured.Items[1]; got.Category != types.FindingCategoryCIReviewBot || got.File != "b.go" {
		t.Fatalf("bot finding = %+v, want only the Greptile comment", got)
	}

	synthetic := scm.Check{Name: "External review: coderabbit", ProviderID: "external-review:coderabbit", ExternalReview: "coderabbit", AppID: scm.CodeRabbitAppID, AppHost: "github.com", Bucket: scm.CheckBucketFail, State: "UNRESOLVED_FINDINGS"}
	optedIn := ciObservationFindings(ciIssues{
		provider:        scm.ProviderGitHub,
		requiredReviews: []string{"coderabbit"},
		checks:          []scm.Check{synthetic, greenGreptile},
		failing:         []string{synthetic.Name},
		reruns:          func(string) int { return 0 },
		botComments:     comments,
		greenBots:       true,
	})
	var files []string
	for _, item := range optedIn.Items {
		if item.Category != types.FindingCategoryCIReviewBot || item.Action != types.ActionAskUser {
			t.Fatalf("opted-in finding = %+v, want only ask-user review-bot findings", item)
		}
		files = append(files, item.File)
	}
	if strings.Join(files, ",") != "a.go,b.go" {
		t.Fatalf("opted-in findings anchor %v, want the CodeRabbit and Greptile comments", files)
	}
}

// A selected check that failed, was re-run, and ended cancelled has no failure
// left to repair: the pre-repair refresh must not hand it to the fixer.
func TestCIResumedRepairFailRerunCancelledSkipsRepair(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		finding string
		current scm.Check
	}{
		{
			name:    "matched by name",
			finding: `{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test"}`,
			current: scm.Check{Name: "test", Bucket: scm.CheckBucketCancel},
		},
		{
			name:    "matched by provider id",
			finding: `{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"}`,
			current: scm.Check{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketCancel},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Error("fix agent ran for a selected check whose re-run was cancelled")
				return nil, errors.New("unexpected fixer invocation")
			}}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.PreviousFindings = `{"findings":[` + tt.finding + `]}`
			host := &completionSnapshotHost{checks: []scm.Check{tt.current}}
			step := &CIStep{}
			outcome, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"})
			if err != nil || outcome != nil {
				t.Fatalf("repair = %#v, %v; want monitoring to resume", outcome, err)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("agent calls = %d, want none for a cancelled re-run", len(ag.calls))
			}
		})
	}
}
