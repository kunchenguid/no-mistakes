package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestClaudeAgentPurposeModelOverridesGlobalModel(t *testing.T) {
	agent, err := NewWithOptions(types.AgentClaude, "claude", []string{"--model", "global-model", "--permission-mode", "acceptEdits"}, Options{
		ModelByPurpose: map[types.AgentPurpose]string{
			types.AgentPurposeReview:    "review-model",
			types.AgentPurposeReviewFix: "fix-model",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claude := agent.(*claudeAgent)

	reviewArgs := claude.buildArgs(nil, "", types.AgentPurposeReview)
	if got := modelFlagValues(reviewArgs, "--model", ""); !reflect.DeepEqual(got, []string{"review-model"}) {
		t.Fatalf("review model flags = %v, want only review-model in %v", got, reviewArgs)
	}
	if !claudeArgsContainPair(reviewArgs, "--permission-mode", "acceptEdits") {
		t.Fatalf("review args lost unrelated global args: %v", reviewArgs)
	}

	fixArgs := claude.buildArgs(nil, "session-1", types.AgentPurposeReviewFix)
	if got := modelFlagValues(fixArgs, "--model", ""); !reflect.DeepEqual(got, []string{"fix-model"}) {
		t.Fatalf("review-fix model flags = %v, want only fix-model in %v", got, fixArgs)
	}
	if !claudeArgsContainPair(fixArgs, "--resume", "session-1") {
		t.Fatalf("review-fix route lost resume identity: %v", fixArgs)
	}
}

func TestPurposeModelReplacesInlineGlobalModelFlags(t *testing.T) {
	claude := &claudeAgent{
		extraArgs: []string{"--model=global-model"},
		modelByPurpose: map[types.AgentPurpose]string{
			types.AgentPurposeReview: "review-model",
		},
	}
	if got := modelFlagValues(claude.buildArgs(nil, "", types.AgentPurposeReview), "--model", ""); !reflect.DeepEqual(got, []string{"review-model"}) {
		t.Fatalf("Claude inline precedence = %v, want review-model", got)
	}

	codex := &codexAgent{
		extraArgs: []string{"-m=global-model"},
		modelByPurpose: map[types.AgentPurpose]string{
			types.AgentPurposeReview: "review-model",
		},
	}
	if got := modelFlagValues(codex.buildArgs("review", "", "", types.AgentPurposeReview), "--model", "-m"); !reflect.DeepEqual(got, []string{"review-model"}) {
		t.Fatalf("Codex inline precedence = %v, want review-model", got)
	}
}

func TestCodexAgentPurposeModelUsesActualFallbackAdapterConfiguration(t *testing.T) {
	agent, err := NewWithOptions(types.AgentCodex, "codex", []string{"-m", "global-model", "-c", `service_tier="priority"`}, Options{
		ModelByPurpose: map[types.AgentPurpose]string{
			types.AgentPurposeReview: "codex-review-model",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codex := agent.(*codexAgent)

	args := codex.buildArgs("review", "", "thread-1", types.AgentPurposeReview)
	if got := modelFlagValues(args, "--model", "-m"); !reflect.DeepEqual(got, []string{"codex-review-model"}) {
		t.Fatalf("codex model flags = %v, want only configured Codex model in %v", got, args)
	}
	if !argsContainPair(args, "-c", `service_tier="priority"`) {
		t.Fatalf("Codex route lost unrelated global args: %v", args)
	}
	if !argsContainPair(args, "resume", "thread-1") {
		// The positional shape is `exec resume ... thread-1`; name the full argv
		// in failure because resume is intentionally not a flag-value pair.
		found := false
		for _, arg := range args {
			found = found || arg == "thread-1"
		}
		if !found {
			t.Fatalf("Codex route lost resume identity: %v", args)
		}
	}
}

func TestFallbackPurposeModelUsesActualAdapter(t *testing.T) {
	observationPath := filepath.Join(t.TempDir(), "observation.json")
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "read")
	t.Setenv("NM_CLAUDE_STDIN_OBSERVATION", observationPath)

	codex := &codexAgent{
		bin: filepath.Join(t.TempDir(), "missing-codex"),
		modelByPurpose: map[types.AgentPurpose]string{
			types.AgentPurposeReview: "codex-route-model",
		},
	}
	claude := newClaudeStdinHelperAgent(t)
	claude.modelByPurpose = map[types.AgentPurpose]string{
		types.AgentPurposeReview: "claude-route-model",
	}

	result, err := NewFallback([]Agent{codex, claude}).Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","required":["ok"]}`),
		Purpose:    types.AgentPurposeReview,
	})
	if err != nil {
		t.Fatalf("fallback Run() error = %v", err)
	}
	if result.Provider != "claude" {
		t.Fatalf("fallback provider = %q, want claude", result.Provider)
	}
	data, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatal(err)
	}
	var observation claudeStdinObservation
	if err := json.Unmarshal(data, &observation); err != nil {
		t.Fatal(err)
	}
	if got := modelFlagValues(observation.Args, "--model", ""); !reflect.DeepEqual(got, []string{"claude-route-model"}) {
		t.Fatalf("fallback model flags = %v, want actual Claude route in %v", got, observation.Args)
	}
	for _, arg := range observation.Args {
		if arg == "codex-route-model" {
			t.Fatalf("fallback leaked primary adapter model into Claude argv: %v", observation.Args)
		}
	}
}

func TestPurposeModelEmptyMapPreservesExistingArgv(t *testing.T) {
	claude := &claudeAgent{bin: "claude", extraArgs: []string{"--model", "global-model"}}
	gotClaude := claude.buildArgs(nil, "", types.AgentPurposeReview)
	wantClaude := []string{"--model", "global-model", "-p", "--verbose", "--output-format", "stream-json", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(gotClaude, wantClaude) {
		t.Fatalf("Claude argv with no purpose route = %v, want %v", gotClaude, wantClaude)
	}

	codex := &codexAgent{bin: "codex", extraArgs: []string{"-m", "global-model"}}
	gotCodex := codex.buildArgs("review", "", "", types.AgentPurposeReview)
	wantCodex := []string{"exec", "-m", "global-model", "review", "--json", "--dangerously-bypass-approvals-and-sandbox", "--color", "never"}
	if !reflect.DeepEqual(gotCodex, wantCodex) {
		t.Fatalf("Codex argv with no purpose route = %v, want %v", gotCodex, wantCodex)
	}
}

func modelFlagValues(args []string, long, short string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == long || short != "" && arg == short {
			if i+1 < len(args) {
				values = append(values, args[i+1])
				i++
			}
			continue
		}
		for _, prefix := range []string{long + "=", short + "="} {
			if prefix != "=" && len(arg) > len(prefix) && arg[:len(prefix)] == prefix {
				values = append(values, arg[len(prefix):])
			}
		}
	}
	return values
}
