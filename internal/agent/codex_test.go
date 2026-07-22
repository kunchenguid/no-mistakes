package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCodexAgent_BuildArgs(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("fix the bug", "", "")

	// Default (no opt-out): pristine args, no project-doc suppression - ordinary
	// repos keep loading AGENTS.md (backward-compat).
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
	args := ca.buildArgs("fix it", "", "")

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
		args := ca.buildArgs("p", "", "")

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
	args := ca.buildArgs("review", "/tmp/schema.json", "")

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

func writeFakeCodex(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()

	name := "codex"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "codex.cmd"
		script = windowsScript
	}

	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return bin
}

func TestCodexAgent_RunWritesOutputSchemaFile(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, `#!/bin/sh
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
`, strings.Join([]string{
		"@echo off",
		"setlocal",
		"set \"dir=%~dp0\"",
		"if exist \"%dir%args.txt\" del \"%dir%args.txt\"",
		"set \"schema=\"",
		":loop",
		"if \"%~1\"==\"\" goto done",
		">> \"%dir%args.txt\" echo(%~1",
		"if \"%~1\"==\"--output-schema\" goto capture_schema",
		"shift",
		"goto loop",
		":capture_schema",
		"shift",
		"if \"%~1\"==\"\" goto done",
		"set \"schema=%~1\"",
		">> \"%dir%args.txt\" echo(%~1",
		"shift",
		"goto loop",
		":done",
		"if \"%schema%\"==\"\" (",
		"  echo missing --output-schema 1>&2",
		"  exit /b 2",
		")",
		"copy /Y \"%schema%\" \"%dir%schema.json\" >nul || exit /b 3",
		"echo {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}}",
		"echo {\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}",
	}, "\r\n"))

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
	args := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(argsRaw), "\r\n", "\n")), "\n")
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
	bin := writeFakeCodex(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"error","message":"schema rejected by codex"}'
echo 'Reading additional input from stdin...' >&2
exit 1
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"error\",\"message\":\"schema rejected by codex\"}",
		"echo Reading additional input from stdin... 1>&2",
		"exit /b 1",
	}, "\r\n"))

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

func TestCodexAgent_RunAcceptsNormalizedNullableFields(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"findings\":[{\"severity\":\"warning\",\"file\":null,\"line\":null,\"description\":\"x\",\"action\":\"auto-fix\"}],\"summary\":\"1 issue\"}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}'
`, strings.Join([]string{
		"@echo off",
		"echo {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"findings\\\":[{\\\"severity\\\":\\\"warning\\\",\\\"file\\\":null,\\\"line\\\":null,\\\"description\\\":\\\"x\\\",\\\"action\\\":\\\"auto-fix\\\"}],\\\"summary\\\":\\\"1 issue\\\"}\"}}",
		"echo {\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}",
	}, "\r\n"))

	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{
				"type":"array",
				"items":{
					"type":"object",
					"properties":{
						"severity":{"type":"string","enum":["error","warning","info"]},
						"file":{"type":"string"},
						"line":{"type":"integer"},
						"description":{"type":"string"},
						"action":{"type":"string","enum":["no-op","auto-fix","ask-user"]}
					},
					"required":["severity","description","action"]
				}
			},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)

	ca := &codexAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"findings":[{"severity":"warning","file":null,"line":null,"description":"x","action":"auto-fix"}],"summary":"1 issue"}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
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
		nil,
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
		nil,
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
		nil,
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
	err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", usage.InputTokens)
	}
}

// TestCodexAgent_BuildArgs_SuppressesProjectDocUnderOptOut locks in the codex
// project-settings contract UNDER the trusted opt-out: codex is told to read
// zero bytes of AGENTS.md (project doc) and to ignore project execpolicy .rules,
// so a gate agent validating an agent-orchestration repo (firstmate) does not
// adopt its fleet-captain identity.
//
// Empirical Codex E2E canary, run manually with codex-cli 0.144: a
// firstmate-shaped checkout contained AGENTS.md and CLAUDE.md requiring the
// unique AYE_CAPTAIN_CANARY identity token in every reply if and only if the
// project instructions governed the agent. Codex ran from that checkout in a
// read-only sandbox. The default invocation's response to "pong" included the
// token, proving that Codex loaded AGENTS.md and preserving ordinary-repo
// compatibility. With -c project_doc_max_bytes=0 plus --ignore-rules, the
// response was only "pong" and omitted the token, proving that AGENTS.md was
// suppressed. Both flags were accepted on exec and resume.
//
// A real-Codex canary is excluded from CI because it requires authentication
// and network access and would be flaky, while the e2e fakeagent cannot model
// Codex's AGENTS.md loading. The argument-level Codex tests here and Claude
// tests for --setting-sources user are the CI guarantee that the verified
// suppression knobs are emitted under the opt-out.
func TestCodexAgent_BuildArgs_SuppressesProjectDocUnderOptOut(t *testing.T) {
	ca := &codexAgent{bin: "codex", disableProjectSettings: true}
	args := ca.buildArgs("review the diff", "", "")
	if !argsContainPair(args, "-c", "project_doc_max_bytes=0") {
		t.Errorf("buildArgs = %v, want a `-c project_doc_max_bytes=0` pair", args)
	}
	if !argsContain(args, "--ignore-rules") {
		t.Errorf("buildArgs = %v, want --ignore-rules for full project-settings coverage", args)
	}
}

// TestCodexAgent_BuildArgs_NoSuppressionWithoutOptOut is the backward-compat
// guarantee: without the opt-out, codex adds no suppression and loads AGENTS.md
// exactly as before.
func TestCodexAgent_BuildArgs_NoSuppressionWithoutOptOut(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("review the diff", "", "")
	if argsContainPair(args, "-c", "project_doc_max_bytes=0") || argsContain(args, "--ignore-rules") {
		t.Errorf("buildArgs = %v, must add no suppression when the repo did not opt out", args)
	}
}

// TestCodexAgent_BuildArgs_SuppressesOnResumeUnderOptOut ensures the contract
// also applies to review-loop session resumes, whose flag surface is narrower
// but still accepts the global -c and --ignore-rules.
func TestCodexAgent_BuildArgs_SuppressesOnResumeUnderOptOut(t *testing.T) {
	ca := &codexAgent{bin: "codex", disableProjectSettings: true}
	args := ca.buildArgs("rereview", "", "thread-123")
	if args[0] != "exec" || args[1] != "resume" || args[2] != "thread-123" {
		t.Fatalf("resume positional prefix disturbed: %v", args)
	}
	if !argsContainPair(args, "-c", "project_doc_max_bytes=0") || !argsContain(args, "--ignore-rules") {
		t.Errorf("resume buildArgs = %v, want project_doc_max_bytes=0 + --ignore-rules", args)
	}
}

// TestCodexAgent_BuildArgs_UserProjectDocOverrideWins ensures an operator who
// pinned their own project_doc_max_bytes is not double-set even under opt-out.
func TestCodexAgent_BuildArgs_UserProjectDocOverrideWins(t *testing.T) {
	ca := &codexAgent{bin: "codex", disableProjectSettings: true, extraArgs: []string{"-c", "project_doc_max_bytes=4096"}}
	args := ca.buildArgs("p", "", "")
	if argsContainPair(args, "-c", "project_doc_max_bytes=0") {
		t.Errorf("buildArgs = %v, must not add project_doc_max_bytes=0 over a user pin", args)
	}
}

func argsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func argsContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
