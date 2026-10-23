package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// Opt-in real-harness test: only the model provider is a loopback fixture.
// OpenCode must execute its actual shell tool, persist the session projection,
// and provide the final answer through the released V2 HTTP API. All OpenCode
// state/configuration lives in the test's isolated HOME, never the user's DB.
func TestOpencodeV2_RealHarnessExecutesToolAndReturnsValidatedOutput(t *testing.T) {
	bin := os.Getenv("NO_MISTAKES_OPENCODE_V2_BIN")
	if bin == "" {
		t.Skip("set NO_MISTAKES_OPENCODE_V2_BIN to an absolute OpenCode 2 binary")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("real-harness test requires an absolute OpenCode binary")
	}
	root := t.TempDir()
	previousOutput := currentManagedServerOutput()
	SetManagedServerOutput(io.Discard)
	t.Cleanup(func() { SetManagedServerOutput(previousOutput) })
	var frames bytes.Buffer
	previousTransport := opencodeV2HTTP.Transport
	opencodeV2HTTP.Transport = opencodeV2RecordingTransport{base: http.DefaultTransport, frames: &frames}
	t.Cleanup(func() { opencodeV2HTTP.Transport = previousTransport })
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(project, "actual-tool-effect.txt")
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected provider route %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var input struct {
			Model string `json:"model"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if input.Model != "fixture" {
			t.Errorf("requested provider model = %q", input.Model)
		}
		call := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(delta any, finish any) {
			value := map[string]any{"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		if call == 1 {
			hasShell := false
			for _, tool := range input.Tools {
				if tool.Function.Name == "shell" {
					hasShell = true
				}
			}
			if !hasShell {
				t.Error("real session did not expose the shell tool")
			}
			arguments, _ := json.Marshal(map[string]any{"command": "test -z \"${OPENCODE_PASSWORD:-}\" && test -z \"${OPENCODE_SERVER_PASSWORD:-}\" && printf 'fixture-effect' > actual-tool-effect.txt"})
			emit(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_fixture", "type": "function", "function": map[string]any{"name": "shell", "arguments": string(arguments)}}}}, nil)
			emit(map[string]any{}, "tool_calls")
		} else {
			emit(map[string]any{"role": "assistant", "content": `{"summary":"real tool completed"}`}, nil)
			emit(map[string]any{}, "stop")
		}
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	config := map[string]any{
		"model":  "nm-fixture/fixture",
		"agents": map[string]any{"title": map[string]any{"disabled": true}},
		"providers": map[string]any{"nm-fixture": map[string]any{
			"package":  "@opencode/ai/providers/openai-compatible",
			"settings": map[string]any{"baseURL": provider.URL + "/v1", "apiKey": "non-secret-local-fixture"},
			"models":   map[string]any{"fixture": map[string]any{"limit": map[string]int{"context": 128000, "output": 4096}, "capabilities": map[string]any{"tools": true}}},
		}},
	}
	encoded, _ := json.Marshal(config)
	env := []string{
		"HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"OPENCODE_CONFIG_DIR=" + filepath.Join(root, "config", "opencode"), "OPENCODE_CONFIG_CONTENT=" + string(encoded),
	}
	a := &opencodeAgent{bin: bin, profile: agentcfg.Profile{Model: "nm-fixture/fixture"}}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var progress strings.Builder
	result, err := a.Run(ctx, RunOpts{CWD: project, Env: env, Prompt: "Execute the requested fixture tool, then return the requested JSON.", JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`), OnChunk: func(text string) { progress.WriteString(text) }})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "fixture-effect" {
		t.Fatalf("real tool effect missing: %q, %v", content, err)
	}
	if string(result.Output) != `{"summary":"real tool completed"}` {
		t.Fatalf("wrong validated result: %s", result.Output)
	}
	if requests.Load() != 2 {
		t.Fatalf("expected one tool step and one final step; got %d requests", requests.Load())
	}
	if !result.UsageReported || result.Usage.OutputTokens != 14 {
		t.Fatalf("both actual provider steps were not accounted: %+v", result.Usage)
	}
	if !strings.Contains(progress.String(), "real tool completed") {
		var types []string
		_ = parseSSE(bytes.NewReader(frames.Bytes()), func(event sseEvent) bool {
			var value map[string]json.RawMessage
			if err := json.Unmarshal([]byte(event.Data), &value); err != nil {
				types = append(types, "unparseable-frame")
				return true
			}
			var kind string
			_ = json.Unmarshal(value["type"], &kind)
			types = append(types, kind)
			return true
		})
		t.Fatalf("real V2 progress did not stream: %q; observed event types: %v", progress.String(), types)
	}
	server := a.server
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.exited:
	case <-time.After(time.Second):
		t.Fatal("owned OpenCode process survived adapter cleanup")
	}
}

type opencodeV2RecordingTransport struct {
	base   http.RoundTripper
	frames io.Writer
}

func (t opencodeV2RecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err == nil && req.URL.Path == "/api/event" {
		response.Body = &opencodeV2RecordedBody{Reader: io.TeeReader(response.Body, t.frames), Closer: response.Body}
	}
	return response, err
}

type opencodeV2RecordedBody struct {
	io.Reader
	io.Closer
}
