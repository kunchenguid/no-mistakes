package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The shared composite action lives in this repository and every enforcing
// repository calls it instead of copying the shell. These tests execute the
// action's real entrypoint the way a runner does - environment in, exit status
// and operator copy out - rather than reading its source text.

const (
	requireActionDir    = ".github/actions/require-no-mistakes"
	requireActionScript = requireActionDir + "/verify.py"
)

type compositeAction struct {
	Name        string                     `yaml:"name"`
	Description string                     `yaml:"description"`
	Inputs      map[string]compositeInput  `yaml:"inputs"`
	Outputs     map[string]compositeOutput `yaml:"outputs"`
	Runs        compositeRuns              `yaml:"runs"`
}

type compositeInput struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
}

type compositeOutput struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

type compositeRuns struct {
	Using string          `yaml:"using"`
	Steps []compositeStep `yaml:"steps"`
}

type compositeStep struct {
	Name  string            `yaml:"name"`
	ID    string            `yaml:"id"`
	Shell string            `yaml:"shell"`
	Env   map[string]string `yaml:"env"`
	Run   string            `yaml:"run"`
}

func loadRequireAction(t *testing.T, actionDir string) compositeAction {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(actionDir, "action.yml"))
	if err != nil {
		t.Fatalf("read composite action: %v", err)
	}
	var action compositeAction
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatalf("parse composite action: %v", err)
	}
	return action
}

// pythonInterpreter mirrors the interpreter resolution the action performs on a
// runner. Windows images ship `python` rather than `python3`.
func pythonInterpreter(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no python interpreter available to execute the composite action")
	return ""
}

type actionRun struct {
	body        string
	headSHA     string
	headRef     string
	author      string
	number      string
	exemptUsers string
	exemptBots  string
	exemptRefs  string
	eventPath   string
	eventName   string
	repository  string
	apiURL      string
	token       string
}

type actionResult struct {
	conclusion string
	output     string
	outputs    map[string]string
}

func runRequireAction(t *testing.T, run actionRun) actionResult {
	t.Helper()
	python := pythonInterpreter(t)
	outputFile := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
		t.Fatalf("seed GITHUB_OUTPUT: %v", err)
	}

	cmd := exec.Command(python, requireActionScript)
	cmd.Env = append(os.Environ(),
		"PR_BODY="+run.body,
		"PR_HEAD_SHA="+run.headSHA,
		"PR_HEAD_REF="+run.headRef,
		"PR_AUTHOR="+run.author,
		"PR_NUMBER="+run.number,
		"NM_EXEMPT_AUTHORS="+run.exemptUsers,
		"NM_EXEMPT_BOT_AUTHORS="+run.exemptBots,
		"NM_EXEMPT_HEAD_BRANCHES="+run.exemptRefs,
		"GITHUB_EVENT_PATH="+run.eventPath,
		"GITHUB_EVENT_NAME="+run.eventName,
		"GITHUB_REPOSITORY="+run.repository,
		"GITHUB_API_URL="+run.apiURL,
		"GITHUB_TOKEN="+run.token,
		"GITHUB_OUTPUT="+outputFile,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	result := actionResult{output: buf.String(), outputs: map[string]string{}}
	raw, readErr := os.ReadFile(outputFile)
	if readErr != nil {
		t.Fatalf("read GITHUB_OUTPUT: %v", readErr)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			result.outputs[name] = value
		}
	}

	switch {
	case err == nil:
		result.conclusion = "success"
	default:
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("execute composite action: %v\n%s", err, buf.String())
		}
		result.conclusion = "failure"
	}
	return result
}

