package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_GhNotAvailable(t *testing.T) {
	t.Parallel()
	// Verify the step skips gracefully when the required provider CLI is missing.
	if _, err := exec.LookPath("gh"); err == nil {
		// gh is available on this machine, so we can't force the missing-CLI path here.
		t.Skip("gh is available, skipping unavailable test")
	}

	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, "abc", "def", config.Commands{})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip when gh is unavailable, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
}

func TestPRStep_UpdatesExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr edit was called to update the PR body
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr edit") {
		t.Errorf("expected gh pr edit to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--body") {
		t.Errorf("expected --body flag in gh pr edit, got:\n%s", ghLog)
	}

	// Verify PR URL was stored
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/42" {
		t.Errorf("PR URL = %v, want https://github.com/test/repo/pull/42", run.PRURL)
	}
}

func TestPRStep_BitbucketUpdatesExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if api.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", api.listCalls)
	}
	if api.updateCalls != 1 {
		t.Fatalf("update calls = %d, want 1", api.updateCalls)
	}
	if api.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", api.createCalls)
	}
	if api.lastAuthHeader == "" {
		t.Fatal("expected Authorization header for Bitbucket API")
	}
	if !strings.Contains(api.lastUpdateBody, "title") || !strings.Contains(api.lastUpdateBody, "description") {
		t.Fatalf("expected Bitbucket PR update payload to include title and description, got %q", api.lastUpdateBody)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://bitbucket.org/test/repo/pull-requests/42" {
		t.Fatalf("PR URL = %v, want Bitbucket PR URL", run.PRURL)
	}
}

func TestPRStep_BitbucketUpdatesExistingPRWithoutHTMLLink(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")
	api.existingPRURL = "https://bitbucket.org/test/repo/pull-requests/42"
	api.createdPRURL = ""

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.lastAuthHeader = r.Header.Get("Authorization")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pullrequests":
			api.listCalls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"values":[{"id":%d,"links":{"html":{"href":%q}}}]}`,
				api.existingPRID,
				api.existingPRURL,
			)
		case r.Method == http.MethodPut && r.URL.Path == fmt.Sprintf("/2.0/repositories/test/repo/pullrequests/%d", api.existingPRID):
			api.updateCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read update body: %v", err)
			}
			api.lastUpdateBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%d}`,
				api.existingPRID,
			)
		default:
			t.Fatalf("unexpected Bitbucket PR API request: %s %s", r.Method, r.URL.String())
		}
	})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if api.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", api.listCalls)
	}
	if api.updateCalls != 1 {
		t.Fatalf("update calls = %d, want 1", api.updateCalls)
	}
	if api.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", api.createCalls)
	}
	if outcome.PRURL != api.existingPRURL {
		t.Fatalf("outcome PR URL = %q, want %q", outcome.PRURL, api.existingPRURL)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != api.existingPRURL {
		t.Fatalf("PR URL = %v, want %q", run.PRURL, api.existingPRURL)
	}
}

func TestPRStep_ZeroBaseSHA(t *testing.T) {
	t.Parallel()
	// New branch scenario: baseSHA is all-zeros, commit log should still work
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{name: "test"}
	zeroSHA := "0000000000000000000000000000000000000000"
	sctx := newTestContextWithDBRecords(t, ag, dir, zeroSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr create was called (not blocked by zero SHA)
	logData, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logData), "pr create") {
		t.Errorf("expected gh pr create, got:\n%s", logData)
	}
}

func TestPRStep_CreatesNewPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// No existing PR - pr view returns exit 1
	env, logFile := fakeGH(t, "")

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr create was called
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr create") {
		t.Errorf("expected gh pr create to be called, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--title add feature --") {
		t.Fatalf("expected fallback PR title to reject raw non-conventional commit summary, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--title chore: add feature --body") {
		t.Fatalf("expected fallback PR title to use conventional commit format, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "add feature\n\n## Risk Assessment\n\n⚠️ Medium: touches critical error handling") {
		t.Fatalf("expected fallback PR body to append risk note under Risk Assessment heading, got:\n%s", ghLog)
	}

	// Verify PR URL was stored
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/99" {
		t.Errorf("PR URL = %v, want https://github.com/test/repo/pull/99", run.PRURL)
	}
}

