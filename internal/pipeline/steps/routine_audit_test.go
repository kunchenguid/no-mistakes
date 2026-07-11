package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type routineRecordingAgent struct {
	calls []agent.RunOpts
	run   func(agent.RunOpts) (*agent.Result, error)
}

func (a *routineRecordingAgent) Name() string { return "routine-recording" }
func (a *routineRecordingAgent) Close() error { return nil }
func (a *routineRecordingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls = append(a.calls, opts)
	return a.run(opts)
}

type routineDirectAgent struct {
	calls int
}

func (a *routineDirectAgent) Name() string { return "forbidden-direct" }
func (a *routineDirectAgent) Close() error { return nil }
func (a *routineDirectAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	a.calls++
	return &agent.Result{}, nil
}

type routineRecordingInvoker struct {
	delegate agent.Invoker
	requests []agent.InvocationRequest
}

func (i *routineRecordingInvoker) Invoke(ctx context.Context, request agent.InvocationRequest) (*agent.Result, error) {
	i.requests = append(i.requests, request)
	return i.delegate.Invoke(ctx, request)
}

func reserveRoutineScope(t *testing.T, sctx *pipeline.StepContext, stepName types.StepName) types.InvocationScope {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, stepName)
	if err != nil {
		t.Fatalf("insert %s step result: %v", stepName, err)
	}
	round, err := sctx.DB.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve %s round: %v", stepName, err)
	}
	scope := types.InvocationScope{
		Kind:         types.InvocationScopePipeline,
		RunID:        sctx.Run.ID,
		StepResultID: step.ID,
		StepRoundID:  round.ID,
	}
	sctx.StepResultID = step.ID
	sctx.CurrentRound = round
	sctx.InvocationScope = scope
	return scope
}

func routeRoutineCalls(sctx *pipeline.StepContext, native *routineRecordingAgent) *routineRecordingInvoker {
	recorder := &routineRecordingInvoker{
		delegate: pipeline.NewUtilityRoutingInvoker(
			config.DefaultRoutingConfig(),
			sctx.DB,
			func(types.AgentName, string) (agent.Agent, error) {
				return native, nil
			},
		),
	}
	sctx.Invoker = recorder
	return recorder
}

func requireRoutineRequest(t *testing.T, requests []agent.InvocationRequest, index int, purpose types.Purpose, scope types.InvocationScope) {
	t.Helper()
	if len(requests) <= index {
		t.Fatalf("routed requests = %d, want request %d for %q", len(requests), index+1, purpose)
	}
	got := requests[index]
	if got.Purpose != purpose {
		t.Fatalf("routed request %d purpose = %q, want %q", index, got.Purpose, purpose)
	}
	if got.Scope != scope {
		t.Fatalf("routed request %d scope = %+v, want exact current-round scope %+v", index, got.Scope, scope)
	}
}

func requireRoutineAttempt(
	t *testing.T,
	database *db.DB,
	scope types.InvocationScope,
	purpose types.Purpose,
	role types.InvocationRole,
	candidate types.InvocationCandidate,
) {
	t.Helper()
	attempts, err := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if err != nil {
		t.Fatalf("get durable invocation attempts: %v", err)
	}
	var matches []*db.InvocationAttempt
	for _, attempt := range attempts {
		if attempt.Start.Purpose == purpose {
			matches = append(matches, attempt)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("durable attempts for purpose %q = %d in %+v, want exactly 1", purpose, len(matches), attempts)
	}
	got := matches[0]
	if got.Start.Role != role {
		t.Fatalf("durable owner role = %q, want %q", got.Start.Role, role)
	}
	if got.Start.Scope != scope {
		t.Fatalf("durable scope = %+v, want exact current-round scope %+v", got.Start.Scope, scope)
	}
	if got.Start.Candidate != candidate {
		t.Fatalf("selected Candidate = %+v, want %+v", got.Start.Candidate, candidate)
	}
	if got.Terminal == nil || got.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("durable terminal = %+v, want succeeded", got.Terminal)
	}
}