// TestRequireActionIsAComposite pins the shape callers depend on: a composite
// action (not a workflow) whose per-repo configuration surface is exemptions
// only, so a caller can never weaken which steps the gate certifies.
func TestRequireActionIsAComposite(t *testing.T) {
	action := loadRequireAction(t, requireActionDir)
	if action.Runs.Using != "composite" {
		t.Fatalf("runs.using = %q, want composite", action.Runs.Using)
	}
	if len(action.Runs.Steps) != 1 {
		t.Fatalf("composite steps = %d, want exactly one enforcement step", len(action.Runs.Steps))
	}
	if got := action.Runs.Steps[0].Shell; got != "bash" {
		t.Fatalf("enforcement step shell = %q, want bash", got)
	}

	for _, name := range []string{"exempt-authors", "exempt-bot-authors", "exempt-head-branches"} {
		if _, ok := action.Inputs[name]; !ok {
			t.Errorf("composite action must expose per-repo exemption input %q", name)
		}
	}
	for name := range action.Inputs {
		if strings.Contains(name, "step") || strings.Contains(name, "required") {
			t.Errorf("input %q would let a caller reconfigure which steps are required", name)
		}
	}
	for _, name := range []string{"compliant", "exempt"} {
		if _, ok := action.Outputs[name]; !ok {
			t.Errorf("composite action must expose output %q", name)
		}
	}

	// Every PR fact the script reads must be forwarded by the composite step,
	// otherwise the runner would silently judge an empty body.
	env := action.Runs.Steps[0].Env
	for _, key := range []string{"PR_BODY", "PR_HEAD_SHA", "PR_HEAD_REF", "PR_AUTHOR", "NM_EXEMPT_AUTHORS", "NM_EXEMPT_BOT_AUTHORS", "NM_EXEMPT_HEAD_BRANCHES", "GITHUB_TOKEN"} {
		if _, ok := env[key]; !ok {
			t.Errorf("composite step must forward %q to the verification script", key)
		}
	}
}

