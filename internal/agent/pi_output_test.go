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

const piStrictVersion = "0.99.1"

// writePiOutputFixture fakes a Pi binary that answers --version with version,
// records each run's argv and prompt in its working directory, and replays
// strictEvents when launched with the output extension and promptEvents
// otherwise.
func writePiOutputFixture(t *testing.T, version string, strictEvents, promptEvents []string, session string) string {
	t.Helper()
	if session != "" {
		header := `{"type":"session","id":"` + session + `"}`
		strictEvents = append([]string{header}, strictEvents...)
		promptEvents = append([]string{header}, promptEvents...)
	}
	posixEvents := func(events []string) string {
		var b strings.Builder
		for _, event := range events {
			b.WriteString("printf '%s\\n' '" + strings.ReplaceAll(event, "'", "'\\''") + "'\n")
		}
		return b.String()
	}
	windowsEvents := func(events []string) string {
		var b strings.Builder
		for _, event := range events {
			b.WriteString("echo " + event + "\r\n")
		}
		return b.String()
	}
	posix := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo " + version + "; exit 0; fi\n" +
		"cat > pi-prompt.txt\nprintf '%s\\n' \"$@\" > pi-argv.txt\necho run >> pi-runs.txt\n" +
		"case \" $* \" in *\" --extension \"*)\n" + posixEvents(strictEvents) + ";;\n*)\n" + posixEvents(promptEvents) + ";;\nesac\n"
	windows := "@echo off\r\n" +
		"if \"%1\"==\"--version\" (echo " + version + "& exit /b 0)\r\n" +
		"more > pi-prompt.txt\r\necho %* > pi-argv.txt\r\necho run>> pi-runs.txt\r\n" +
		"echo %* | findstr /C:\"--extension\" >nul && goto strict\r\n" +
		windowsEvents(promptEvents) + "exit /b 0\r\n:strict\r\n" + windowsEvents(strictEvents)
	return writeFakePi(t, t.TempDir(), posix, windows)
}

func piTextEvents(text string) []string {
	encoded, _ := json.Marshal(text)
	return []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":` + string(encoded) + `}],"usage":{"input":11,"output":7}}}`}
}

func piOutputPathMessages(events *[]LifecycleEvent) func(LifecycleEvent) {
	return func(event LifecycleEvent) {
		if event.Phase == piLifecycleOutputPath || event.Phase == LifecyclePhaseFallback {
			*events = append(*events, event)
		}
	}
}

