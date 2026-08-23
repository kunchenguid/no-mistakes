package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This repository's own gate is a thin caller of the shared composite action in
// .github/actions/require-no-mistakes. The verdict surface itself is owned by
// require_no_mistakes_action_test.go, which executes verify.py directly; the
// tests here own the CALLER: its pin, its exemptions, its triggers, its
// concurrency identity, its fork boundary, and the fact that its wiring
// actually reaches a verdict.
//
// The published workflow pins an immutable upstream SHA, but these tests run
// the action from the WORKING TREE. That asymmetry is deliberate and is what
// makes the pin a self-certification guard: a pull request that edits the
// action is fully tested here on its own head, while the required check that
// judges it keeps running the published pinned copy.

// requiredWorkflowTestHeadSHA is the commit the generated pipeline summary
// attestation binds to. Tests that execute the gate pass the same value as the
// PR head SHA unless they are asserting a mismatch.
const requiredWorkflowTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// requireActionUsesPrefix is the `uses:` prefix every enforcing repository in
// the fleet points at. The path after it must resolve to the action directory
// in this repository, so a rename breaks the caller test rather than the fleet.
const requireActionUsesPrefix = "kunchenguid/no-mistakes/"

// requiredActionPin is the exact commit the gate delegates to. It is
// deliberately asserted by value, not just by shape: the pin must always name
// a commit that already carries the action, and bumping it is a separate,
// deliberate pull request that updates this constant in the same change.
const requiredActionPin = "32d396ac0f29135daf7fcb9964aba9d5f4e796d6"

var immutableActionPin = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestNoMistakesRequiredWorkflowCallsTheSharedActionAtAnImmutablePin pins the
// migration this repository dogfoods for the whole fleet: the gate delegates to
// the shared action rather than carrying its own copy of the enforcement, and
// it names a 40-character commit SHA. A mutable ref - `@main` above all - would
// let the pull request under judgement rewrite its own judge.
func TestNoMistakesRequiredWorkflowCallsTheSharedActionAtAnImmutablePin(t *testing.T) {
	step := requiredWorkflowCheckStep(t, loadRequiredWorkflow(t))

	if step.Run != "" {
		t.Fatalf("check step still carries inline enforcement:\n%s", step.Run)
	}
	if step.Uses == "" {
		t.Fatal("check step must delegate to the shared action via uses:")
	}

	reference, pin, ok := strings.Cut(step.Uses, "@")
	if !ok {
		t.Fatalf("uses: %q carries no ref; the action must be pinned", step.Uses)
	}
	if !immutableActionPin.MatchString(pin) {
		t.Fatalf("action pinned at %q, want a 40-character commit SHA (never a branch or tag)", pin)
	}
	if pin != requiredActionPin {
		t.Fatalf("action pin changed to %q, want %q; bump it only in a separate deliberate pull request", pin, requiredActionPin)
	}

	actionPath, ok := strings.CutPrefix(reference, requireActionUsesPrefix)
	if !ok {
		t.Fatalf("uses: %q does not reference %s", step.Uses, requireActionUsesPrefix)
	}
	if actionPath != requireActionDir {
		t.Fatalf("uses: points at %q, want the action directory %q", actionPath, requireActionDir)
	}
	if _, err := os.Stat(filepath.Join(actionPath, "action.yml")); err != nil {
		t.Fatalf("pinned action path does not resolve in this repository: %v", err)
	}
}