func TestPRStep_BitbucketCreatesNewPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if api.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", api.listCalls)
	}
	if api.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", api.createCalls)
	}
	if api.updateCalls != 0 {
		t.Fatalf("update calls = %d, want 0", api.updateCalls)
	}
	if !strings.Contains(api.lastCreateBody, `"source"`) || !strings.Contains(api.lastCreateBody, `"destination"`) {
		t.Fatalf("expected Bitbucket PR create payload to include source and destination, got %q", api.lastCreateBody)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != api.createdPRURL {
		t.Fatalf("PR URL = %v, want %q", run.PRURL, api.createdPRURL)
	}
}

func TestPRStep_BitbucketCreatesNewPRWithoutHTMLLink(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")
	api.createdPRURL = ""

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}
	api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.lastAuthHeader = r.Header.Get("Authorization")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/2.0/repositories/test/repo/pullrequests":
			api.listCalls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"values":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/2.0/repositories/test/repo/pullrequests":
			api.createCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read create body: %v", err)
			}
			api.lastCreateBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":99}`)
		default:
			t.Fatalf("unexpected Bitbucket PR API request: %s %s", r.Method, r.URL.String())
		}
	})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if outcome.PRURL != "https://bitbucket.org/test/repo/pull-requests/99" {
		t.Fatalf("PR URL = %q, want derived Bitbucket PR URL", outcome.PRURL)
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://bitbucket.org/test/repo/pull-requests/99" {
		t.Fatalf("PR URL = %v, want derived Bitbucket PR URL", run.PRURL)
	}
}

func TestPRStep_BitbucketMissingEnvSkipsBeforeBuildingContent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected Bitbucket PR step to skip before building content, got %d agent calls", len(ag.calls))
	}
}

func TestPRStep_BitbucketUsesProcessEnvWhenStepEnvIsNil(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")
	t.Setenv("NO_MISTAKES_BITBUCKET_EMAIL", "test@example.com")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_TOKEN", "test-token")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_BASE_URL", api.server.URL)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: process env bitbucket pr","body":"## Summary\n\n- create PR via process env"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bitbucket PR step should never need approval")
	}
	if outcome.PRURL != api.createdPRURL {
		t.Fatalf("PR URL = %q, want %q", outcome.PRURL, api.createdPRURL)
	}
	if api.createCalls != 1 {
		t.Fatalf("expected Bitbucket PR create API to be called once, got %d", api.createCalls)
	}
}

func TestPRStep_UsesAgentGeneratedTitleAndBody(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	findings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: improve pipeline header UX","body":"## Summary\n\n- keep branch status readable\n- fix footer truncation"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, findings); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "--title fix: improve pipeline header UX") {
		t.Fatalf("expected generated PR title in gh call, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "keep branch status readable") {
		t.Fatalf("expected generated PR body in gh call, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "fix footer truncation\n\n## Risk Assessment\n\n⚠️ Medium: touches critical error handling") {
		t.Fatalf("expected risk note under Risk Assessment heading, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--title feature") {
		t.Fatalf("expected PR title to avoid raw branch name, got:\n%s", ghLog)
	}
}

func TestPRStep_AppendsTestingSectionFromTestStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	reviewFindings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	testRound1 := `{"findings":[{"id":"test-1","severity":"error","file":"pkg/handler_test.go","line":42,"description":"expected 429 got 200"}],"summary":"1 failure"}`

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: improve pipeline header UX","body":"## Summary\n\n- keep branch status readable\n- fix footer truncation"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, reviewFindings); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(reviewStep.ID, 1, "initial", &reviewFindings, nil, 500); err != nil {
		t.Fatal(err)
	}

	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 1, "initial", &testRound1, nil, 800); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 2, "auto_fix", nil, nil, 600); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)

	wantOrder := "## Risk Assessment\n\n⚠️ Medium: touches critical error handling\n\n## Testing\n\n- 🔧 **Test** - 1 issue found → auto-fixed\n\n## Pipeline"
	if !strings.Contains(ghLog, wantOrder) {
		t.Fatalf("expected testing section between risk assessment and pipeline, got:\n%s", ghLog)
	}
}

