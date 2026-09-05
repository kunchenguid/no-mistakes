package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOpenCodeStageEffortSharedServerConcurrentReplay(t *testing.T) {
	for _, mode := range []string{"success", "retry", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			var sessions, deleted, arrived atomic.Int32
			var mu sync.Mutex
			attempts := map[string]int{}
			seenSessions := map[string]bool{}
			ready := make(chan struct{})
			wants := map[string]string{"review": "high", "review-fix": "medium", "test": "low"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/session" && r.Method == http.MethodPost:
					fmt.Fprintf(w, `{"id":"s%d"}`, sessions.Add(1))
				case r.URL.Path == "/global/event":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
				case strings.HasSuffix(r.URL.Path, "/message"):
					var body struct {
						Variant string            `json:"variant"`
						Model   map[string]string `json:"model"`
						Parts   []struct {
							Text string `json:"text"`
						} `json:"parts"`
						Info struct {
							Format json.RawMessage `json:"format"`
						} `json:"info"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					purpose := ""
					for _, part := range body.Parts {
						for candidate := range wants {
							if strings.Contains(part.Text, "duty="+candidate+";") {
								purpose = candidate
							}
						}
					}
					if purpose == "" {
						t.Error("request lost duty prompt")
						return
					}
					if body.Variant != wants[purpose] {
						t.Errorf("%s variant = %q, want %q", purpose, body.Variant, wants[purpose])
					}
					if !reflect.DeepEqual(body.Model, map[string]string{"providerID": "openai", "modelID": "same"}) {
						t.Errorf("model = %v", body.Model)
					}
					mu.Lock()
					attempts[purpose]++
					attempt := attempts[purpose]
					if seenSessions[r.URL.Path] {
						t.Error("session reused across requests")
					}
					seenSessions[r.URL.Path] = true
					mu.Unlock()
					if attempt == 1 {
						if arrived.Add(1) == 3 {
							close(ready)
						}
						select {
						case <-ready:
						case <-r.Context().Done():
							return
						}
						switch mode {
						case "retry":
							fmt.Fprint(w, `{"info":{"error":{"name":"APIError","data":{"message":"temporary failure","statusCode":503,"isRetryable":true}}},"parts":[]}`)
							return
						case "fallback":
							if len(body.Info.Format) == 0 {
								t.Error("native schema missing")
							}
							fmt.Fprint(w, `{"info":{"error":{"name":"APIError","data":{"message":"Thinking may not be enabled when tool_choice forces tool use."}}},"parts":[]}`)
							return
						}
					}
					if mode == "fallback" && attempt == 2 && len(body.Info.Format) != 0 {
						t.Error("fallback retained native schema")
					}
					fmt.Fprint(w, `{"info":{"id":"msg","role":"assistant"},"parts":[{"type":"text","text":"{\"ok\":true}"}]}`)
				case r.Method == http.MethodDelete:
					deleted.Add(1)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			stages := agentcfg.StageEfforts{"review": "high", "review-fix": "medium"}
			a, err := NewWithOptions(types.AgentOpenCode, filepath.Join(dir, "must-not-start-another-server"), nil, Options{Profile: agentcfg.Profile{Model: "openai/same", Effort: "low"}, StageEfforts: stages})
			if err != nil {
				t.Fatal(err)
			}
			a.(*opencodeAgent).server = &managedServer{port: mustParsePort(server.URL)}
			stages["review"] = "low"
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			for purpose := range wants {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := a.Run(ctx, RunOpts{Prompt: "duty=" + purpose + ";", Purpose: purpose, CWD: dir, JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)})
					if err != nil {
						t.Errorf("%s: %v", purpose, err)
						return
					}
					if string(result.Output) != `{"ok":true}` {
						t.Errorf("%s output = %q", purpose, result.Output)
					}
				}()
			}
			wg.Wait()
			count := 1
			if mode != "success" {
				count = 2
			}
			for purpose := range wants {
				if attempts[purpose] != count {
					t.Errorf("%s attempts = %d, want %d", purpose, attempts[purpose], count)
				}
			}
			if sessions.Load() != int32(3*count) || deleted.Load() != sessions.Load() {
				t.Errorf("sessions created=%d deleted=%d", sessions.Load(), deleted.Load())
			}
		})
	}
}