func proseFastCandidate() types.InvocationCandidate {
	return types.InvocationCandidate{
		Profile:        string(config.ProfileProseFast),
		Tier:           0,
		CandidateIndex: 0,
		Runner:         types.RunnerCodex,
		Model:          "gpt-5.6-luna",
		Effort:         types.EffortLow,
	}
}

func toolsBalancedCandidate() types.InvocationCandidate {
	return types.InvocationCandidate{
		Profile:        string(config.ProfileToolsBalanced),
		Tier:           0,
		CandidateIndex: 0,
		Runner:         types.RunnerCodex,
		Model:          "gpt-5.6-terra",
		Effort:         types.EffortHigh,
	}
}

func writeAmbiguousIntentTranscripts(t *testing.T, repoDir, fakeHome string) {
	t.Helper()
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", testClaudeProjectDirName(repoDir))
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	for index, sessionID := range []string{"s1", "s2"} {
		transcript := fmt.Sprintf(
			`{"type":"user","cwd":%s,"timestamp":%q,"uuid":"u%d","sessionId":%q,"message":{"role":"user","content":"please add Bar() to internal_foo.go"}}
{"type":"assistant","cwd":%s,"timestamp":%q,"uuid":"a%d","sessionId":%q,"message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":%s}}]}}
`,
			testJSONString(t, repoDir),
			timestamp,
			index,
			sessionID,
			testJSONString(t, repoDir),
			timestamp,
			index,
			sessionID,
			testJSONString(t, filepath.Join(repoDir, "internal_foo.go")),
		)
		if err := os.WriteFile(filepath.Join(claudeDir, sessionID+".jsonl"), []byte(transcript), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntentDisambiguationUsesJournaledPurposeRouteAndCurrentRound(t *testing.T) {
	repoDir, fakeHome, baseSHA, headSHA := initIntentRepo(t)
	writeAmbiguousIntentTranscripts(t, repoDir, fakeHome)
	withFakeHome(t, fakeHome)

	sctx := newIntentIntegrationContext(t, repoDir, baseSHA, headSHA, &config.Config{
		Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3},
	})
	direct := &routineDirectAgent{}
	sctx.Agent = direct
	scope := reserveRoutineScope(t, sctx, types.StepIntent)
	routed := &routineRecordingAgent{
		run: func(opts agent.RunOpts) (*agent.Result, error) {
			schema := string(opts.JSONSchema)
			switch {
			case strings.Contains(schema, `"agent_name"`):
				return &agent.Result{Output: json.RawMessage(`{"agent_name":"claude","session_id":"s2"}`)}, nil
			case strings.Contains(schema, `"summary"`):
				return &agent.Result{Output: json.RawMessage(`{"summary":"selected ambiguous intent"}`)}, nil
			default:
				return nil, fmt.Errorf("unexpected routed intent schema: %s", schema)
			}
		},
	}
	invoker := routeRoutineCalls(sctx, routed)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute intent: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("intent outcome = %+v, want matched intent", outcome)
	}
	if direct.calls != 0 {
		t.Fatalf("direct StepContext.Agent executions = %d, want 0", direct.calls)
	}
	if len(routed.calls) != 2 {
		t.Fatalf("routed native executions = %d, want disambiguation and summarization", len(routed.calls))
	}
	if len(invoker.requests) != 2 {
		t.Fatalf("semantic routed requests = %d, want disambiguation and summarization", len(invoker.requests))
	}
	requireRoutineRequest(t, invoker.requests, 0, types.PurposeIntentDisambiguation, scope)
	requireRoutineRequest(t, invoker.requests, 1, types.PurposeIntentSummarization, scope)
	requireRoutineAttempt(
		t,
		sctx.DB,
		scope,
		types.PurposeIntentDisambiguation,
		types.InvocationRoleVerifier,
		toolsBalancedCandidate(),
	)
	requireRoutineAttempt(
		t,
		sctx.DB,
		scope,
		types.PurposeIntentSummarization,
		types.InvocationRoleVerifier,
		proseFastCandidate(),
	)
}

func TestPRCompositionUsesJournaledPurposeRouteAndCurrentRound(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	direct := &routineDirectAgent{}
	sctx := newTestContextWithDBRecords(t, direct, dir, baseSHA, headSHA, config.Commands{})
	scope := reserveRoutineScope(t, sctx, types.StepPR)
	routed := &routineRecordingAgent{
		run: func(agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"test(pipeline): certify routing","body":"## What Changed\n\n- certify routine routing behavior"}`)}, nil
		},
	}
	invoker := routeRoutineCalls(sctx, routed)

	content, err := (&PRStep{}).buildPRContent(sctx, "feature/routing", baseSHA, 0)
	if err != nil {
		t.Fatalf("build PR content: %v", err)
	}
	if content.Title == "" || content.Body == "" {
		t.Fatalf("PR content = %+v, want routed composition", content)
	}
	if direct.calls != 0 {
		t.Fatalf("direct StepContext.Agent executions = %d, want 0", direct.calls)
	}
	if len(routed.calls) != 1 {
		t.Fatalf("routed native executions = %d, want 1", len(routed.calls))
	}
	if len(invoker.requests) != 1 {
		t.Fatalf("semantic routed requests = %d, want 1", len(invoker.requests))
	}
	requireRoutineRequest(t, invoker.requests, 0, types.PurposePRComposition, scope)
	requireRoutineAttempt(
		t,
		sctx.DB,
		scope,
		types.PurposePRComposition,
		types.InvocationRoleFixer,
		proseFastCandidate(),
	)
}

func TestTestEvidenceUsesJournaledPurposeRouteAndCurrentRound(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	direct := &routineDirectAgent{}
	sctx := newTestContextWithDBRecords(t, direct, dir, baseSHA, headSHA, config.Commands{})
	scope := reserveRoutineScope(t, sctx, types.StepTest)
	routed := &routineRecordingAgent{
		run: func(agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"focused behavior passed","tested":["go test ./focused"],"testing_summary":"focused behavior passed","artifacts":[]}`)}, nil
		},
	}
	invoker := routeRoutineCalls(sctx, routed)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute test evidence: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("test evidence outcome = %+v, want accepted evidence", outcome)
	}
	if direct.calls != 0 {
		t.Fatalf("direct StepContext.Agent executions = %d, want 0", direct.calls)
	}
	if len(routed.calls) != 1 {
		t.Fatalf("routed native executions = %d, want 1", len(routed.calls))
	}
	if len(invoker.requests) != 1 {
		t.Fatalf("semantic routed requests = %d, want 1", len(invoker.requests))
	}
	requireRoutineRequest(t, invoker.requests, 0, types.PurposeTestEvidence, scope)
	requireRoutineAttempt(
		t,
		sctx.DB,
		scope,
		types.PurposeTestEvidence,
		types.InvocationRoleFixer,
		toolsBalancedCandidate(),
	)
}

// The behavioral tests above prove each routine executes through the durable
// router. This secondary structural guard only prevents a caller from owning a
// native-adapter constructor, which no recording seam can safely exercise.
func TestRoutineCallersDoNotConstructNativeAdapters(t *testing.T) {
	forbidden := []string{"agent.New(", "agent.NewWithOptions(", "agent.NewFallback(", "agent.NewLegacyInvoker("}
	for _, file := range []string{"intent.go", "pr.go", "test.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, constructor := range forbidden {
			if strings.Contains(string(src), constructor) {
				t.Errorf("%s constructs a native adapter directly (%q); routine work must route through a registered Purpose", file, constructor)
			}
		}
	}
}