func TestPRStep_UnwrapsNestedJSONBody(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	// Agent returns body as the serialized prContent JSON (the bug LLMs sometimes produce).
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: improve pipeline header UX","body":"{\"title\":\"fix: improve pipeline header UX\",\"body\":\"## Summary\\n\\n- keep branch status readable\\n- fix footer truncation\"}"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)

	// The guard should unwrap the nested body and use the real markdown.
	if !strings.Contains(ghLog, "keep branch status readable") {
		t.Fatalf("expected unwrapped PR body in gh call, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, `"title"`) {
		t.Fatalf("expected JSON wrapper to be stripped from PR body, got:\n%s", ghLog)
	}
}

func TestUnwrapNestedPRBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty string", body: "", want: ""},
		{name: "plain markdown", body: "## Summary\n\n- bullet one", want: "## Summary\n\n- bullet one"},
		{name: "invalid JSON starting with brace", body: "{not valid json", want: "{not valid json"},
		{name: "valid JSON but empty nested body", body: `{"title":"fix: stuff","body":""}`, want: `{"title":"fix: stuff","body":""}`},
		{name: "nested JSON body is unwrapped", body: `{"title":"fix: stuff","body":"## Summary\n\n- real body"}`, want: "## Summary\n\n- real body"},
		{name: "nested JSON body with whitespace", body: `{"title":"fix: stuff","body":"  ## Summary  "}`, want: "## Summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapNestedPRBody(tt.body)
			if got != tt.want {
				t.Errorf("unwrapNestedPRBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestAppendGeneratedSections_StripsAgentGeneratedSections(t *testing.T) {
	body := "## Summary\n\n- improve PR descriptions\n\n## Testing\n\n- model-added testing\n\n## Risk Assessment\n\nold risk\n\n## Pipeline\n\nold pipeline"

	got := appendGeneratedSections(
		body,
		"real risk",
		"## Testing\n\n- deterministic testing",
		"## Pipeline\n\n- deterministic pipeline",
	)

	if strings.Count(got, "## Testing") != 1 {
		t.Fatalf("expected one Testing section, got:\n%s", got)
	}
	if strings.Count(got, "## Risk Assessment") != 1 {
		t.Fatalf("expected one Risk Assessment section, got:\n%s", got)
	}
	if strings.Count(got, "## Pipeline") != 1 {
		t.Fatalf("expected one Pipeline section, got:\n%s", got)
	}
	if strings.Contains(got, "model-added testing") || strings.Contains(got, "old risk") || strings.Contains(got, "old pipeline") {
		t.Fatalf("expected generated sections to replace agent-provided ones, got:\n%s", got)
	}
}

func TestAppendGeneratedSections_StripsCommonHeadingVariants(t *testing.T) {
	body := "## Summary\n\n- improve PR descriptions\n\n## tests:\n\n- model-added testing\n\n## risk assessment\n\nold risk\n\n## Pipeline:\n\nold pipeline"

	got := appendGeneratedSections(
		body,
		"real risk",
		"## Testing\n\n- deterministic testing",
		"## Pipeline\n\n- deterministic pipeline",
	)

	if strings.Contains(got, "model-added testing") || strings.Contains(got, "old risk") || strings.Contains(got, "old pipeline") {
		t.Fatalf("expected generated heading variants to be replaced, got:\n%s", got)
	}
	if strings.Count(got, "## Testing") != 1 {
		t.Fatalf("expected one normalized Testing section, got:\n%s", got)
	}
	if strings.Count(got, "## Risk Assessment") != 1 {
		t.Fatalf("expected one normalized Risk Assessment section, got:\n%s", got)
	}
	if strings.Count(got, "## Pipeline") != 1 {
		t.Fatalf("expected one normalized Pipeline section, got:\n%s", got)
	}
}

func TestPRStep_GitLabCreatesNewMR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGlab(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"feat: improve gitlab flow","body":"## Summary\n\n- add gitlab support\n\n## Testing\n\n- go test ./..."}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "mr create") {
		t.Fatalf("expected glab mr create to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--title feat: improve gitlab flow") {
		t.Fatalf("expected generated title in glab call, got:\n%s", ghLog)
	}
}

func TestPRStep_SkipsWhenProviderCLIUnavailable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip instead of failure, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL != nil {
		t.Fatalf("expected no PR URL when provider CLI unavailable, got %q", *run.PRURL)
	}
}

func TestPRStep_SkipsBeforeBuildingContentWhenProviderCLIUnavailable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("expected PR content generation to be skipped when CLI is unavailable")
			return nil, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Env = []string{"PATH=" + t.TempDir()}

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip instead of failure, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no agent calls when provider CLI unavailable, got %d", len(ag.calls))
	}
}