// TestRequireActionEnforcesTheGate is the behavioral contract: it runs the real
// entrypoint over PR bodies the pipeline itself generates.
func TestRequireActionEnforcesTheGate(t *testing.T) {
	signature := "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)

	tests := []struct {
		name       string
		run        actionRun
		want       string
		wantOut    []string
		notWantOut []string
	}{
		{
			name: "compliant pipeline body passes",
			run:  actionRun{body: compliant, headSHA: requiredWorkflowTestHeadSHA, number: "549"},
			want: "success",
			wantOut: []string{
				"Found no-mistakes signature in PR #549 body.",
				"Found structurally compliant pipeline step attestation.",
				"PR-body attestation is author-editable and is not cryptographic proof",
			},
		},
		{
			name:       "missing signature fails without naming the version floor",
			run:        actionRun{body: "a regular pull request", headSHA: requiredWorkflowTestHeadSHA},
			want:       "failure",
			wantOut:    []string{"This PR was not raised through no-mistakes.", "git push no-mistakes", "CONTRIBUTING.md"},
			notWantOut: []string{">= 1.46.0"},
		},
		{
			name:    "signature without attestation names the version floor",
			run:     actionRun{body: "## Pipeline\n\n" + signature + "\n", headSHA: requiredWorkflowTestHeadSHA},
			want:    "failure",
			wantOut: []string{">= 1.46.0", "https://github.com/kunchenguid/no-mistakes/pull/670", "only writes the signature"},
		},
		{
			name:    "unparseable attestation names the version floor",
			run:     actionRun{body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {not-json} -->\n", headSHA: requiredWorkflowTestHeadSHA},
			want:    "failure",
			wantOut: []string{">= 1.46.0", "https://github.com/kunchenguid/no-mistakes/pull/670", "only writes the signature"},
		},
		{
			name:    "attestation missing required JSON fields names the version floor",
			run:     actionRun{body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"steps\":[]} -->\n", headSHA: requiredWorkflowTestHeadSHA},
			want:    "failure",
			wantOut: []string{">= 1.46.0", "https://github.com/kunchenguid/no-mistakes/pull/670"},
		},
		{
			name:       "head_sha that does not match the PR head fails",
			run:        actionRun{body: compliant, headSHA: "ffffffffffffffffffffffffffffffffffffffff"},
			want:       "failure",
			wantOut:    []string{"head_sha", "does not match", requiredWorkflowTestHeadSHA, "ffffffffffffffffffffffffffffffffffffffff"},
			notWantOut: []string{">= 1.46.0"},
		},
		{
			name:       "empty attestation head_sha fails",
			run:        actionRun{body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"\",\"steps\":[{\"step\":\"review\",\"status\":\"completed\"},{\"step\":\"test\",\"status\":\"completed\"},{\"step\":\"document\",\"status\":\"completed\"}]} -->\n", headSHA: requiredWorkflowTestHeadSHA},
			want:       "failure",
			wantOut:    []string{"head_sha", "does not match"},
			notWantOut: []string{">= 1.46.0"},
		},
		{
			name:       "skipped document fails",
			run:        actionRun{body: pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusSkipped), headSHA: requiredWorkflowTestHeadSHA},
			want:       "failure",
			wantOut:    []string{"document", "skipped"},
			notWantOut: []string{">= 1.46.0"},
		},
		{
			name:       "failed test fails",
			run:        actionRun{body: pipelineSummaryWithStatuses(t, types.StepStatusFailed, types.StepStatusFailed, types.StepStatusCompleted), headSHA: requiredWorkflowTestHeadSHA},
			want:       "failure",
			wantOut:    []string{"test", "failed"},
			notWantOut: []string{">= 1.46.0"},
		},
		{
			name:    "missing review step fails",
			run:     actionRun{body: "## Pipeline\n\n" + signature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"abc\",\"steps\":[{\"step\":\"test\",\"status\":\"completed\"},{\"step\":\"document\",\"status\":\"completed\"}]} -->\n", headSHA: "abc"},
			want:    "failure",
			wantOut: []string{"review", "missing"},
		},
		{
			name:    "pending review fails",
			run:     actionRun{body: pipelineSummaryWithStatuses(t, types.StepStatusPending, types.StepStatusCompleted, types.StepStatusCompleted), headSHA: requiredWorkflowTestHeadSHA},
			want:    "failure",
			wantOut: []string{"review", "pending"},
		},
		{
			name:    "running test fails",
			run:     actionRun{body: pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusRunning, types.StepStatusCompleted), headSHA: requiredWorkflowTestHeadSHA},
			want:    "failure",
			wantOut: []string{"test", "running"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runRequireAction(t, tc.run)
			if got.conclusion != tc.want {
				t.Fatalf("conclusion = %q, want %q\n%s", got.conclusion, tc.want, got.output)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(got.output, want) {
					t.Errorf("output does not contain %q:\n%s", want, got.output)
				}
			}
			for _, notWant := range tc.notWantOut {
				if strings.Contains(got.output, notWant) {
					t.Errorf("output unexpectedly contains %q:\n%s", notWant, got.output)
				}
			}
			wantCompliant := "false"
			if tc.want == "success" {
				wantCompliant = "true"
			}
			if got.outputs["compliant"] != wantCompliant {
				t.Errorf("compliant output = %q, want %q", got.outputs["compliant"], wantCompliant)
			}
			if got.outputs["exempt"] != "false" {
				t.Errorf("exempt output = %q, want false for a judged PR", got.outputs["exempt"])
			}
		})
	}
}

