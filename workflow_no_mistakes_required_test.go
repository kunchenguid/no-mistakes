package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// requiredWorkflowTestHeadSHA is the commit the generated pipeline summary
// attestation binds to. Tests that execute the workflow script pass the same
// value as PR_HEAD_SHA unless they are asserting a mismatch.
const requiredWorkflowTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// TestNoMistakesRequiredWorkflowExemptsReleaseAutomation pins the exemption
// logic so the release pipeline (release-please via GITHUB_TOKEN) and
// dependabot are never silently blocked by the gate.
func TestNoMistakesRequiredWorkflowExemptsReleaseAutomation(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	exempt := []string{
		"github-actions[bot]",
		"dependabot[bot]",
		"release-please[bot]",
	}
	for _, login := range exempt {
		needle := "github.event.pull_request.user.login != '" + login + "'"
		if !strings.Contains(content, needle) {
			t.Errorf("workflow must exempt %q via %q", login, needle)
		}
	}
}

// TestNoMistakesRequiredWorkflowChecksSignatureMarker pins the exact signature
// string the check greps for. It must match the literal line produced by
// internal/pipeline/steps/prsummary.go when building the Pipeline section.
func TestNoMistakesRequiredWorkflowChecksSignatureMarker(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	marker := "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
	if !strings.Contains(content, marker) {
		t.Fatalf("workflow must grep for the prsummary.go signature marker:\n  %s", marker)
	}

	summary, err := os.ReadFile("internal/pipeline/steps/prsummary.go")
	if err != nil {
		t.Fatalf("read prsummary.go: %v", err)
	}
	if !strings.Contains(string(summary), marker) {
		t.Fatalf("prsummary.go no longer writes the expected marker; update both files in sync")
	}
}

// TestNoMistakesRequiredWorkflowReadsPRBodyViaEnv pins the shell-injection-safe
// pattern: the PR body must be piped through an env var, not interpolated
// directly into the shell script body.
func TestNoMistakesRequiredWorkflowReadsPRBodyViaEnv(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "PR_BODY: ${{ github.event.pull_request.body }}") {
		t.Errorf("workflow must expose PR body via the PR_BODY env var")
	}
	if !strings.Contains(content, "PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}") {
		t.Errorf("workflow must expose PR head SHA via the PR_HEAD_SHA env var")
	}
	if strings.Contains(content, "${{ github.event.pull_request.body }}\n          run:") {
		t.Errorf("workflow must not interpolate PR body directly into run: script (injection risk)")
	}
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
// 29965243268. It then executes the workflow's real shell step for every job
// that survives scheduling.
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

