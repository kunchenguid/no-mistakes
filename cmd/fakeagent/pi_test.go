package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRunPiEmitsJSONTextAndPinnedIdentity(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	status := runPi([]string{"--model", "openai-codex/gpt-5.4", "--thinking", "high"}, strings.NewReader("review"), defaultScenario())
	w.Close()
	var out bytes.Buffer
	out.ReadFrom(r)
	if status != 0 {
		t.Fatalf("status %d", status)
	}
	var event struct {
		Message struct {
			Model    string
			Provider string
			Content  []struct{ Text string }
		}
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("unexpected stream %s", out.String())
	}
	if err := json.Unmarshal(lines[1], &event); err != nil {
		t.Fatal(err)
	}
	if event.Message.Model != "gpt-5.4" || event.Message.Provider != "openai-codex" || len(event.Message.Content) != 1 || !json.Valid([]byte(event.Message.Content[0].Text)) {
		t.Fatalf("bad final message: %+v", event.Message)
	}
}