// TestNoMistakesRequiredWorkflowExemptsReleaseAutomation pins the exemption
// logic so the release pipeline (release-please via GITHUB_TOKEN) and
// dependabot are never silently blocked by the gate.
//
// These stay job-level rather than moving to the action's exempt-authors input:
// an in-job exemption still needs the run to start, and a GITHUB_TOKEN pull
// request's run is created in action_required and never starts.
func TestNoMistakesRequiredWorkflowExemptsReleaseAutomation(t *testing.T) {
	condition := loadRequiredWorkflow(t).Jobs["check"].If
	for _, tc := range []struct {
		login   string
		wantRun bool
	}{
		{login: "github-actions[bot]", wantRun: false},
		{login: "dependabot[bot]", wantRun: false},
		{login: "release-please[bot]", wantRun: false},
		{login: "human-contributor", wantRun: true},
		{login: "unlisted-automation[bot]", wantRun: true},
	} {
		t.Run(tc.login, func(t *testing.T) {
			got, err := evaluateRequiredWorkflowAuthorCondition(condition, tc.login)
			if err != nil {
				t.Fatalf("evaluate check job condition: %v", err)
			}
			if got != tc.wantRun {
				t.Fatalf("check job runs for author %q = %t, want %t", tc.login, got, tc.wantRun)
			}
		})
	}
}

func evaluateRequiredWorkflowAuthorCondition(condition, author string) (bool, error) {
	terms := strings.Split(condition, "&&")
	if len(terms) == 0 {
		return false, fmt.Errorf("empty author condition")
	}
	termPattern := regexp.MustCompile(`^github\.event\.pull_request\.user\.login\s*!=\s*'([^']+)'$`)
	for _, term := range terms {
		matches := termPattern.FindStringSubmatch(strings.TrimSpace(term))
		if matches == nil {
			return false, fmt.Errorf("unsupported author condition term %q", strings.TrimSpace(term))
		}
		if author == matches[1] {
			return false, nil
		}
	}
	return true, nil
}

// TestNoMistakesRequiredWorkflowTriggersOnRelevantPREvents ensures the check
// re-runs when the PR body is edited so a contributor cannot bypass by opening
// clean then editing the body.
//
// It also pins the deliberate absence of "synchronize". A push never changes
// the PR body, and the pipeline pushes (Push step) before it writes the
// deterministic "## Pipeline" section (PR step), so on any PR whose body is not
// yet compliant - every PR the pipeline adopts rather than opens itself - the
// synchronize run pins a FAILURE check run to the new head for a body the same
// run is about to fix. GitHub keeps that failure alongside the later `edited`
// SUCCESS instead of replacing it, and `gh pr checks` collapses same-named
// check runs by startedAt alone, so the pipeline's own CI monitor can read the
// stale failure and park the run red with no push able to clear it (PR #773
// carried check runs 96017425510 FAILURE and 96017420271 SUCCESS on one head).
// Body-bearing events still bind attestation.head_sha to the PR head at that
// event. No ruleset requires this status, so no head SHA needs a run of its own.
func TestNoMistakesRequiredWorkflowTriggersOnRelevantPREvents(t *testing.T) {
	types := requiredWorkflowPullRequestTypes(t, loadRequiredWorkflow(t))

	for _, typ := range []string{"opened", "edited", "reopened"} {
		if !slices.Contains(types, typ) {
			t.Errorf("workflow must trigger on pull_request type %q, got %v", typ, types)
		}
	}
	if slices.Contains(types, "synchronize") {
		t.Errorf("workflow must not judge PR-body compliance on synchronize, got %v", types)
	}
}

// requiredWorkflowPullRequestTypes reads the workflow's pull_request event
// types from the parsed document, so a type named only in a comment cannot
// satisfy a trigger assertion.
func requiredWorkflowPullRequestTypes(t *testing.T, workflow requiredWorkflow) []string {
	t.Helper()
	pullRequest, ok := workflow.On["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("workflow on.pull_request = %T, want a mapping", workflow.On["pull_request"])
	}
	raw, ok := pullRequest["types"].([]any)
	if !ok {
		t.Fatalf("workflow on.pull_request.types = %T, want a sequence", pullRequest["types"])
	}
	types := make([]string, 0, len(raw))
	for _, entry := range raw {
		types = append(types, fmt.Sprint(entry))
	}
	return types
}