func TestNoMistakesRequiredWorkflowEnforcesPipelineAttestation(t *testing.T) {
	workflow := loadRequiredWorkflow(t)
	script := requiredWorkflowRunScript(t, workflow)
	signature := "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"

	tests := []struct {
		name       string
		body       string
		headSHA    string
		want       string
		wantOut    []string
		notWantOut []string
	}{
		{
			name:    "missing signature",
			body:    "a regular pull request",
			want:    "failure",
			wantOut: []string{"This PR was not raised through no-mistakes.", "git push no-mistakes", "CONTRIBUTING.md"},
			notWantOut: []string{
				">= 1.46.0",
			},
		},
		{
			name: "signature without attestation",
			body: "## Pipeline\n\n" + signature + "\n",
			want: "failure",
			wantOut: []string{
				">= 1.46.0",
				"https://github.com/kunchenguid/no-mistakes/pull/670",
				"only writes the signature",
			},
		},
		{
			name: "unparseable attestation JSON",
			body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {not-json} -->\n",
			want: "failure",
			wantOut: []string{
				">= 1.46.0",
				"https://github.com/kunchenguid/no-mistakes/pull/670",
				"only writes the signature",
			},
		},
		{
			name: "attestation missing required JSON fields",
			body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"steps\":[]} -->\n",
			want: "failure",
			wantOut: []string{
				">= 1.46.0",
				"https://github.com/kunchenguid/no-mistakes/pull/670",
			},
		},
		{
			name: "quota skip on document is non-compliant",
			body: pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusSkipped),
			want: "failure",
			wantOut: []string{
				"document",
				"skipped",
			},
			notWantOut: []string{
				">= 1.46.0",
			},
		},
		{
			name: "failed test is non-compliant",
			body: pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusFailed, types.StepStatusCompleted),
			want: "failure",
			wantOut: []string{
				"test",
				"failed",
			},
			notWantOut: []string{
				">= 1.46.0",
			},
		},
		{
			name:    "missing review step is non-compliant",
			body:    "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"abc\",\"steps\":[{\"step\":\"test\",\"status\":\"completed\"},{\"step\":\"document\",\"status\":\"completed\"}]} -->\n",
			headSHA: "abc",
			want:    "failure",
			wantOut: []string{
				"review",
				"missing",
			},
			notWantOut: []string{
				">= 1.46.0",
			},
		},
		{
			name: "pending review is non-compliant",
			body: pipelineSummaryWithStatuses(t, types.StepStatusPending, types.StepStatusCompleted, types.StepStatusCompleted),
			want: "failure",
			wantOut: []string{
				"review",
				"pending",
			},
		},
		{
			name: "running test is non-compliant",
			body: pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusRunning, types.StepStatusCompleted),
			want: "failure",
			wantOut: []string{
				"test",
				"running",
			},
		},
		{
			name:    "generated compliant attestation passes",
			body:    pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted),
			want:    "success",
			wantOut: []string{"Found no-mistakes signature"},
		},
		{
			name: "empty attestation head_sha is non-compliant",
			body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"\",\"steps\":[{\"step\":\"review\",\"status\":\"completed\"},{\"step\":\"test\",\"status\":\"completed\"},{\"step\":\"document\",\"status\":\"completed\"}]} -->\n",
			want: "failure",
			wantOut: []string{
				"head_sha",
				"does not match",
			},
			notWantOut: []string{
				">= 1.46.0",
			},
		},
		{
			name:    "attestation head_sha that does not match the PR head is non-compliant",
			body:    pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted),
			headSHA: "ffffffffffffffffffffffffffffffffffffffff",
			want:    "failure",
			wantOut: []string{
				"head_sha",
				"does not match",
				requiredWorkflowTestHeadSHA,
				"ffffffffffffffffffffffffffffffffffffffff",
			},
			notWantOut: []string{
				">= 1.46.0",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conclusion, output := runRequiredWorkflowStep(t, script, tc.body, tc.headSHA)
			if conclusion != tc.want {
				t.Fatalf("conclusion = %q, want %q\n%s", conclusion, tc.want, output)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(output, want) {
					t.Errorf("output does not contain %q:\n%s", want, output)
				}
			}
			for _, notWant := range tc.notWantOut {
				if strings.Contains(output, notWant) {
					t.Errorf("output unexpectedly contains %q:\n%s", notWant, output)
				}
			}
		})
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

	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
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
	Steps []requiredWorkflowStep `yaml:"steps"`
}

type requiredWorkflowStep struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

type requiredWorkflowEvent struct {
	Action    string
	Body      string
	HeadSHA   string
	PRNumber  int64
	RunID     int64
	RunNumber int64
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
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
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

	script := requiredWorkflowRunScript(t, workflow)
	results := make([]requiredWorkflowResult, len(events))
	for i, event := range events {
		result := requiredWorkflowResult{RunID: event.RunID, RunNumber: event.RunNumber, Action: event.Action}
		if !execute[i] {
			result.Conclusion = "cancelled"
			results[i] = result
			continue
		}

		conclusion, output := runRequiredWorkflowStep(t, script, event.Body, event.HeadSHA)
		result.Executed = true
		result.Conclusion = conclusion
		if conclusion != "success" && conclusion != "failure" {
			t.Fatalf("execute compliance step for run %d: unexpected conclusion %q\n%s", event.RunID, conclusion, output)
		}
		results[i] = result
	}
	return results
}

func requiredWorkflowRunScript(t *testing.T, workflow requiredWorkflow) string {
	t.Helper()
	job, ok := workflow.Jobs["check"]
	if !ok || len(job.Steps) == 0 || job.Steps[0].Run == "" {
		t.Fatal("required workflow is missing the check job run script")
	}
	return job.Steps[0].Run
}

func runRequiredWorkflowStep(t *testing.T, runScript, body, headSHA string) (conclusion, output string) {
	t.Helper()
	if headSHA == "" {
		headSHA = requiredWorkflowTestHeadSHA
	}
	cmd := exec.Command("bash", "-c", runScript)
	cmd.Env = append(os.Environ(),
		"PR_BODY="+body,
		"PR_HEAD_SHA="+headSHA,
		"PR_AUTHOR=first-time-fork-contributor",
		"PR_NUMBER=549",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return "success", buf.String()
	}
	if _, ok := err.(*exec.ExitError); ok {
		return "failure", buf.String()
	}
	t.Fatalf("execute compliance step: %v\n%s", err, buf.String())
	return "", ""
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
