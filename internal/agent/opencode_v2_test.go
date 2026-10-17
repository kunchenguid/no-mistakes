package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// These fixtures implement the published V2 OpenAPI and session-event contract,
// not the V1 adapter's wire types. The full pipeline fixture and opt-in real
// OpenCode smoke test cover process startup independently of this HTTP boundary.
type opencodeV2Fixture struct {
	mu           sync.Mutex
	calls        []string
	created      map[string]json.RawMessage
	prompt       string
	outcome      string
	pages        []string
	events       string
	defaultModel bool
	wait         func(http.ResponseWriter, *http.Request)
}

func (f *opencodeV2Fixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /api/model/default":
			if r.URL.Query().Get("location[directory]") == "" {
				t.Error("default model lookup lost its worktree location")
			}
			if f.defaultModel {
				fmt.Fprint(w, `{"data":{"providerID":"provider-fixture","id":"default-fixture"}}`)
			} else {
				fmt.Fprint(w, `{"data":null}`)
			}
		case "POST /api/session":
			if err := json.NewDecoder(r.Body).Decode(&f.created); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{"data":{"id":"ses_fixture"}}`)
		case "GET /api/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, f.events)
			// Ending the volatile stream is deliberate. Only the wait/projection
			// boundary may establish the final answer or absence of tool effects.
		case "POST /api/session/ses_fixture/prompt":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body) != 1 {
				t.Errorf("V2 prompt must not carry ignored V1 format/model fields: %v", body)
			}
			if err := json.Unmarshal(body["text"], &f.prompt); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{"data":{"id":"msg_prompt","type":"user"}}`)
		case "POST /api/experimental/session/ses_fixture/wait":
			if f.wait != nil {
				f.wait(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "GET /api/session/ses_fixture":
			outcome := f.outcome
			if outcome == "" {
				outcome = "succeeded"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": "ses_fixture", "outcome": outcome}})
		case "GET /api/session/ses_fixture/message":
			index := 0
			if r.URL.Query().Get("cursor") != "" {
				if r.URL.Query().Get("order") != "" {
					t.Error("cursor requests must not repeat order")
				}
				if r.URL.Query().Get("cursor") != "page-2" {
					t.Error("unexpected cursor")
				}
				index = 1
			} else if r.URL.Query().Get("order") != "asc" {
				t.Error("messages must be read chronologically")
			}
			if index >= len(f.pages) {
				t.Error("unexpected projection page")
				http.Error(w, "bad page", 500)
				return
			}
			fmt.Fprint(w, f.pages[index])
		case "POST /api/session/ses_fixture/interrupt":
			fmt.Fprint(w, `{"interrupted":false}`)
		case "DELETE /api/session/ses_fixture":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected protocol request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "V1 API unavailable", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *opencodeV2Fixture) count(methodPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call == methodPath {
			n++
		}
	}
	return n
}

func v2Page(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"data":[{"id":"msg_final","type":"assistant","content":[{"type":"reasoning","text":"not output"},{"type":"text","text":` + string(encoded) + `}],"tokens":{"input":12,"output":5,"cache":{"read":3,"write":2}}}],"cursor":{"next":null}}`
}

func v2Agent(server *httptest.Server) *opencodeAgent {
	return &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}, v2: true}
}

func TestOpencodeV2_FullLifecycleUsesAsyncCompletionAndValidatesFinalJSON(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{v2Page(`{"summary":"final only"}`)}, events: `data: {"type":"session.text.delta","data":{"sessionID":"ses_other","delta":"FOREIGN"}}

data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"delta":"progress"}}