// TestRequireActionExemptions covers the per-repo configuration surface: an
// exempt PR bypasses the gate even with a body that would otherwise fail, and a
// non-matching configuration never softens the verdict.
func TestRequireActionExemptions(t *testing.T) {
	nonCompliant := "a release-please pull request with no pipeline section"

	tests := []struct {
		name   string
		run    actionRun
		want   string
		reason string
	}{
		{
			name:   "configured author is exempt",
			run:    actionRun{body: nonCompliant, author: "github-actions[bot]", exemptUsers: "github-actions[bot]\ndependabot[bot]"},
			want:   "success",
			reason: "author github-actions[bot] is a configured exempt author",
		},
		{
			name:   "comma separated author list is exempt",
			run:    actionRun{body: nonCompliant, author: "dependabot[bot]", exemptUsers: "github-actions[bot], dependabot[bot]"},
			want:   "success",
			reason: "author dependabot[bot] is a configured exempt author",
		},
		{
			name:   "bot authors are exempt when opted in",
			run:    actionRun{body: nonCompliant, author: "renovate[bot]", exemptBots: "true"},
			want:   "success",
			reason: "author renovate[bot] is a bot and bot authors are exempt",
		},
		{
			name:   "structural release branch is exempt",
			run:    actionRun{body: nonCompliant, headRef: "release-please--branches--main", exemptRefs: "release-please--*"},
			want:   "success",
			reason: "head branch release-please--branches--main matches exempt pattern release-please--*",
		},
		{
			name: "unconfigured bot author is still judged",
			run:  actionRun{body: nonCompliant, author: "renovate[bot]", exemptUsers: "dependabot[bot]"},
			want: "failure",
		},
		{
			name: "human author matching no exemption is judged",
			run:  actionRun{body: nonCompliant, author: "kunchenguid", headRef: "feature", exemptUsers: "dependabot[bot]", exemptBots: "true", exemptRefs: "release-please--*"},
			want: "failure",
		},
		{
			name: "non-matching branch pattern is judged",
			run:  actionRun{body: nonCompliant, headRef: "release-please-manual", exemptRefs: "release-please--*"},
			want: "failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runRequireAction(t, tc.run)
			if got.conclusion != tc.want {
				t.Fatalf("conclusion = %q, want %q\n%s", got.conclusion, tc.want, got.output)
			}
			if tc.want != "success" {
				if got.outputs["exempt"] != "false" {
					t.Errorf("exempt output = %q, want false", got.outputs["exempt"])
				}
				return
			}
			if got.outputs["exempt"] != "true" {
				t.Errorf("exempt output = %q, want true", got.outputs["exempt"])
			}
			if got.outputs["compliant"] != "false" {
				t.Errorf("compliant output = %q, want false because an exemption is not validation", got.outputs["compliant"])
			}
			if got.outputs["exempt-reason"] != tc.reason {
				t.Errorf("exempt-reason = %q, want %q", got.outputs["exempt-reason"], tc.reason)
			}
			if !strings.Contains(got.output, tc.reason) {
				t.Errorf("output does not explain the exemption %q:\n%s", tc.reason, got.output)
			}
		})
	}
}

// TestRequireActionSynchronizeSettlesAfterBodyPublication reproduces the
// existing-PR ordering race: synchronize observes the pushed head before the
// PR edit, then the later API snapshot contains the matching attestation.
func TestRequireActionSynchronizeSettlesAfterBodyPublication(t *testing.T) {
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	eventPath := filepath.Join(t.TempDir(), "event.json")
	event := `{"action":"synchronize","pull_request":{"number":42,"body":"old body","head":{"sha":"` + requiredWorkflowTestHeadSHA + `","ref":"feature"},"user":{"login":"contributor"}}}`
	if err := os.WriteFile(eventPath, []byte(event), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var wrongRequest atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/repos/test/repo/pulls/42" || r.Header.Get("Authorization") != "Bearer test-token" {
			wrongRequest.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		body := "old body"
		if calls >= 2 {
			body = compliant
		}
		_, _ = w.Write([]byte(`{"number":42,"body":` + mustJSONString(t, body) + `,"head":{"sha":"` + requiredWorkflowTestHeadSHA + `","ref":"feature"},"user":{"login":"contributor"}}`))
	}))
	defer server.Close()
	got := runRequireAction(t, actionRun{eventPath: eventPath, eventName: "pull_request", repository: "test/repo", apiURL: server.URL, token: "test-token"})
	if got.conclusion != "success" || calls.Load() < 2 || wrongRequest.Load() {
		t.Fatalf("synchronize should wait through authenticated read-only API requests: conclusion=%s calls=%d wrong_request=%t output=%s", got.conclusion, calls.Load(), wrongRequest.Load(), got.output)
	}
}

