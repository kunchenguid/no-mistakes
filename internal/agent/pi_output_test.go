package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func piOutputEvents(output string) []string {
	return []string{
		`{"type":"message_end","message":{"role":"assistant","provider":"openai-codex","model":"gpt-6.1-sol","stopReason":"toolUse","content":[{"type":"toolCall","id":"final-1","name":"no_mistakes_output","arguments":` + output + `}],"usage":{"input":11,"output":7,"cacheRead":9,"cacheWrite":2}}}`,
		`{"type":"tool_execution_end","toolCallId":"final-1","toolName":"no_mistakes_output","isError":false,"result":{"content":[],"details":{"output":` + output + `},"terminate":true}}`,
	}
}

func writePiOutputFixture(t *testing.T, events []string, session string) string {
	t.Helper()
	posix := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$@\" > pi-argv.txt\n"
	windows := "@echo off\r\nmore > nul\r\necho %* > pi-argv.txt\r\n"
	if session != "" {
		events = append([]string{`{"type":"session","id":"` + session + `"}`}, events...)
	}
	for _, event := range events {
		posix += "printf '%s\\n' '" + strings.ReplaceAll(event, "'", "'\\''") + "'\n"
		windows += "echo " + event + "\r\n"
	}
	return writeFakePi(t, t.TempDir(), posix, windows)
}

func TestPiAgent_StructuredOutputUsesAnEphemeralExtension(t *testing.T) {
	const sessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"
	cwd := t.TempDir()
	bin := writePiOutputFixture(t, piOutputEvents(`{"ok":true}`), sessionID)
	pa := &piAgent{bin: bin, extraArgs: []string{"--no-extensions", "--no-tools"}}
	result, err := pa.Run(context.Background(), RunOpts{
		Prompt: "review", CWD: cwd,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		Session:    &SessionRef{ID: sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"ok":true}` || result.Model != "gpt-6.1-sol" || result.ModelProvider != "openai-codex" {
		t.Fatalf("structured result: %+v", result)
	}
	if result.SessionID != sessionID || !result.Resumed || result.SessionUsageCumulative {
		t.Fatalf("session: %+v", result)
	}
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 || result.Usage.CacheReadTokens != 9 || result.Usage.CacheCreationTokens != 2 {
		t.Fatalf("usage: %+v", result.Usage)
	}
	argv, err := os.ReadFile(filepath.Join(cwd, "pi-argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(argv))
	if len(args) < 2 || args[len(args)-2] != "--extension" {
		t.Fatalf("missing managed extension: %q", argv)
	}
	path := strings.Trim(args[len(args)-1], `"`)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("extension was not removed: %s: %v", path, err)
	}
}

func TestPreparePiOutputKeepsTheSchemaContract(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"required":{"type":"string","enum":["yes","no"]},"optional":{"type":"array","items":{"type":"integer"}}},"required":["required"]}`)
	path, cleanup, err := preparePiOutput(schema)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "const parameters = "
	_, tail, found := strings.Cut(string(content), prefix)
	if !found {
		t.Fatal("missing schema declaration")
	}
	parameters, _, _ := strings.Cut(tail, ";\n")
	for _, tc := range []struct {
		output string
		valid  bool
	}{
		{`{"required":"yes"}`, true},
		{`{"required":"yes","optional":null}`, true},
		{`{"required":"yes","optional":[1]}`, true},
		{`{}`, false},
		{`{"required":null}`, false},
		{`{"required":"maybe"}`, false},
		{`{"required":"yes","optional":"wrong"}`, false},
	} {
		err := validateStructuredOutput(json.RawMessage(tc.output), json.RawMessage(parameters))
		if (err == nil) != tc.valid {
			t.Errorf("%s valid=%v: %v", tc.output, tc.valid, err)
		}
	}
}

func TestPreparePiOutputWithoutSchemaCreatesNothing(t *testing.T) {
	path, cleanup, err := preparePiOutput(nil)
	if err != nil || path != "" || cleanup == nil {
		t.Fatalf("empty schema: %q %v", path, err)
	}
	cleanup()
	if _, _, err := preparePiOutput(json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed schema accepted")
	}
}

func TestPiAgent_RejectsUnconstrainedTextEvenWhenItMatchesTheSchema(t *testing.T) {
	bin := writePiOutputFixture(t, []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input":11,"output":7}}}`}, "")
	result, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt: "review", CWD: t.TempDir(), JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "schema-constrained") || IsStructuredOutputRejected(err) {
		t.Fatalf("missing capability must fail without a review rerun: %v", err)
	}
	if result == nil || result.Usage.InputTokens != 11 || !result.UsageReported {
		t.Fatalf("refusal lost usage: %+v", result)
	}
}

func TestPiParser_OnlyAcceptsTheFinalSuccessfulTerminatingOutput(t *testing.T) {
	good := piOutputEvents(`{"ok":true}`)
	for _, tc := range []struct {
		name   string
		events []string
		want   string
	}{
		{"valid", good, `{"ok":true}`},
		{"failed", []string{good[0], strings.Replace(good[1], `"isError":false`, `"isError":true`, 1)}, ""},
		{"unterminated", []string{good[0], strings.Replace(good[1], `"terminate":true`, `"terminate":false`, 1)}, ""},
		{"wrong call", []string{good[0], strings.Replace(good[1], `"toolCallId":"final-1"`, `"toolCallId":"other"`, 1)}, ""},
		{"wrong tool", []string{good[0], strings.Replace(good[1], `"toolName":"no_mistakes_output"`, `"toolName":"bash"`, 1)}, ""},
		{"later assistant", append(append([]string{}, good...), `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"later"}]}}`), ""},
		{"mixed batch", []string{strings.Replace(good[0], `"content":[`, `"content":[{"type":"toolCall","id":"read-1","name":"read"},`, 1), good[1]}, ""},
		{"missing result", good[:1], ""},
		{"result without call", good[1:], ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &piParser{}
			if err := p.parse(context.Background(), strings.NewReader(strings.Join(tc.events, "\n"))); err != nil {
				t.Fatal(err)
			}
			if got := p.finalOutputToolText(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}
