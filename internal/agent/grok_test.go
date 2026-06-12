package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNew_GrokAgent(t *testing.T) {
	a, err := New(types.AgentGrok, "grok", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "grok" {
		t.Errorf("expected name %q, got %q", "grok", a.Name())
	}
	if _, ok := a.(*grokAgent); !ok {
		t.Fatalf("agent type = %T, want *grokAgent", a)
	}
}

func TestNewWithOptions_GrokAgent(t *testing.T) {
	a, err := NewWithOptions(types.AgentGrok, "/path/to/grok", []string{"--model", "grok-3"}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ga, ok := a.(*grokAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *grokAgent", a)
	}
	if ga.bin != "/path/to/grok" {
		t.Errorf("bin = %q, want override path", ga.bin)
	}
	if len(ga.extraArgs) != 2 || ga.extraArgs[0] != "--model" || ga.extraArgs[1] != "grok-3" {
		t.Errorf("extraArgs = %v, want [--model grok-3]", ga.extraArgs)
	}
}

func TestBuildGrokArgs_Ordering(t *testing.T) {
	args := buildGrokArgs("/tmp/prompt.md", []string{"--model", "grok-3"}, "/repo")

	want := []string{
		"--model", "grok-3",
		"--prompt-file", "/tmp/prompt.md",
		"--cwd", "/repo",
		"--permission-mode", "bypassPermissions",
		"--output-format", "plain",
	}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %d: %v", len(want), len(args), args)
	}
	for i, expected := range want {
		if args[i] != expected {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected, args[i])
		}
	}
	for _, arg := range args {
		if arg == "--effort" || strings.HasPrefix(arg, "--effort=") {
			t.Fatalf("args must not include --effort, got %v", args)
		}
	}
}

func TestBuildGrokPromptIncludesSchemaContract(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	prompt := buildGrokPrompt("do a thing", schema)
	if !strings.Contains(prompt, "do a thing") {
		t.Errorf("prompt missing user prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "no-mistakes final output contract") {
		t.Errorf("prompt missing contract header: %s", prompt)
	}
	if !strings.Contains(prompt, "```json") {
		t.Errorf("prompt missing fenced-json instruction: %s", prompt)
	}
	if !strings.Contains(prompt, "summary") {
		t.Errorf("prompt missing schema property: %s", prompt)
	}
	if !strings.HasPrefix(prompt, "## no-mistakes final output contract") {
		t.Errorf("expected schema contract prepended, got: %q", prompt)
	}
}

func TestBuildGrokPromptOmitsContractWhenSchemaEmpty(t *testing.T) {
	prompt := buildGrokPrompt("do a thing", nil)
	if prompt != "do a thing" {
		t.Errorf("expected raw prompt when no schema, got: %q", prompt)
	}
}