func TestPRStep_ExistingBranchUsesMergeBaseCommitLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "first feature commit")
	oldRemoteSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "second feature commit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, oldRemoteSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "first feature commit") {
		t.Errorf("expected PR body to include first feature commit, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "second feature commit") {
		t.Errorf("expected PR body to include second feature commit, got:\n%s", ghLog)
	}
}

func TestPRStep_AgentNonConventionalTitleFallsBack(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"Improve pipeline header UX","body":"## Summary\n\n- improvements"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	// The title should be prefixed with "chore: ", not the raw agent output
	if strings.Contains(ghLog, "--title Improve pipeline header UX --") {
		t.Fatal("non-conventional agent title should have been rejected")
	}
	if !strings.Contains(ghLog, "chore: Improve pipeline header UX") {
		t.Fatal("expected agent title to be prefixed with chore:, got: " + ghLog)
	}
	// The agent's body should be preserved, not replaced with fallback
	if !strings.Contains(ghLog, "## Summary") {
		t.Fatal("expected agent body to be preserved, got: " + ghLog)
	}
}

func TestPRStep_AgentScopedBreakingTitlePassesThrough(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	const title = "feat(api)!: require auth token"
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"feat(api)!: require auth token","body":"## Summary\n\n- require auth token on all API requests"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "--title "+title+" --body") {
		t.Fatalf("expected scoped conventional breaking-change title to pass through unchanged, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--title chore: "+title+" --body") {
		t.Fatalf("expected scoped conventional breaking-change title to avoid fallback prefix, got:\n%s", ghLog)
	}
}

// TestPRStep_PromptGuidesScopeToRealModule verifies the PR prompt instructs
// the agent to pick a scope that is a real, primary, not-too-granular
// module/package name in the codebase.
func TestPRStep_PromptGuidesScopeToRealModule(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _ := fakeGH(t, "")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			payload := json.RawMessage(`{"title":"fix(daemon): tidy logs","body":"## Summary\n\n- tidy"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(capturedPrompt, "real package/module name that exists in the codebase") {
		t.Errorf("expected PR prompt to require scope be a real package/module name in the codebase, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "primary module affected") {
		t.Errorf("expected PR prompt to require scope be the primary module affected, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "not too granular") {
		t.Errorf("expected PR prompt to warn scope should not be too granular, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "fewer than 10 distinct") {
		t.Errorf("expected PR prompt to convey typical module count heuristic, got:\n%s", capturedPrompt)
	}
}