`}
	a := v2Agent(f.server(t))
	a.profile = agentcfg.Profile{Model: "provider-fixture/model-fixture", Effort: "high"}
	var chunks strings.Builder
	result, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review the actual change", JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`), OnChunk: func(s string) { chunks.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"summary":"final only"}` {
		t.Fatalf("wrong authoritative output: %s", result.Output)
	}
	// The streamed delta "progress" is buffered and replaced by the complete
	// fetched text at final reconciliation, so displayed progress is complete
	// rather than a stale fragment.
	if chunks.String() != `{"summary":"final only"}` {
		t.Fatalf("wrong streamed text: %q", chunks.String())
	}
	if result.Usage.InputTokens != 17 || result.Usage.OutputTokens != 5 || result.Usage.CacheReadTokens != 3 || result.Usage.CacheCreationTokens != 2 {
		t.Fatalf("wrong usage: %+v", result.Usage)
	}
	var model opencodeV2Model
	if err := json.Unmarshal(f.created["model"], &model); err != nil {
		t.Fatal(err)
	}
	if model.ID != "model-fixture" || model.ProviderID != "provider-fixture" || model.Variant != "high" {
		t.Fatalf("model selection lost: %+v", model)
	}
	if !strings.Contains(f.prompt, "review the actual change") || !strings.Contains(f.prompt, `"required":["summary"]`) {
		t.Fatalf("emitted schema instruction missing: %s", f.prompt)
	}
	for _, call := range []string{"POST /api/session", "POST /api/session/ses_fixture/prompt", "POST /api/experimental/session/ses_fixture/wait", "POST /api/session/ses_fixture/interrupt", "DELETE /api/session/ses_fixture"} {
		if f.count(call) != 1 {
			t.Errorf("%s calls = %d", call, f.count(call))
		}
	}
}

func TestOpencodeV2_EffortOnlyUsesTheLocationsDefaultModel(t *testing.T) {
	f := &opencodeV2Fixture{defaultModel: true, pages: []string{v2Page("OK")}}
	a := v2Agent(f.server(t))
	a.profile = agentcfg.Profile{Effort: "high"}
	_, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	var model opencodeV2Model
	if err := json.Unmarshal(f.created["model"], &model); err != nil {
		t.Fatal(err)
	}
	if model.ID != "default-fixture" || model.Variant != "high" {
		t.Fatalf("effort default model lost: %+v", model)
	}
}

func TestOpencodeV2_ProviderFailureCannotBecomeAnAnswerOrAReplayedTurn(t *testing.T) {
	f := &opencodeV2Fixture{outcome: "failed", pages: []string{`{"data":[{"id":"msg_failed","type":"assistant","content":[{"type":"text","text":"{\"summary\":\"looks valid\"}"}],"error":{"type":"provider","message":"503 temporary rate limit","status":503}}],"cursor":{}}`}}
	_, err := v2Agent(f.server(t)).Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "do work", JSONSchema: json.RawMessage(`{"type":"object"}`)})
	if err == nil || !strings.Contains(err.Error(), "503 temporary rate limit") {
		t.Fatalf("provider error lost: %v", err)
	}
	if f.count("POST /api/session/ses_fixture/prompt") != 1 {
		t.Fatal("admitted failure replayed")
	}
}

func TestOpencodeV2_PaginationFindsEarlierToolEffectsBeforeRejectingFinalOutput(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{
		`{"data":[{"id":"msg_tool","type":"assistant","content":[{"type":"tool","id":"tool_effect","name":"shell","state":{"status":"completed"}}],"tokens":{"input":10,"output":2}}],"cursor":{"next":"page-2"}}`,
		v2Page("503 temporarily unavailable"),
	}}
	result, err := v2Agent(f.server(t)).Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "do work", JSONSchema: json.RawMessage(`{"type":"object"}`)})
	if err == nil || !IsStructuredOutputRejected(err) || !opencodeReplayUnsafe(err) {
		t.Fatalf("schema failure lost tool-evidence fence: %v", err)
	}
	if result == nil || result.Usage.InputTokens != 27 {
		t.Fatalf("earlier usage lost: %+v", result)
	}
	if f.count("GET /api/session/ses_fixture/message") != 2 || f.count("POST /api/session/ses_fixture/prompt") != 1 {
		t.Fatal("projection incomplete or turn replayed")
	}
}

func TestOpencodeV2_CancellationInterruptsAndDeletesOnlyItsOwnedSession(t *testing.T) {
	entered := make(chan struct{})
	f := &opencodeV2Fixture{wait: func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }}
	a := v2Agent(f.server(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.Run(ctx, RunOpts{CWD: t.TempDir(), Prompt: "work"}); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("did not reach running wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation hung")
	}
	if f.count("POST /api/session/ses_fixture/interrupt") != 1 || f.count("DELETE /api/session/ses_fixture") != 1 {
		t.Fatal("owned execution was not cleaned up")
	}
}

func TestOpencodeV2_ProjectionWithoutContentCannotProveNoTools(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{`{"data":[{"id":"msg_bad","type":"assistant"}],"cursor":{}}`}}
	_, err := v2Agent(f.server(t)).Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "work"})
	if err == nil || !opencodeReplayUnsafe(err) {
		t.Fatalf("incomplete evidence did not fail closed: %v", err)
	}
}

func TestOpencodeV2_BackfillsOnlyTheMissingFinalStreamSuffix(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{v2Page("prefix and final suffix")}, events: `data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"delta":"prefix"}}

