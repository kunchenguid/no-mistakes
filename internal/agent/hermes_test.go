package agent

import (
	"encoding/json"
	"testing"
)

func TestHermesAgent_BuildArgs(t *testing.T) {
	ha := &hermesAgent{bin: "hermes"}
	args := ha.buildArgs("review the diff")

	expected := []string{
		"-z", "review the diff",
		"--ignore-rules",
		"--yolo",
	}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestHermesAgent_BuildArgs_ExtraArgsFirst(t *testing.T) {
	ha := &hermesAgent{bin: "hermes", extraArgs: []string{"--model", "glm-5.2"}}
	args := ha.buildArgs("fix it")

	expected := []string{
		"--model", "glm-5.2",
		"-z", "fix it",
		"--ignore-rules",
		"--yolo",
	}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestHermesAgent_BuildArgs_UserYoloSuppressesDefault(t *testing.T) {
	// Each case verifies that the managed --yolo is not duplicated when the
	// user already provided an approval-related flag. expectedYolo is the
	// total number of --yolo occurrences in the final argv.
	tests := []struct {
		name         string
		extra        []string
		expectedYolo int
	}{
		{"yolo", []string{"--yolo"}, 1},           // user's own, managed default suppressed
		{"safe-mode", []string{"--safe-mode"}, 0}, // suppresses default, no --yolo at all
		{"safe-mode-eq", []string{"--safe-mode=true"}, 0},
		{"model-only", []string{"--model", "glm-5.2"}, 1}, // managed default added
		{"empty", []string{}, 1},                          // managed default added
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ha := &hermesAgent{bin: "hermes", extraArgs: tt.extra}
			args := ha.buildArgs("p")

			count := 0
			for _, a := range args {
				if a == "--yolo" {
					count++
				}
			}
			if count != tt.expectedYolo {
				t.Errorf("expected %d --yolo occurrence(s), got %d in %v", tt.expectedYolo, count, args)
			}
		})
	}
}

func TestHermesAgent_Name(t *testing.T) {
	ha := &hermesAgent{bin: "hermes"}
	if ha.Name() != "hermes" {
		t.Errorf("Name() = %q, want %q", ha.Name(), "hermes")
	}
}

func TestBuildHermesPrompt_NoSchema(t *testing.T) {
	got := buildHermesPrompt("do the thing", nil)
	if got != "do the thing" {
		t.Errorf("expected unchanged prompt, got %q", got)
	}
}

func TestBuildHermesPrompt_WithSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	got := buildHermesPrompt("review", schema)
	if got == "review" {
		t.Fatal("expected prompt to be extended with schema contract")
	}
	if !contains(got, "JSON Schema") {
		t.Errorf("expected schema contract text in prompt")
	}
	if !contains(got, `"type":"object"`) && !contains(got, `"type": "object"`) {
		t.Errorf("expected schema JSON in prompt")
	}
}

func TestEmitHermesChunks(t *testing.T) {
	var collected []string
	onChunk := func(s string) { collected = append(collected, s) }

	// Exactly one chunk-size boundary
	emitHermesChunks(onChunk, "hello world")
	if len(collected) != 1 || collected[0] != "hello world" {
		t.Fatalf("expected single chunk %q, got %v", "hello world", collected)
	}

	// Empty string produces no callbacks
	collected = nil
	emitHermesChunks(onChunk, "")
	if len(collected) != 0 {
		t.Fatalf("expected no chunks for empty string, got %v", collected)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