// TestNoMistakesRequiredWorkflowExecutesEveryBodyEvent reproduces the
// first-time-fork incident in which an opened event and two same-head body
// edits became actionable together. The scheduler fixture implements GitHub's
// documented one-running/one-pending concurrency limit, including pending-run
// replacement even when cancel-in-progress is false, and the exact
// cancel-in-progress ordering observed in runs 29962844999, 29962943078, and
// 29965243268. It then drives the check job's real delegation for every job
// that survives scheduling.
//
// This is also the caller's end-to-end wiring test: the workflow forwards no
// pr-* inputs, so a verdict only appears if the action really does read the
// body and head SHA out of the workflow event payload.
func TestNoMistakesRequiredWorkflowExecutesEveryBodyEvent(t *testing.T) {
	workflow := loadRequiredWorkflow(t)
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	events := []requiredWorkflowEvent{
		{Action: "opened", Body: compliant, HeadSHA: requiredWorkflowTestHeadSHA, PRNumber: 549, RunID: 29962844999, RunNumber: 586},
		{Action: "edited", Body: "signature removed", HeadSHA: requiredWorkflowTestHeadSHA, PRNumber: 549, RunID: 29962943078, RunNumber: 587},
		{Action: "edited", Body: compliant, HeadSHA: requiredWorkflowTestHeadSHA, PRNumber: 549, RunID: 29965243268, RunNumber: 588},
	}

	got := executeRequiredWorkflowFixture(t, workflow, events)
	want := []requiredWorkflowResult{
		{RunID: 29962844999, RunNumber: 586, Action: "opened", Executed: true, Conclusion: "success"},
		{RunID: 29962943078, RunNumber: 587, Action: "edited", Executed: true, Conclusion: "failure"},
		{RunID: 29965243268, RunNumber: 588, Action: "edited", Executed: true, Conclusion: "success"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("same-head body-event results =\n  %v\nwant every event executed to its own terminal result:\n  %v", got, want)
	}
}

// TestNoMistakesRequiredWorkflowBindsAttestationToTheEventHead keeps the one
// verdict that is genuinely a property of the caller's wiring rather than of
// verify.py: the head SHA the gate judges against comes from the event payload
// this workflow subscribes to, so a body carrying an older attestation fails
// even though it is otherwise well-formed. Every other verdict is owned by
// TestRequireActionEnforcesTheGate against the same interpreter.
func TestNoMistakesRequiredWorkflowBindsAttestationToTheEventHead(t *testing.T) {
	workflow := loadRequiredWorkflow(t)
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	staleHead := "ffffffffffffffffffffffffffffffffffffffff"

	conclusion, output := runRequiredWorkflowCheckJob(t, workflow, requiredWorkflowEvent{
		Action: "edited", Body: compliant, HeadSHA: staleHead, PRNumber: 549,
	})
	if conclusion != "failure" {
		t.Fatalf("conclusion = %q, want failure for an attestation bound to another head\n%s", conclusion, output)
	}
	for _, want := range []string{"head_sha", "does not match", requiredWorkflowTestHeadSHA, staleHead} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

func TestNoMistakesRequiredWorkflowPreservesHeadEventCoalescing(t *testing.T) {
	workflow := loadRequiredWorkflow(t)
	events := []requiredWorkflowEvent{
		{Action: "opened", PRNumber: 549, RunID: 1001},
		{Action: "edited", PRNumber: 549, RunID: 1002},
		{Action: "edited", PRNumber: 549, RunID: 1003},
		{Action: "reopened", PRNumber: 549, RunID: 1005},
		{Action: "reopened", PRNumber: 549, RunID: 1006},
	}
	groups := make([]string, len(events))
	for i, event := range events {
		groups[i] = renderRequiredWorkflowTemplate(t, workflow.Concurrency.Group, event)
	}
	if groups[0] == groups[1] || groups[0] == groups[2] || groups[1] == groups[2] {
		t.Fatalf("body-bearing event groups must be unique: %v", groups[:3])
	}
	if groups[3] != groups[4] {
		t.Fatalf("reopened groups = %q and %q, want preserved coalescing", groups[3], groups[4])
	}
	for _, bodyGroup := range groups[:3] {
		if bodyGroup == groups[3] {
			t.Fatalf("body event group %q can be canceled by a reopen", bodyGroup)
		}
	}
}

func TestNoMistakesRequiredWorkflowPublishesStableEventIdentity(t *testing.T) {
	workflow := loadRequiredWorkflow(t)
	if workflow.Jobs["check"].Name != "PR must be raised via no-mistakes" {
		t.Fatalf("required check name changed to %q", workflow.Jobs["check"].Name)
	}

	first := requiredWorkflowEvent{Action: "edited", PRNumber: 549, RunID: 29962943078, RunNumber: 587}
	latest := requiredWorkflowEvent{Action: "edited", PRNumber: 549, RunID: 29965243268, RunNumber: 588}
	firstName := renderRequiredWorkflowTemplate(t, workflow.RunName, first)
	latestName := renderRequiredWorkflowTemplate(t, workflow.RunName, latest)
	for _, want := range []string{"#549", "edited", "587", "29962943078"} {
		if !strings.Contains(firstName, want) {
			t.Errorf("first event run name %q does not expose %q", firstName, want)
		}
	}
	for _, want := range []string{"#549", "edited", "588", "29965243268"} {
		if !strings.Contains(latestName, want) {
			t.Errorf("latest event run name %q does not expose %q", latestName, want)
		}
	}
	if firstName == latestName {
		t.Fatalf("distinct body events have ambiguous run name %q", firstName)
	}
	if first.RunNumber >= latest.RunNumber {
		t.Fatalf("fixture event ordering is not monotonic: %d then %d", first.RunNumber, latest.RunNumber)
	}
}

func TestNoMistakesRequiredWorkflowKeepsForkBoundaryReadOnly(t *testing.T) {
	workflow := loadRequiredWorkflow(t)
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("required workflow must retain the safe pull_request boundary")
	}
	if _, ok := workflow.On["pull_request_target"]; ok {
		t.Fatal("required workflow must not gain pull_request_target write authority")
	}
	if got := workflow.Permissions["contents"]; got != "read" {
		t.Fatalf("contents permission = %q, want read", got)
	}
	for permission, access := range workflow.Permissions {
		if access == "write" {
			t.Fatalf("permission %q unexpectedly grants write authority", permission)
		}
	}

	data, err := os.ReadFile(requiredWorkflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "secrets.") {
		t.Fatal("required workflow must not expose secrets to fork runs")
	}
	if strings.Contains(lower, "actions/checkout") {
		t.Fatal("required workflow must not check out or execute fork code")
	}
}

const requiredWorkflowPath = ".github/workflows/no-mistakes-required.yml"

type requiredWorkflow struct {
	RunName     string                         `yaml:"run-name"`
	On          map[string]any                 `yaml:"on"`
	Permissions map[string]string              `yaml:"permissions"`
	Concurrency requiredWorkflowConcurrency    `yaml:"concurrency"`
	Jobs        map[string]requiredWorkflowJob `yaml:"jobs"`
}

type requiredWorkflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type requiredWorkflowJob struct {
	Name  string                 `yaml:"name"`
	If    string                 `yaml:"if"`
	Steps []requiredWorkflowStep `yaml:"steps"`
}

type requiredWorkflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Run  string            `yaml:"run"`
}

