package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexAgent_BuildArgs(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("fix the bug", "")

	expected := []string{
		"exec", "fix the bug",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
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

func TestCodexAgent_BuildArgs_ExtraArgsAfterExec(t *testing.T) {
	ca := &codexAgent{bin: "codex", extraArgs: []string{"-m", "gpt-5.4"}}
	args := ca.buildArgs("fix it", "")

	expected := []string{
		"exec",
		"-m", "gpt-5.4",
		"fix it",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
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

func TestCodexAgent_BuildArgs_UserExecutionModeSuppressesBypass(t *testing.T) {
	tests := [][]string{
		{"--ask-for-approval", "untrusted"},
		{"--sandbox", "read-only"},
		{"--sandbox=workspace-write"},
		{"--dangerously-bypass-approvals-and-sandbox"},
	}
	for _, extra := range tests {
		ca := &codexAgent{bin: "codex", extraArgs: extra}
		args := ca.buildArgs("p", "")

		bypassCount := 0
		for _, a := range args {
			if a == "--dangerously-bypass-approvals-and-sandbox" {
				bypassCount++
			}
		}
		if len(extra) == 1 && extra[0] == "--dangerously-bypass-approvals-and-sandbox" {
			if bypassCount != 1 {
				t.Errorf("extra=%v expected single bypass, got %d: %v", extra, bypassCount, args)
			}
		} else if bypassCount != 0 {
			t.Errorf("extra=%v expected no default bypass, got: %v", extra, args)
		}
	}
}

func TestCodexAgent_BuildArgs_WithOutputSchema(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("review", "/tmp/schema.json")

	want := []string{
		"exec", "review",
		"--json",
		"--output-schema", "/tmp/schema.json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
	}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %d: %v", len(want), len(args), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg[%d]: expected %q, got %q in %v", i, want[i], args[i], args)
		}
	}
}

func TestCodexAgent_RunWritesOutputSchemaFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
dir=$(dirname "$0")
: > "$dir/args.txt"
schema=""
want_schema=""
for arg do
  printf '%s\n' "$arg" >> "$dir/args.txt"
  if [ "$want_schema" = "1" ]; then
    schema="$arg"
    want_schema=""
    continue
  fi
  if [ "$arg" = "--output-schema" ]; then
    want_schema="1"
  fi
done
if [ -z "$schema" ]; then
  echo "missing --output-schema" >&2
  exit 2
fi
cp "$schema" "$dir/schema.json"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	ca := &codexAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}

	captured, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		t.Fatalf("read captured schema: %v", err)
	}
	wantSchema := `{"additionalProperties":false,"properties":{"ok":{"type":"boolean"}},"required":["ok"],"type":"object"}`
	if string(captured) != wantSchema {
		t.Fatalf("schema file = %s, want %s", string(captured), wantSchema)
	}

	argsRaw, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")
	var schemaPath string
	for i, arg := range args {
		if arg == "--output-schema" && i+1 < len(args) {
			schemaPath = args[i+1]
			break
		}
	}
	if schemaPath == "" {
		t.Fatalf("missing --output-schema in args: %v", args)
	}
	if _, err := os.Stat(schemaPath); !os.IsNotExist(err) {
		t.Fatalf("expected temporary schema file to be removed, stat err = %v", err)
	}
}

func TestCodexAgent_RunIncludesJSONLErrorOnExitFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s\n' '{"type":"error","message":"schema rejected by codex"}'
echo 'Reading additional input from stdin...' >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	ca := &codexAgent{bin: bin}
	_, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	})
	if err == nil {
		t.Fatal("expected codex failure")
	}
	if !strings.Contains(err.Error(), "schema rejected by codex") {
		t.Fatalf("expected JSONL error in message, got %v", err)
	}
}

func TestCodexOutputSchemaAddsAdditionalPropertiesFalse(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"outer":{"type":"object","properties":{"inner":{"type":"string"}}}
		},
		"required":["outer"]
	}`)

	got, err := codexOutputSchema(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"additionalProperties":false,"properties":{"outer":{"additionalProperties":false,"properties":{"inner":{"type":["string","null"]}},"required":["inner"],"type":"object"}},"required":["outer"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("schema = %s, want %s", string(got), want)
	}
}

func TestCodexOutputSchemaRequiresAllPropertiesAndMakesOptionalNullable(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{
				"type":"array",
				"items":{
					"type":"object",
					"properties":{
						"severity":{"type":"string","enum":["error","warning"]},
						"file":{"type":"string"},
						"line":{"type":"integer"},
						"description":{"type":"string"}
					},
					"required":["severity","description"]
				}
			}
		},
		"required":["findings"]
	}`)

	got, err := codexOutputSchema(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"additionalProperties":false,"properties":{"findings":{"items":{"additionalProperties":false,"properties":{"description":{"type":"string"},"file":{"type":["string","null"]},"line":{"type":["integer","null"]},"severity":{"enum":["error","warning"],"type":"string"}},"required":["description","file","line","severity"],"type":"object"},"type":"array"}},"required":["findings"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("schema = %s, want %s", string(got), want)
	}
}

func TestParseCodexEvents_AgentMessage(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"success\":true,\"summary\":\"done\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":50,"output_tokens":100}}`,
		"",
	}, "\n")

	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		nil,
		&usage,
		&lastMessage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lastMessage != `{"success":true,"summary":"done"}` {
		t.Errorf("unexpected last message: %s", lastMessage)
	}
	if usage.InputTokens != 200 {
		t.Errorf("expected input tokens 200, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("expected output tokens 100, got %d", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 50 {
		t.Errorf("expected cache read tokens 50, got %d", usage.CacheReadTokens)
	}
}

func TestParseCodexEvents_SeparatesMultipleMessages(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"second"}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&lastMessage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "first" {
		t.Errorf("expected 'first', got %q", chunks[0])
	}
	if chunks[1] != "second" {
		t.Errorf("expected 'second', got %q", chunks[1])
	}
}

func TestParseCodexEvents_DoesNotSeparateSplitTurnMessages(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"hello "}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"world"}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&lastMessage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello " || chunks[1] != "world" {
		t.Fatalf("expected streamed turn chunks, got %v", chunks)
	}
	if lastMessage != "world" {
		t.Fatalf("expected last message 'world', got %q", lastMessage)
	}
}

func TestParseCodexEvents_SkipsMalformedLines(t *testing.T) {
	events := "garbage\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n"

	var usage TokenUsage
	var lastMessage string
	err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", usage.InputTokens)
	}
}