// TestRequireActionSynchronizeRejectsEmptyEventContext prevents reruns with an
// empty event document from falling back to an arbitrary PR or stale body.
func TestRequireActionSynchronizeRejectsEmptyEventContext(t *testing.T) {
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	eventPath := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(eventPath, []byte(`{"action":"synchronize","pull_request":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"number":42,"body":` + mustJSONString(t, compliant) + `,"head":{"sha":"` + requiredWorkflowTestHeadSHA + `"}}`))
	}))
	defer server.Close()
	got := runRequireAction(t, actionRun{
		body: compliant, headSHA: requiredWorkflowTestHeadSHA, number: "42",
		eventPath: eventPath, eventName: "pull_request", repository: "test/repo", apiURL: server.URL,
	})
	if got.conclusion != "failure" || !strings.Contains(got.output, "empty PR identity") {
		t.Fatalf("empty synchronize context must fail closed: %s", got.output)
	}
	if calls.Load() != 0 {
		t.Fatalf("empty synchronize context made %d API calls", calls.Load())
	}
}

func TestRequireActionSynchronizeExemptionsDoNotWaitForSettlement(t *testing.T) {
	tests := []struct {
		name       string
		pull       string
		exemptUser string
		exemptBots string
		exemptRefs string
	}{
		{name: "author", pull: `"head":{"sha":"` + requiredWorkflowTestHeadSHA + `","ref":"feature"},"user":{"login":"release-bot"}`, exemptUser: "release-bot"},
		{name: "bot", pull: `"head":{"sha":"` + requiredWorkflowTestHeadSHA + `","ref":"feature"},"user":{"login":"release[bot]"}`, exemptBots: "true"},
		{name: "branch", pull: `"head":{"sha":"` + requiredWorkflowTestHeadSHA + `","ref":"release-please--main"},"user":{"login":"release-bot"}`, exemptRefs: "release-please--*"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eventPath := writeRequireActionEvent(t, `{"action":"synchronize","pull_request":{"number":42,"body":"old body",`+tc.pull+`}}`)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			got := runRequireAction(t, actionRun{
				eventPath: eventPath, eventName: "pull_request", repository: "test/repo", apiURL: server.URL,
				exemptUsers: tc.exemptUser, exemptBots: tc.exemptBots, exemptRefs: tc.exemptRefs,
			})
			if got.conclusion != "success" || got.outputs["exempt"] != "true" {
				t.Fatalf("synchronize exemption failed: conclusion=%s output=%s", got.conclusion, got.output)
			}
			if calls.Load() != 0 {
				t.Fatalf("synchronize exemption made %d API calls", calls.Load())
			}
		})
	}
}

func TestRequireActionSynchronizeFailsClosedOnUnsettledSnapshots(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		apiURL string
	}{
		{name: "unauthorized API", status: http.StatusForbidden, body: `{"message":"forbidden"}`},
		{name: "malformed API response", status: http.StatusOK, body: `{"number":`},
		{name: "missing attestation", status: http.StatusOK, body: `{"number":42,"body":"ordinary body","head":{"sha":"` + requiredWorkflowTestHeadSHA + `"}}`},
		{name: "stale API head", status: http.StatusOK, body: `{"number":42,"body":"ordinary body","head":{"sha":"ffffffffffffffffffffffffffffffffffffffff"}}`},
		{name: "inaccessible API", apiURL: "http://127.0.0.1:1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			apiURL := tc.apiURL
			var server *httptest.Server
			if apiURL == "" {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer server.Close()
				apiURL = server.URL
			}
			eventPath := writeRequireActionEvent(t, `{"action":"synchronize","pull_request":{"number":42,"body":"old body","head":{"sha":"`+requiredWorkflowTestHeadSHA+`","ref":"feature"},"user":{"login":"contributor"}}}`)
			got := runRequireAction(t, actionRun{eventPath: eventPath, eventName: "pull_request", repository: "test/repo", apiURL: apiURL})
			if got.conclusion != "failure" || !strings.Contains(got.output, "could not read a settled current pull request snapshot") {
				t.Fatalf("unsettled synchronize snapshot must fail closed: %s", got.output)
			}
		})
	}
}

