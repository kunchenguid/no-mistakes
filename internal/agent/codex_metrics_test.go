package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCodexMetricsAccumulator_CategorizesAndTimes proves the accumulator counts
// productive round-trips, counts and categorizes tool calls (including compound
// commands and wait/poll calls), and reader-times each tool's subprocess wall.
func TestCodexMetricsAccumulator_CategorizesAndTimes(t *testing.T) {
	m := newCodexMetricsAccumulator()
	base := time.Unix(1_700_000_000, 0)

	// A narration message (round-trip, not a tool call).
	m.onItem("item.completed", &codexItem{ID: "i0", Type: "agent_message", Text: "starting"}, base)

	// A test/lint command that runs for 5s.
	m.onItem("item.started", &codexItem{ID: "i1", Type: "command_execution", Command: "bash -lc 'go test ./...'"}, base)
	m.onItem("item.completed", &codexItem{ID: "i1", Type: "command_execution", Command: "bash -lc 'go test ./...'"}, base.Add(5*time.Second))

	// A compound command: edit then git, running for 2s.
	m.onItem("item.started", &codexItem{ID: "i2", Type: "command_execution", Command: "bash -lc 'apply_patch p && git commit -am wip'"}, base.Add(5*time.Second))
	m.onItem("item.completed", &codexItem{ID: "i2", Type: "command_execution", Command: "bash -lc 'apply_patch p && git commit -am wip'"}, base.Add(7*time.Second))

	// A poll/wait call, running for 1s.
	m.onItem("item.started", &codexItem{ID: "i3", Type: "command_execution", Command: "bash -lc 'sleep 1'"}, base.Add(7*time.Second))
	m.onItem("item.completed", &codexItem{ID: "i3", Type: "command_execution", Command: "bash -lc 'sleep 1'"}, base.Add(8*time.Second))

	// A non-shell tool (mcp) counts as a tool call in the other bucket.
	m.onItem("item.completed", &codexItem{ID: "i4", Type: "mcp_tool_call"}, base.Add(8*time.Second))

	// Final answer message.
	m.onItem("item.completed", &codexItem{ID: "i5", Type: "agent_message", Text: "done"}, base.Add(8*time.Second))

	got := m.metrics()
	if got.ModelRoundtrips != 6 {
		t.Errorf("ModelRoundtrips = %d, want 6", got.ModelRoundtrips)
	}
	if got.ToolCalls != 4 {
		t.Errorf("ToolCalls = %d, want 4", got.ToolCalls)
	}
	wantCats := ToolCategoryCounts{Wait: 1, TestLint: 1, Edit: 1, Git: 1, Other: 1}
	if got.ToolCategories != wantCats {
		t.Errorf("ToolCategories = %+v, want %+v", got.ToolCategories, wantCats)
	}
	if got.SubprocessWaitMS != 8000 {
		t.Errorf("SubprocessWaitMS = %d, want 8000", got.SubprocessWaitMS)
	}
}

func TestCodexMetricsAccumulator_NilSafe(t *testing.T) {
	var m *codexMetricsAccumulator
	m.onItem("item.completed", &codexItem{Type: "agent_message"}, time.Now()) // must not panic
}

func TestCodexMetricsAccumulator_IgnoresNonModelItems(t *testing.T) {
	m := newCodexMetricsAccumulator()
	at := time.Unix(1_700_000_000, 0)

	m.onItem("item.completed", &codexItem{ID: "reasoning", Type: "reasoning"}, at)
	m.onItem("item.completed", &codexItem{ID: "unknown", Type: "metadata"}, at)
	m.onItem("item.completed", &codexItem{ID: "message", Type: "agent_message"}, at)
	m.onItem("item.completed", &codexItem{ID: "tool", Type: "mcp_tool_call"}, at)

	got := m.metrics()
	if got.ModelRoundtrips != 2 {
		t.Fatalf("ModelRoundtrips = %d, want 2", got.ModelRoundtrips)
	}
}

// TestParseCodexEvents_ExtractsMetricsAndReasoning proves the live-stream parser
// fills the metrics accumulator and captures reasoning tokens from usage.
func TestParseCodexEvents_ExtractsMetricsAndReasoning(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t-1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"ok"}}`,
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"bash -lc 'grep -rn x .'"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"bash -lc 'grep -rn x .'"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":20,"reasoning_output_tokens":7}}`,
		"",
	}, "\n")

	var usage TokenUsage
	var lastMessage, codexErr, threadID string
	metrics := newCodexMetricsAccumulator()
	if err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage, &codexErr, &threadID, metrics); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if usage.ReasoningTokens != 7 {
		t.Errorf("ReasoningTokens = %d, want 7", usage.ReasoningTokens)
	}
	got := metrics.metrics()
	if got.ModelRoundtrips != 2 || got.ToolCalls != 1 {
		t.Errorf("roundtrips/tools = %d/%d, want 2/1", got.ModelRoundtrips, got.ToolCalls)
	}
	if got.ToolCategories.Read != 1 {
		t.Errorf("read category = %d, want 1", got.ToolCategories.Read)
	}
}

// TestParseCodexEvents_MissingUsageLeavesZero proves an absent turn.completed
// usage (missing provider usage) does not fabricate token counts.
func TestParseCodexEvents_MissingUsageLeavesZero(t *testing.T) {
	events := `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}` + "\n"
	var usage TokenUsage
	var lastMessage string
	metrics := newCodexMetricsAccumulator()
	if err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage, nil, nil, metrics); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if usage.InputTokens != 0 || usage.ReasoningTokens != 0 {
		t.Fatalf("usage should be zero with no turn.completed: %+v", usage)
	}
}

func TestParseCodexRolloutModel(t *testing.T) {
	rollout := strings.Join([]string{
		`{"type":"session_meta","payload":{"model_provider":"openai","cwd":"/secret/path"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-5.6-sol","cwd":"/secret/path"}}`,
		`{"type":"response_item"}`,
		"",
	}, "\n")
	model, provider := parseCodexRolloutModel(strings.NewReader(rollout))
	if model != "gpt-5.6-sol" {
		t.Errorf("model = %q, want gpt-5.6-sol", model)
	}
	if provider != "openai" {
		t.Errorf("provider = %q, want openai", provider)
	}
}

func TestParseCodexRolloutModel_SanitizesAndBounds(t *testing.T) {
	rollout := `{"type":"turn_context","payload":{"model":"gpt-5 rm -rf / injected"}}` + "\n"
	model, _ := parseCodexRolloutModel(strings.NewReader(rollout))
	if strings.ContainsAny(model, " /") {
		t.Fatalf("model must be sanitized to a token, got %q", model)
	}
	if model != "gpt-5" {
		t.Fatalf("model = %q, want gpt-5 (stops at first non-token char)", model)
	}
}

func TestFindCodexRollout(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	// Place a rollout in yesterday's partition to exercise the multi-day window.
	day := now.AddDate(0, 0, -1)
	partition := filepath.Join(dir, day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(partition, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(partition, "rollout-2026-07-11T23-00-00-thread-xyz.jsonl")
	if err := os.WriteFile(want, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findCodexRollout(dir, "thread-xyz", now)
	if got != want {
		t.Fatalf("findCodexRollout = %q, want %q", got, want)
	}
	if findCodexRollout(dir, "no-such-thread", now) != "" {
		t.Fatal("missing rollout must resolve to empty string")
	}
}

func TestResolveCodexModel_MissingRolloutIsUnknown(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	model, provider := resolveCodexModel("absent-thread", time.Now())
	if model != "" || provider != "" {
		t.Fatalf("missing rollout must be unknown, got %q/%q", model, provider)
	}
}
