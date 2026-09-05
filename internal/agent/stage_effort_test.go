package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func effortRecordingBinary(t *testing.T) string {
	t.Helper()
	return writeFakePi(t, t.TempDir(), `#!/bin/sh
cat >/dev/null
printf '%s\n' "$*" >> argv.txt
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}'
`, "@echo off\r\nmore > nul\r\necho %* >> argv.txt\r\necho {\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\r\n")
}

// These assertions read argv emitted by real adapter subprocess launches, not
// source text or a mocked dispatch decision. Non-Pi fixtures intentionally do
// not model successful wire output; their argv is the evidence under test.
func TestStageEffortNativeInvocations(t *testing.T) {
	for _, tc := range []struct {
		name types.AgentName
		flag string
	}{
		{types.AgentPi, "--thinking"}, {types.AgentClaude, "--effort"}, {types.AgentCodex, "model_reasoning_effort="}, {types.AgentGrok, "--reasoning-effort"}, {types.AgentCopilot, "--effort"},
	} {
		t.Run(string(tc.name), func(t *testing.T) {
			for _, rawPin := range []bool{false, true} {
				raw := []string(nil)
				if rawPin {
					if tc.name == types.AgentCodex {
						raw = []string{"-c", `model_reasoning_effort="low"`}
					} else {
						raw = []string{tc.flag, "low"}
					}
				}
				a, err := NewWithOptions(tc.name, effortRecordingBinary(t), raw, Options{Profile: agentcfg.Profile{Effort: "medium"}, StageEfforts: agentcfg.StageEfforts{"intent": "low", "rebase": "low", "review": "high", "review-fix": "medium", "test": "low", "document": "low", "lint": "low", "pr": "low", "ci": "low"}})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				for _, purpose := range []string{"intent", "rebase", "review", "review-fix", "review", "test-evidence", "test-fix", "document", "document-fix", "housekeeping", "lint", "lint-fix", "pr", "ci", "unknown"} {
					dir := t.TempDir()
					_, _ = a.Run(context.Background(), RunOpts{Prompt: "fixture", Purpose: purpose, CWD: dir})
					data, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
					if err != nil {
						t.Fatal(err)
					}
					want := "low"
					if purpose == "review-fix" || purpose == "unknown" {
						want = "medium"
					}
					if purpose == "review" {
						want = "high"
					}
					if rawPin {
						want = "low"
					}
					argv := string(data)
					pin := tc.flag + " " + want
					if tc.name == types.AgentCodex {
						quote := `"`
						if runtime.GOOS == "windows" {
							// cmd.exe's echo %* records Go's escaped command line,
							// rather than the decoded argv a native executable sees.
							quote = `\"`
						}
						pin = tc.flag + quote + want + quote
					}
					if !strings.Contains(argv, pin) {
						t.Fatalf("%s raw=%v: argv %q missing %q", purpose, rawPin, argv, pin)
					}
					for _, line := range strings.Split(strings.TrimSpace(argv), "\n") {
						if strings.Count(line, tc.flag) != 1 {
							t.Fatalf("duplicate effort: %s", line)
						}
					}
				}
			}
		})
	}
}

func TestStageEffortConcurrentInvocations(t *testing.T) {
	stages := agentcfg.StageEfforts{"review": "high", "review-fix": "medium"}
	a, err := NewWithOptions(types.AgentPi, effortRecordingBinary(t), nil, Options{Profile: agentcfg.Profile{Model: "unchanged", Effort: "low"}, StageEfforts: stages})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Construction must detach the caller's mutable map.
	stages["review"] = "low"
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		purpose, want := "review", "high"
		if i%2 == 1 {
			purpose, want = "review-fix", "medium"
		}
		dir := t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.Run(context.Background(), RunOpts{CWD: dir, Prompt: "fixture", Purpose: purpose})
			if err != nil {
				t.Error(err)
				return
			}
			if result.Text != "ok" {
				t.Errorf("result = %+v", result)
			}
			data, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(string(data), "--thinking "+want) || !strings.Contains(string(data), "--model unchanged") {
				t.Errorf("%s received %s", purpose, data)
			}
		}()
	}
	wg.Wait()
}

func TestStageEffortOpenCodeRequests(t *testing.T) {
	bodies := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"effort-session"}`)
		case r.URL.Path == "/global/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case strings.HasSuffix(r.URL.Path, "/message"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			bodies <- body
			fmt.Fprint(w, `{"info":{"id":"msg","role":"assistant","structured":{"ok":true}},"parts":[{"type":"text","text":"{\"ok\":true}"}]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	a, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{Profile: agentcfg.Profile{Model: "openai/same", Effort: "medium"}, StageEfforts: agentcfg.StageEfforts{"review": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	a.(*opencodeAgent).server = &managedServer{port: mustParsePort(server.URL)}
	for _, purpose := range []string{"review", "review-fix", "review", "test"} {
		_, err := a.Run(context.Background(), RunOpts{Prompt: "fixture", Purpose: purpose, CWD: t.TempDir(), JSONSchema: json.RawMessage(`{"type":"object"}`)})
		if err != nil {
			t.Fatal(err)
		}
		body := <-bodies
		want := "high"
		if purpose != "review" {
			want = "medium"
		}
		if body["variant"] != want {
			t.Fatalf("%s variant = %v", purpose, body["variant"])
		}
		if !reflect.DeepEqual(body["model"], map[string]any{"providerID": "openai", "modelID": "same"}) {
			t.Fatalf("model changed: %v", body["model"])
		}
	}
}

func TestStageEffortProviderFallback(t *testing.T) {
	primary, err := NewWithOptions(types.AgentClaude, filepath.Join(t.TempDir(), "absent"), nil, Options{StageEfforts: agentcfg.StageEfforts{"review": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := NewWithOptions(types.AgentPi, effortRecordingBinary(t), nil, Options{Profile: agentcfg.Profile{Effort: "medium"}, StageEfforts: agentcfg.StageEfforts{"review": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	a := NewFallback([]Agent{primary, secondary})
	defer a.Close()
	for _, purpose := range []string{"review", "review-fix", "review"} {
		dir := t.TempDir()
		result, err := a.Run(context.Background(), RunOpts{Prompt: "fixture", Purpose: purpose, CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		if result.Provider != "pi" {
			t.Fatalf("provider = %q", result.Provider)
		}
		data, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
		if err != nil {
			t.Fatal(err)
		}
		want := "high"
		if purpose == "review-fix" {
			want = "medium"
		}
		if !strings.Contains(string(data), "--thinking "+want) {
			t.Fatalf("fallback %s argv = %s", purpose, data)
		}
	}
}

func TestStageEffortCapabilitiesAndValidation(t *testing.T) {
	a, err := NewWithOptions(types.AgentPi, "pi", nil, Options{DisableProjectSettings: true, StageEfforts: agentcfg.StageEfforts{"review": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !SupportsSessionResume(a) || !SupportsSessionProvider(a, "pi") || !NeutralizesGateInstructions(a) {
		t.Fatal("wrapper hid adapter capabilities")
	}
	for _, name := range []types.AgentName{types.AgentRovoDev, types.AgentAntigravity, types.AgentCursor, "acp:custom"} {
		if a, err := NewWithOptions(name, "fixture", nil, Options{StageEfforts: agentcfg.StageEfforts{"review": "high"}}); err == nil {
			a.Close()
			t.Errorf("accepted unsupported %s", name)
		}
	}
}