func TestRequireActionSynchronizeHasWholeOperationDeadline(t *testing.T) {
	eventPath := writeRequireActionEvent(t, `{"action":"synchronize","pull_request":{"number":42,"body":"old body","head":{"sha":"`+requiredWorkflowTestHeadSHA+`","ref":"feature"},"user":{"login":"contributor"}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not flush")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}))
	defer server.Close()
	started := time.Now()
	got := runRequireAction(t, actionRun{eventPath: eventPath, eventName: "pull_request", repository: "test/repo", apiURL: server.URL})
	elapsed := time.Since(started)
	if got.conclusion != "failure" || !strings.Contains(got.output, "could not read a settled current pull request snapshot") {
		t.Fatalf("trickling API response must fail closed: %s", got.output)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("synchronize settlement exceeded its wall-clock bound: %s", elapsed)
	}
}

func TestRequireActionOpenedEventDoesNotReadTheAPI(t *testing.T) {
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	eventPath := writeRequireActionEvent(t, `{"action":"opened","pull_request":{"number":42,"body":`+mustJSONString(t, compliant)+`,"head":{"sha":"`+requiredWorkflowTestHeadSHA+`","ref":"feature"},"user":{"login":"contributor"}}}`)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	got := runRequireAction(t, actionRun{eventPath: eventPath, eventName: "pull_request", repository: "test/repo", apiURL: server.URL})
	if got.conclusion != "success" {
		t.Fatalf("opened event should remain API-independent: %s", got.output)
	}
	if calls.Load() != 0 {
		t.Fatalf("opened event made %d API calls", calls.Load())
	}
}

func TestRequireActionDocumentsStructuralFabricationBoundary(t *testing.T) {
	body := "## Pipeline\n\nUpdates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n\n" +
		`<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"abc","steps":[{"step":"review","status":"completed"},{"step":"test","status":"completed"},{"step":"document","status":"completed"}]} -->`
	got := runRequireAction(t, actionRun{body: body, headSHA: "abc"})
	if got.conclusion != "success" || !strings.Contains(got.output, "not cryptographic proof") {
		t.Fatalf("structurally valid author-written declaration should expose the accepted provenance limitation: %s", got.output)
	}
}

// TestRequireActionReadsTheEventPayloadWhenInputsAreOmitted is what keeps a
// caller thin: a pull_request-triggered workflow forwards nothing and the
// action still binds the attestation to the real PR head.
func TestRequireActionReadsTheEventPayloadWhenInputsAreOmitted(t *testing.T) {
	compliant := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	eventPath := filepath.Join(t.TempDir(), "event.json")
	payload := `{"pull_request":{"number":812,"body":` + mustJSONString(t, compliant) +
		`,"head":{"sha":"` + requiredWorkflowTestHeadSHA + `","ref":"fm/example"},"user":{"login":"kunchenguid"}}}`
	if err := os.WriteFile(eventPath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write event payload: %v", err)
	}

	got := runRequireAction(t, actionRun{eventPath: eventPath})
	if got.conclusion != "success" {
		t.Fatalf("conclusion = %q, want success\n%s", got.conclusion, got.output)
	}
	if !strings.Contains(got.output, "PR #812") {
		t.Errorf("output does not name the PR from the event payload:\n%s", got.output)
	}

	// The same payload with a head the attestation does not cover must fail, so
	// the payload path is genuinely bound and not merely tolerated.
	moved := strings.Replace(payload, requiredWorkflowTestHeadSHA, "ffffffffffffffffffffffffffffffffffffffff", 1)
	movedPath := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(movedPath, []byte(moved), 0o644); err != nil {
		t.Fatalf("write moved event payload: %v", err)
	}
	got = runRequireAction(t, actionRun{eventPath: movedPath})
	if got.conclusion != "failure" {
		t.Fatalf("conclusion = %q, want failure after the head moved\n%s", got.conclusion, got.output)
	}
	if !strings.Contains(got.output, "does not match") {
		t.Errorf("output does not report the head_sha bind failure:\n%s", got.output)
	}
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return string(encoded)
}

func writeRequireActionEvent(t *testing.T, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write event payload: %v", err)
	}
	return path
}
