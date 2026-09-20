package jev

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveTestEnabled is the shared gate for tests that call the real TypeSafe
// API. The default suite never hits the network: both NO_MISTAKES_JEV_LIVE=1
// and a non-empty TYPESAFE_API_KEY are required. The key is never logged.
func liveTestEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv("NO_MISTAKES_JEV_LIVE") != "1" {
		t.Skip("set NO_MISTAKES_JEV_LIVE=1 and TYPESAFE_API_KEY to run the live TypeSafe scenario")
		return false
	}
	if os.Getenv(EnvKey) == "" {
		t.Skip(EnvKey + " is unset; live TypeSafe scenario skipped")
		return false
	}
	return true
}

// TestEvaluate_LiveTypeSafeAPI is the operator-credentialed live scenario for
// issue #1125: one batched System One evaluation against the real endpoint
// with the pinned model. It is skipped unless explicitly opted in so CI and
// `go test ./...` never spend quota or require a key.
func TestEvaluate_LiveTypeSafeAPI(t *testing.T) {
	if !liveTestEnabled(t) {
		return
	}
	client := NewClientFromEnv()
	if client == nil {
		t.Fatal("NewClientFromEnv returned nil after the live-test gate passed")
	}

	state := map[string]any{
		"change":     "func RenderWidget() string { return \"changed\" }",
		"candidates": []map[string]string{{"path": "widget/user.go"}},
	}
	questions := map[string]Question{
		"ctx_0": {
			Type: "score",
			Instructions: "You are ranking supporting files for a code review. " +
				"How relevant is widget/user.go as surrounding context for reviewing the change?",
			Criteria: []string{
				"Unrelated to the change: reviewing the change would not benefit from reading this file",
				"Weakly related: shares a package or naming with the change but no evident coupling to its behavior",
				"Relevant context: references or couples to the changed symbols or behavior, so reading it would inform the review",
				"Essential context: the change cannot be judged correctly without understanding this file",
			},
		},
	}

	start := time.Now()
	resp, err := client.Evaluate(context.Background(), state, questions)
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("live Evaluate: %v", err)
	}
	if resp == nil {
		t.Fatal("live Evaluate returned a nil response")
	}
	if !strings.HasPrefix(resp.Model, "jev-") {
		t.Fatalf("reported model %q, want a jev- versioned id", resp.Model)
	}
	answer, ok := resp.Answers["ctx_0"]
	if !ok || answer.Type != "score" {
		t.Fatalf("ctx_0 answer missing or not a score: %+v", resp.Answers)
	}
	if resp.Usage.InputTokens <= 0 {
		t.Fatalf("usage.input_tokens = %d, want billed input", resp.Usage.InputTokens)
	}

	t.Logf("live TypeSafe: model=%s input_tokens=%d output_tokens=%d wall_ms=%d score=%.2f confidence=%.2f",
		resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens, wall.Milliseconds(), answer.Score, answer.Confidence)

	writeLiveEvidence(t, "live-api.json", map[string]any{
		"kind":            "typesafe-evaluate",
		"endpoint":        DefaultEndpoint,
		"requested_model": Model,
		"reported_model":  resp.Model,
		"input_tokens":    resp.Usage.InputTokens,
		"output_tokens":   resp.Usage.OutputTokens,
		"wall_ms":         wall.Milliseconds(),
		"answer_type":     answer.Type,
		"answer_count":    len(resp.Answers),
	})
}

func writeLiveEvidence(t *testing.T, name string, payload any) {
	t.Helper()
	dir := os.Getenv("NO_MISTAKES_JEV_LIVE_EVIDENCE")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create live evidence dir: %v", err)
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("encode live evidence: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write live evidence: %v", err)
	}
}
