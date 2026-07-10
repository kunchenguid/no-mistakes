package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maybeInjectFailure emits a scenario-driven failure so the routing invoker
// exercises adapter retry, provider circuits, or non-operational handling. It
// returns (exitCode, true) when it fully handled the response, or (0, false)
// when the caller should proceed with the normal success emission (used by the
// "transient" mode once its retry budget is exhausted).
//
// The fake emits only the raw CLI wire signals; the real adapter performs the
// operational/transient/model-output classification, so these modes prove the
// production classifier, not a fake reimplementation of it.
func maybeInjectFailure(agent string, action Action) (int, bool) {
	switch action.Fail {
	case "":
		return 0, false
	case "operational":
		// A non-transient operational needle: the adapter classifies an
		// OperationalError on the first exec and opens the provider circuit.
		needle := action.FailNeedle
		if needle == "" {
			needle = "usage limit reached for this account"
		}
		fmt.Fprintln(os.Stderr, needle)
		return 1, true
	case "transient":
		times := action.FailTimes
		if times <= 0 {
			times = 2
		}
		if nextFailureCount(agent, action) <= times {
			needle := action.FailNeedle
			if needle == "" {
				needle = "overloaded: 429 too many requests"
			}
			fmt.Fprintln(os.Stderr, needle)
			return 1, true
		}
		// Retry budget spent: fall through to the normal success emission.
		return 0, false
	case "output":
		// No parseable structured output: the adapter classifies a
		// non-operational model-output error, which never opens a circuit.
		emitUnparseableOutput(agent)
		return 0, true
	default:
		fmt.Fprintf(os.Stderr, "fakeagent: unknown fail mode %q\n", action.Fail)
		return 1, true
	}
}

// emitUnparseableOutput writes a wire-valid response carrying no structured
// output, so the adapter's parser returns a non-operational model-output error.
func emitUnparseableOutput(agent string) {
	enc := json.NewEncoder(os.Stdout)
	switch agent {
	case "codex":
		// Codex surfaces the final answer as agent_message text; plain prose
		// (not JSON) yields no structured output under a schema request.
		_ = enc.Encode(map[string]any{
			"type": "item.completed",
			"item": map[string]any{"type": "agent_message", "text": "no structured output emitted"},
		})
		_ = enc.Encode(map[string]any{
			"type":  "turn.completed",
			"usage": map[string]int{"input_tokens": 10, "cached_input_tokens": 0, "output_tokens": 5},
		})
	default: // claude wire shape
		_ = enc.Encode(map[string]any{
			"type":    "assistant",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "no structured output emitted"}}},
		})
		_ = enc.Encode(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			// structured_output intentionally omitted -> errNoStructuredOutput.
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
	}
}

// failureStateDir holds per-action counters for the "transient" mode. It lives
// beside $FAKEAGENT_LOG so each e2e harness (its own temp root) starts clean.
func failureStateDir() string {
	if log := os.Getenv("FAKEAGENT_LOG"); log != "" {
		return filepath.Join(filepath.Dir(log), "fakeagent-state")
	}
	return filepath.Join(os.TempDir(), "fakeagent-state")
}

// nextFailureCount returns the 1-based exec count for an action's identity,
// incrementing a counter file. Retries within one Invoke are sequential, so a
// plain read-increment-write is race-free in practice.
func nextFailureCount(agent string, action Action) int {
	dir := failureStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 1
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{agent, action.Match, action.Model, action.Effort, action.FailNeedle}, "\x00")))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8]))
	n := 0
	if data, err := os.ReadFile(path); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o644)
	return n
}