func readPiRuns(t *testing.T, cwd string) int {
	t.Helper()
	runs, err := os.ReadFile(filepath.Join(cwd, "pi-runs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(runs)))
}

func TestPiAgent_StructuredOutputUsesAnEphemeralExtension(t *testing.T) {
	const sessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"
	cwd := t.TempDir()
	bin := writePiOutputFixture(t, piStrictVersion, piOutputEvents(`{"ok":true}`), piTextEvents(`{"ok":true}`), sessionID)
	pa := &piAgent{bin: bin, extraArgs: []string{"--no-extensions", "--no-tools"}}
	var logged []LifecycleEvent
	result, err := pa.Run(context.Background(), RunOpts{
		Prompt: "review", CWD: cwd,
		JSONSchema:  json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		Session:     &SessionRef{ID: sessionID},
		OnLifecycle: piOutputPathMessages(&logged),
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
	if len(logged) != 1 || !strings.Contains(logged[0].Message, "strict schema-constrained no_mistakes_output") {
		t.Fatalf("step log must name the strict path: %+v", logged)
	}
}

func TestPiAgent_OlderPiUsesThePromptInlinedSchema(t *testing.T) {
	for _, version := range []string{"0.81.9", "0.82.0-beta", "v0.82.0-rc.1", "unknown"} {
		t.Run(version, func(t *testing.T) {
			cwd := t.TempDir()
			bin := writePiOutputFixture(t, version, piOutputEvents(`{"ok":false}`), piTextEvents("```json\n{\"ok\":true}\n```"), "")
			var logged []LifecycleEvent
			result, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
				Prompt: "review", CWD: cwd,
				JSONSchema:  json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
				OnLifecycle: piOutputPathMessages(&logged),
			})
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Output) != `{"ok":true}` {
				t.Fatalf("prompt-path output: %+v", result)
			}
			argv, err := os.ReadFile(filepath.Join(cwd, "pi-argv.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(argv), "--extension") {
				t.Fatalf("older Pi was given the output extension: %q", argv)
			}
			prompt, err := os.ReadFile(filepath.Join(cwd, "pi-prompt.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(prompt), `"required": [`) {
				t.Fatalf("prompt path must inline the schema: %s", prompt)
			}
			if len(logged) != 1 || !strings.Contains(logged[0].Message, "prompt-inlined schema") || !strings.Contains(logged[0].Message, version) {
				t.Fatalf("step log must name the prompt path and why: %+v", logged)
			}
		})
	}
}

func TestPiVersionSupportsStrictOutput(t *testing.T) {
	for version, want := range map[string]bool{
		"0.82.0": true, "0.82.1": true, "0.99.1": true, "1.0.0": true, "v0.82.0": true,
		"0.83.0-beta": true, "0.82.0+build.7": true,
		"0.82.0-beta": false, "0.81.9": false, "0.9.0": false, "": false, "0.82": false, "x.y.z": false,
	} {
		if got := piVersionSupportsStrictOutput(version); got != want {
			t.Errorf("%q = %v, want %v", version, got, want)
		}
	}
}

func TestPiAgent_ProviderWithoutStrictToolsFallsBackToThePromptPath(t *testing.T) {
	cwd := t.TempDir()
	refusal := `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"Tool \"no_mistakes_output\" requires JSON-schema constrained sampling, but strict tools are unsupported.","content":[]}}`
	bin := writePiOutputFixture(t, piStrictVersion, []string{refusal}, piTextEvents(`{"ok":true}`), "")
	pa := &piAgent{bin: bin}
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	var logged []LifecycleEvent
	result, err := pa.Run(context.Background(), RunOpts{Prompt: "review", CWD: cwd, JSONSchema: schema, OnLifecycle: piOutputPathMessages(&logged)})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("fallback output: %+v", result)
	}
	if len(logged) != 2 || logged[0].Phase != piLifecycleOutputPath || logged[1].Phase != LifecyclePhaseFallback ||
		!strings.Contains(logged[1].Message, "prompt-inlined schema") {
		t.Fatalf("step log must record the fallback: %+v", logged)
	}
	if runs := readPiRuns(t, cwd); runs != 2 {
		t.Fatalf("runs = %d, want the refused strict run plus the prompt run", runs)
	}

	logged = nil
	if _, err := pa.Run(context.Background(), RunOpts{Prompt: "review", CWD: cwd, JSONSchema: schema, OnLifecycle: piOutputPathMessages(&logged)}); err != nil {
		t.Fatal(err)
	}
	if runs := readPiRuns(t, cwd); runs != 3 {
		t.Fatalf("runs = %d; a known refusal must go straight to the prompt path", runs)
	}
	if len(logged) != 1 || !strings.Contains(logged[0].Message, "prompt-inlined schema (provider/model cannot take strict") {
		t.Fatalf("step log must name the cached prompt path: %+v", logged)
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
	// The embedded template is checked out with CRLF line endings on Windows.
	line, _, _ := strings.Cut(tail, "\n")
	parameters := strings.TrimSuffix(strings.TrimRight(line, "\r"), ";")
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

func TestPiAgent_StrictRunWithoutTheOutputToolIsASchemaRejection(t *testing.T) {
	good := piOutputEvents(`{"ok":true}`)
	for name, events := range map[string][]string{
		"plain text": piTextEvents(`{"ok":true}`),
		"batched then text": {
			strings.Replace(good[0], `"content":[`, `"content":[{"type":"toolCall","id":"read-1","name":"read"},`, 1),
			good[1],
			piTextEvents(`{"ok":true}`)[0],
		},
	} {
		t.Run(name, func(t *testing.T) {
			bin := writePiOutputFixture(t, piStrictVersion, events, piTextEvents(`{"ok":true}`), "")
			result, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
				Prompt: "review", CWD: t.TempDir(), JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
			})
			if err == nil || !IsStructuredOutputRejected(err) || !strings.Contains(err.Error(), "no_mistakes_output") {
				t.Fatalf("a strict run without the output tool must be a schema rejection: %v", err)
			}
			if result == nil || result.Output != nil || !result.UsageReported {
				t.Fatalf("rejection must keep usage and carry no output: %+v", result)
			}
		})
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
