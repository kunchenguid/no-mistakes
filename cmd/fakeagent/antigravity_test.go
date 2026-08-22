package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractAgyPromptReadsPrintArgument(t *testing.T) {
	got, err := extractAgyPrompt([]string{"--dangerously-skip-permissions", "--print", "review this branch", "--output-format", "stream-json"})
	if err != nil {
		t.Fatalf("extractAgyPrompt() error = %v", err)
	}
	if got != "review this branch" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestExtractAgyPromptAcceptsShortAlias(t *testing.T) {
	got, err := extractAgyPrompt([]string{"-p", "short alias"})
	if err != nil {
		t.Fatalf("extractAgyPrompt() error = %v", err)
	}
	if got != "short alias" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestRunAgyEmitsStreamJSONResult(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	status := runAgy([]string{"--print", "do work", "--output-format", "stream-json"}, defaultScenario())
	_ = w.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(r)
	if status != 0 {
		t.Fatalf("runAgy() status = %d", status)
	}
	for _, want := range []string{`"event":"init"`, `"event":"step_update"`, `"event":"result"`, `"status":"SUCCESS"`, `"structured_output"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %s:\n%s", want, out.String())
		}
	}
}

func TestRunAgyReturnsErrorWhenConfiguredFixtureIsMissing(t *testing.T) {
	t.Setenv("FAKEAGENT_FIXTURE", t.TempDir())

	if status := runAgy([]string{"--print", "do work", "--output-format", "stream-json"}, defaultScenario()); status == 0 {
		t.Fatal("runAgy() status = 0, want configured fixture error")
	}
}

func TestPatchAgyFixtureRewritesContentAndKeepsEnvelope(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`{"event":"init","conversation_id":"fix","init":{"cwd":"/tmp","tools":["run_command"]}}`,
		`{"event":"step_update","step_update":{"step_index":2,"state":"DONE","step_type":"agent_response","text_delta":"recorded"}}`,
		`{"event":"result","result":{"conversation_id":"fix","status":"SUCCESS","response":"recorded","structured_output":{"ok":true},"usage":{"input_tokens":1}}}`,
		"",
	}, "\n"))

	patched, err := patchAgyFixture(raw, Action{
		Text: "scenario text",
		Structured: map[string]any{
			"findings": []any{},
			"summary":  "no issues found",
		},
	})
	if err != nil {
		t.Fatalf("patchAgyFixture() error = %v", err)
	}

	joined := string(patched)
	if !strings.Contains(joined, `"text_delta":"scenario text"`) {
		t.Fatalf("agent_response delta not patched:\n%s", joined)
	}
	if !strings.Contains(joined, `"response":"scenario text"`) {
		t.Fatalf("result response not patched:\n%s", joined)
	}
	if !strings.Contains(joined, `"summary":"no issues found"`) {
		t.Fatalf("structured_output not patched:\n%s", joined)
	}
	if strings.Contains(joined, `"cwd":"/tmp"`) == false || !strings.Contains(joined, `"usage":{"input_tokens":1}`) {
		t.Fatalf("envelope fields should stay untouched:\n%s", joined)
	}
}

func TestPatchAgyFixtureDropsStructuredOutputForPlainActions(t *testing.T) {
	raw := []byte(`{"event":"result","result":{"status":"SUCCESS","response":"x","structured_output":{"ok":true}}}` + "\n")

	patched, err := patchAgyFixture(raw, Action{Text: "plain prose"})
	if err != nil {
		t.Fatalf("patchAgyFixture() error = %v", err)
	}
	if strings.Contains(string(patched), "structured_output") {
		t.Fatalf("plain action should drop structured_output:\n%s", patched)
	}
}

func TestRunAgyReplaysRecordedFixture(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "antigravity")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureContents := map[string]string{
		"plain.jsonl":      `{"event":"result","result":{"conversation_id":"plain-recorded","status":"SUCCESS","response":"recorded"}}` + "\n",
		"structured.jsonl": `{"event":"result","result":{"conversation_id":"structured-recorded","status":"SUCCESS","response":"recorded","structured_output":{"ok":true}}}` + "\n",
	}
	for name, content := range fixtureContents {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKEAGENT_FIXTURE", dir)

	t.Run("structured", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		status := runAgy([]string{"--print", "work", "--json-schema", "{}", "--output-format", "stream-json"}, defaultScenario())
		_ = w.Close()
		os.Stdout = oldStdout
		var out bytes.Buffer
		_, _ = out.ReadFrom(r)
		if status != 0 {
			t.Fatalf("runAgy() status = %d", status)
		}
		if !strings.Contains(out.String(), `"conversation_id":"structured-recorded"`) {
			t.Fatalf("fixture replay should carry the recorded envelope:\n%s", out.String())
		}
		if !strings.Contains(out.String(), `"summary"`) {
			t.Fatalf("fixture replay should splice scenario structured output:\n%s", out.String())
		}
	})

	t.Run("plain", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		status := runAgy([]string{"--print", "work", "--output-format", "stream-json"}, defaultScenario())
		_ = w.Close()
		os.Stdout = oldStdout
		var out bytes.Buffer
		_, _ = out.ReadFrom(r)
		if status != 0 {
			t.Fatalf("runAgy() status = %d", status)
		}
		if !strings.Contains(out.String(), `"conversation_id":"plain-recorded"`) {
			t.Fatalf("plain fixture replay should carry the recorded envelope:\n%s", out.String())
		}
		if strings.Contains(out.String(), `"conversation_id":"structured-recorded"`) {
			t.Fatalf("plain call replayed the structured fixture:\n%s", out.String())
		}
		if !strings.Contains(out.String(), `"summary"`) {
			t.Fatalf("plain fixture replay should splice scenario structured output:\n%s", out.String())
		}
	})
}