`}
	var chunks strings.Builder
	result, err := v2Agent(f.server(t)).Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "work", OnChunk: func(text string) { chunks.WriteString(text) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "prefix and final suffix" || chunks.String() != result.Text {
		t.Fatalf("stream/projection reconciliation duplicated or lost output: %q / %q", chunks.String(), result.Text)
	}
}

func TestOpencodeV2_RecoversDisplayedProgressWhenTheFirstDeltaIsMissing(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{v2Page("hello world")}, events: `data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"delta":"world"}}

data: {"type":"session.text.ended","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"text":"hello world"}}

`}
	var chunks strings.Builder
	result, err := v2Agent(f.server(t)).Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "work", OnChunk: func(text string) { chunks.WriteString(text) }})
	if err != nil {
		t.Fatal(err)
	}
	// The authoritative answer is unchanged by stream repair.
	if result.Text != "hello world" {
		t.Fatalf("wrong authoritative answer: %q", result.Text)
	}
	if chunks.String() != "hello world" {
		t.Fatalf("streamed progress did not recover the missing first delta: %q", chunks.String())
	}
}

func TestOpencodeV2_ReportsActivityWhileBufferingDeltas(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{v2Page("hello world")}, events: `data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"delta":"hello"}}

data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"delta":" world"}}

data: {"type":"session.text.ended","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"text":"hello world"}}

`}
	a := v2Agent(f.server(t))
	var chunks strings.Builder
	var lifecycle []LifecycleEvent
	_, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "work", OnChunk: func(text string) { chunks.WriteString(text) }, OnLifecycle: func(event LifecycleEvent) { lifecycle = append(lifecycle, event) }})
	if err != nil {
		t.Fatal(err)
	}
	// The authoritative answer and displayed progress are complete.
	if chunks.String() != "hello world" {
		t.Fatalf("expected complete text after the end event: %q", chunks.String())
	}
	// Activity must be reported while deltas are buffered, before the end.
	var sawActivity bool
	for _, event := range lifecycle {
		if event.Phase == LifecyclePhaseActivity {
			sawActivity = true
		}
	}
	if !sawActivity {
		t.Fatal("expected LifecyclePhaseActivity while buffering deltas")
	}
}

func TestOpencodeV2_EmitsUnfinishedPartsInOriginalOrderFromCompleteText(t *testing.T) {
	// Two text parts stream without any session.text.ended, so the final
	// flush must emit them in ordinal order from the complete fetched text
	// rather than ranging over the unordered parts map.
	f := &opencodeV2Fixture{pages: []string{`{"data":[{"id":"msg_final","type":"assistant","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}],"tokens":{"input":12,"output":5}}],"cursor":{"next":null}}`},
		events: `data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":0,"delta":"hello "}}

data: {"type":"session.text.delta","data":{"sessionID":"ses_fixture","assistantMessageID":"msg_final","ordinal":1,"delta":"world"}}

`}
	var chunks strings.Builder
	result, err := v2Agent(f.server(t)).Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "work", OnChunk: func(text string) { chunks.WriteString(text) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello world" {
		t.Fatalf("wrong authoritative answer: %q", result.Text)
	}
	if chunks.String() != "hello world" {
		t.Fatalf("unfinished parts not emitted in original order from complete text: %q", chunks.String())
	}
}

func TestOpencodeV2_RedirectCannotReceiveThePrivateServerCredential(t *testing.T) {
	var forwarded atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Store(true) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic fixture-credential" {
			t.Error("owned server did not receive authentication")
		}
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		fmt.Fprint(w, "fixture-credential must not appear in errors")
	}))
	defer origin.Close()
	client := opencodeV2Client{authorization: "Basic fixture-credential"}
	err := client.json(context.Background(), http.MethodGet, origin.URL+"/api/info", nil, nil)
	if err == nil || forwarded.Load() || strings.Contains(err.Error(), "fixture-credential") {
		t.Fatalf("unsafe redirect or error disclosure: forwarded=%v err=%v", forwarded.Load(), err)
	}
}
