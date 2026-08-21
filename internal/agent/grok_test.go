package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGrokAgentBuildArgsUsesManagedHeadlessContractWithoutPinningModel(t *testing.T) {
	a := &grokAgent{bin: "grok", disableProjectSettings: true}
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	args := a.buildArgs("/tmp/prompt.txt", schema, "")

	wantPairs := [][]string{
		{"--prompt-file", "/tmp/prompt.txt"},
		{"--output-format", "streaming-messages-json"},
		{"--json-schema", string(schema)},
		{"--permission-mode", "bypassPermissions"},
		{"--system-prompt-override", grokGateSystemPrompt},
	}
	for _, pair := range wantPairs {
		if !grokArgsContainPair(args, pair[0], pair[1]) {
			t.Errorf("args %v missing managed pair %v", args, pair)
		}
	}
	for _, want := range []string{"--verbatim", "--no-subagents", "--no-auto-update"} {
		if !grokArgsContain(args, want) {
			t.Errorf("args %v missing %s", args, want)
		}
	}
	for _, arg := range args {
		if arg == "-m" || arg == "--model" || strings.HasPrefix(arg, "--model=") {
			t.Fatalf("managed args must leave Grok's current default model unpinned: %v", args)
		}
	}
}

func TestGrokAgentBuildArgsPreservesExplicitModelOverrideAndResume(t *testing.T) {
	a := &grokAgent{bin: "grok", extraArgs: []string{"--model", "operator-selected"}}
	args := a.buildArgs("/tmp/prompt.txt", nil, "session-123")

	if !grokArgsContainPair(args, "--model", "operator-selected") {
		t.Fatalf("args %v missing explicit model override", args)
	}
	if !grokArgsContainPair(args, "--resume", "session-123") {
		t.Fatalf("args %v missing resume identity", args)
	}
	if grokArgsContain(args, "--system-prompt-override") {
		t.Fatalf("project instruction override must be opt-in policy only: %v", args)
	}
}

func TestGrokAgentPassesInvocationEnvironmentToProcess(t *testing.T) {
	observationPath := filepath.Join(t.TempDir(), "environment")
	a := newGrokEnvHelperAgent(t)

	_, err := a.runOnce(context.Background(), RunOpts{
		Prompt: "report environment",
		CWD:    t.TempDir(),
		Env: []string{
			"NM_GROK_ENV_HELPER=run",
			"NM_GROK_ENV_OBSERVATION=" + observationPath,
			"NM_GROK_INVOCATION_VALUE=present",
		},
	})
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	got, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatalf("read environment observation: %v", err)
	}
	if string(got) != "present" {
		t.Fatalf("invocation environment = %q, want present", got)
	}
}

func newGrokEnvHelperAgent(t *testing.T) *grokAgent {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("current test executable: %v", err)
	}
	return &grokAgent{bin: exe, extraArgs: []string{"-test.run=^TestGrokEnvHelper$", "--"}}
}

func TestGrokEnvHelper(t *testing.T) {
	if os.Getenv("NM_GROK_ENV_HELPER") != "run" {
		return
	}
	if err := os.WriteFile(os.Getenv("NM_GROK_ENV_OBSERVATION"), []byte(os.Getenv("NM_GROK_INVOCATION_VALUE")), 0o644); err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString(`{"type":"result","subtype":"success","is_error":false,"result":"done"}` + "\n")
	os.Exit(0)
}

func TestGrokAgentHTTP402FailsClosedWithoutRetry(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("current test executable: %v", err)
	}
	a := &grokAgent{bin: exe, extraArgs: []string{"-test.run=^TestGrokHTTP402Helper$", "--"}}
	attempts := 0
	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
		Env:    []string{"NM_GROK_HTTP402_HELPER=run"},
		OnAttempt: func(Attempt) {
			attempts++
		},
	})
	if err == nil {
		t.Fatal("HTTP 402 must fail the invocation rather than report a successful result")
	}
	if result != nil {
		t.Fatalf("HTTP 402 result = %+v, want nil", result)
	}
	if !strings.Contains(err.Error(), "HTTP 402") {
		t.Fatalf("HTTP 402 failure = %v, want provider detail", err)
	}
	if attempts != 1 {
		t.Fatalf("HTTP 402 attempts = %d, want 1 because payment failures are not transient", attempts)
	}
}