type requiredWorkflowEvent struct {
	Action    string
	Body      string
	HeadSHA   string
	HeadRef   string
	PRNumber  int64
	RunID     int64
	RunNumber int64
	Author    string
}

type requiredWorkflowResult struct {
	RunID      int64
	RunNumber  int64
	Action     string
	Executed   bool
	Conclusion string
}

func loadRequiredWorkflow(t *testing.T) requiredWorkflow {
	t.Helper()
	data, err := os.ReadFile(requiredWorkflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var workflow requiredWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	return workflow
}

func executeRequiredWorkflowFixture(t *testing.T, workflow requiredWorkflow, events []requiredWorkflowEvent) []requiredWorkflowResult {
	t.Helper()
	groups := make(map[string][]int)
	for i, event := range events {
		group := renderRequiredWorkflowTemplate(t, workflow.Concurrency.Group, event)
		groups[group] = append(groups[group], i)
	}

	execute := make([]bool, len(events))
	for _, indexes := range groups {
		switch {
		case len(indexes) == 1:
			execute[indexes[0]] = true
		case workflow.Concurrency.CancelInProgress:
			// This is the ordering the real first-time-fork approval incident
			// produced: the opened run executed and both waiting edits were
			// canceled. GitHub does not guarantee concurrency-group ordering.
			execute[indexes[0]] = true
		default:
			// GitHub permits one running and one pending run per group. A newer
			// pending run replaces an older pending run even when in-progress
			// cancellation is disabled.
			execute[indexes[0]] = true
			execute[indexes[len(indexes)-1]] = true
		}
	}

	results := make([]requiredWorkflowResult, len(events))
	for i, event := range events {
		result := requiredWorkflowResult{RunID: event.RunID, RunNumber: event.RunNumber, Action: event.Action}
		if !execute[i] {
			result.Conclusion = "cancelled"
			results[i] = result
			continue
		}

		conclusion, output := runRequiredWorkflowCheckJob(t, workflow, event)
		result.Executed = true
		result.Conclusion = conclusion
		if conclusion != "success" && conclusion != "failure" {
			t.Fatalf("execute compliance step for run %d: unexpected conclusion %q\n%s", event.RunID, conclusion, output)
		}
		results[i] = result
	}
	return results
}

// requiredWorkflowCheckStep returns the check job's single step, so a second
// step smuggled into the gate is caught rather than silently ignored.
func requiredWorkflowCheckStep(t *testing.T, workflow requiredWorkflow) requiredWorkflowStep {
	t.Helper()
	job, ok := workflow.Jobs["check"]
	if !ok {
		t.Fatal("required workflow is missing the check job")
	}
	if len(job.Steps) != 1 {
		t.Fatalf("check job has %d steps, want exactly the shared-action call", len(job.Steps))
	}
	return job.Steps[0]
}

// runRequiredWorkflowCheckJob drives the check job the way a runner does: it
// resolves the action the workflow delegates to, forwards the step's `with:`
// inputs under the env names the action maps them to, and hands the action a
// real workflow event payload for every PR fact the caller does not pass.
func runRequiredWorkflowCheckJob(t *testing.T, workflow requiredWorkflow, event requiredWorkflowEvent) (conclusion, output string) {
	t.Helper()
	step := requiredWorkflowCheckStep(t, workflow)
	reference, _, _ := strings.Cut(step.Uses, "@")
	actionPath, ok := strings.CutPrefix(reference, requireActionUsesPrefix)
	if !ok || actionPath != requireActionDir {
		t.Fatalf("check step uses %q, want the shared action in this repository", step.Uses)
	}

	headSHA := event.HeadSHA
	if headSHA == "" {
		headSHA = requiredWorkflowTestHeadSHA
	}
	author := event.Author
	if author == "" {
		author = "first-time-fork-contributor"
	}
	prNumber := event.PRNumber
	if prNumber == 0 {
		prNumber = 549
	}

	payload := map[string]any{
		"pull_request": map[string]any{
			"body":   event.Body,
			"number": prNumber,
			"head":   map[string]any{"sha": headSHA, "ref": event.HeadRef},
			"user":   map[string]any{"login": author},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	eventPath := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(eventPath, raw, 0o644); err != nil {
		t.Fatalf("write event payload: %v", err)
	}

	action := loadRequireAction(t, actionPath)
	if action.Runs.Using != "composite" {
		t.Fatalf("action runs.using = %q, want composite", action.Runs.Using)
	}
	if len(action.Runs.Steps) != 1 {
		t.Fatalf("composite action has %d steps, want exactly one", len(action.Runs.Steps))
	}
	compositeStep := action.Runs.Steps[0]
	if compositeStep.Shell != "bash" {
		t.Fatalf("composite action shell = %q, want bash", compositeStep.Shell)
	}
	if strings.TrimSpace(compositeStep.Run) == "" {
		t.Fatal("composite action step has no run script")
	}
	for input := range step.With {
		if _, ok := action.Inputs[input]; !ok {
			t.Fatalf("check step passes unknown action input %q", input)
		}
	}

	inputExpression := regexp.MustCompile(`^\$\{\{\s*inputs\.([a-z0-9-]+)\s*\}\}$`)
	env := make([]string, 0, len(compositeStep.Env)+3)
	for name, expression := range compositeStep.Env {
		matches := inputExpression.FindStringSubmatch(expression)
		if matches == nil {
			t.Fatalf("composite action env %q has unsupported expression %q", name, expression)
		}
		inputName := matches[1]
		input, ok := action.Inputs[inputName]
		if !ok {
			t.Fatalf("composite action env %q references undeclared input %q", name, inputName)
		}
		value, passed := step.With[inputName]
		if !passed {
			value = input.Default
		}
		env = append(env, name+"="+value)
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable to execute the composite action")
	}
	actionDir, err := filepath.Abs(actionPath)
	if err != nil {
		t.Fatalf("resolve composite action path: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputPath, nil, 0o644); err != nil {
		t.Fatalf("seed GITHUB_OUTPUT: %v", err)
	}

	cmd := exec.Command(bash, "-c", compositeStep.Run)
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env,
		"GITHUB_ACTION_PATH="+actionDir,
		"GITHUB_EVENT_PATH="+eventPath,
		"GITHUB_OUTPUT="+outputPath,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	if err == nil {
		return "success", buf.String()
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("execute composite action: %v\n%s", err, buf.String())
	}
	return "failure", buf.String()
}

func pipelineSummaryWithStatuses(t *testing.T, review, testStep, document types.StepStatus) string {
	t.Helper()
	stepResults := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: review},
		{ID: "test", StepName: types.StepTest, Status: testStep},
		{ID: "document", StepName: types.StepDocument, Status: document},
		{ID: "pr", StepName: types.StepPR, Status: types.StepStatusRunning},
		{ID: "ci", StepName: types.StepCI, Status: types.StepStatusPending},
	}
	rounds := make(map[string][]*db.StepRound, len(stepResults))
	for _, sr := range stepResults {
		rounds[sr.ID] = []*db.StepRound{{Round: 1, Trigger: "initial", DurationMS: 1}}
	}
	md, _ := steps.BuildPipelineSummary(stepResults, rounds, requiredWorkflowTestHeadSHA)
	if md == "" {
		t.Fatal("BuildPipelineSummary returned empty markdown")
	}
	return md
}

func renderRequiredWorkflowTemplate(t *testing.T, template string, event requiredWorkflowEvent) string {
	t.Helper()
	const bodyEventGroupExpression = "(github.event.action == 'opened' || github.event.action == 'edited') && github.run_id || 'head-change'"
	bodyEventGroup := "head-change"
	if event.Action == "opened" || event.Action == "edited" {
		bodyEventGroup = strconv.FormatInt(event.RunID, 10)
	}
	template = strings.ReplaceAll(template, "${{ "+bodyEventGroupExpression+" }}", bodyEventGroup)

	replacements := []struct {
		expression string
		value      string
	}{
		{expression: "github.event.action", value: event.Action},
		{expression: "github.event.pull_request.number", value: strconv.FormatInt(event.PRNumber, 10)},
		{expression: "github.event.pull_request.head.sha", value: event.HeadSHA},
		{expression: "github.run_id", value: strconv.FormatInt(event.RunID, 10)},
		{expression: "github.run_number", value: strconv.FormatInt(event.RunNumber, 10)},
	}
	for _, replacement := range replacements {
		template = strings.ReplaceAll(template, "${{ "+replacement.expression+" }}", replacement.value)
	}
	if strings.Contains(template, "${{") {
		t.Fatalf("fixture cannot evaluate workflow expression in %q", template)
	}
	return strings.Join(strings.Fields(template), " ")
}