func TestGrokHTTP402Helper(t *testing.T) {
	if os.Getenv("NM_GROK_HTTP402_HELPER") != "run" {
		return
	}
	_, _ = os.Stdout.WriteString(`{"type":"result","subtype":"success","is_error":false,"result":"not accepted"}` + "\n")
	_, _ = os.Stderr.WriteString("HTTP 402 payment required\n")
	os.Exit(1)
}

func TestGrokAgentNeutralizationFailsClosedUntilEmpiricallyVerified(t *testing.T) {
	if (&grokAgent{disableProjectSettings: true}).NeutralizesGateInstructions() {
		t.Fatal("system prompt replacement is not enough to claim complete project-setting isolation")
	}
	if (&grokAgent{}).NeutralizesGateInstructions() {
		t.Fatal("agent without the trusted opt-out must not claim neutralization")
	}
	for _, args := range [][]string{
		{"--system-prompt-override", "operator prompt"},
		{"--system-prompt=operator prompt"},
		{"--agent", "custom"},
		{"--rules", "operator rules"},
		{"--append-system-prompt=operator rules"},
	} {
		if (&grokAgent{disableProjectSettings: true, extraArgs: args}).NeutralizesGateInstructions() {
			t.Fatalf("override %v must defeat the neutralization claim", args)
		}
	}
}

func TestParseGrokEventsStructuredSuccess(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"grok-session","model":"grok-4.6"}`,
		`{"type":"assistant","message":{"model":"grok-4.6","content":[{"type":"text","text":"working"}]},"session_id":"grok-session"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"summary":"ok"},"session_id":"grok-session","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"reasoning_tokens":2}}`,
	}, "\n") + "\n"
	var chunks []string

	result, err := parseGrokEvents(context.Background(), strings.NewReader(events), func(chunk string) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	if got, want := string(result.Output), `{"summary":"ok"}`; got != want {
		t.Fatalf("output = %s, want %s", got, want)
	}
	if result.Text != "done" || result.SessionID != "grok-session" {
		t.Fatalf("result identity/text = %+v", result)
	}
	if result.Model != "grok-4.6" || result.ModelProvider != "" {
		t.Fatalf("model = %q provider = %q", result.Model, result.ModelProvider)
	}
	// Grok reports disjoint uncached/read/creation input buckets. Normalize
	// InputTokens to total prompt input so FreshInputTokens subtracts cache
	// reads exactly once.
	wantUsage := TokenUsage{InputTokens: 19, OutputTokens: 7, CacheReadTokens: 5, CacheCreationTokens: 3, ReasoningTokens: 2, Reported: true, CacheCreationReported: true, ReasoningReported: true}
	if !reflect.DeepEqual(result.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	if !reflect.DeepEqual(chunks, []string{"working"}) {
		t.Fatalf("chunks = %v, want [working]", chunks)
	}
}

func TestParseGrokEventsTreatsAllZeroUsageAsUnknown(t *testing.T) {
	events := `{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"reasoning_tokens":0}}` + "\n"
	result, err := parseGrokEvents(context.Background(), strings.NewReader(events), nil)
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	if result.UsageReported || result.CacheCreationReported || result.Usage.Reported || result.Usage.CacheCreationReported || result.Usage.ReasoningReported {
		t.Fatalf("all-zero Grok usage must remain unknown, got %+v", result)
	}
}

func TestParseGrokEventsRequiresStructuredOutputWhenSchemaRequested(t *testing.T) {
	events := `{"type":"result","subtype":"success","is_error":false,"result":"plain"}` + "\n"
	parsed, err := parseGrokEvents(context.Background(), strings.NewReader(events), nil)
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	_, err = finalizeGrokResult(parsed, json.RawMessage(`{"type":"object"}`))
	if !errors.Is(err, errGrokNoStructuredOutput) {
		t.Fatalf("finalizeGrokResult() error = %v, want errGrokNoStructuredOutput", err)
	}
}

func TestFinalizeGrokResultValidatesStructuredOutputAgainstSchema(t *testing.T) {
	result := &Result{Output: json.RawMessage(`{"summary":42}`)}
	_, err := finalizeGrokResult(result, json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`))
	if err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("finalizeGrokResult() error = %v, want schema mismatch", err)
	}
}

func TestParseGrokEventsSurfacesTerminalError(t *testing.T) {
	events := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["tool failed"]}` + "\n"
	_, err := parseGrokEvents(context.Background(), strings.NewReader(events), nil)
	if err == nil || !strings.Contains(err.Error(), "tool failed") {
		t.Fatalf("parseGrokEvents() error = %v, want terminal detail", err)
	}
}
