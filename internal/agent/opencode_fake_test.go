package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpencodePassword is the basic-auth secret the fake expects; the agent
// under test is built with the same value, so a request that forgets the
// header answers 401 exactly like the real server.
const fakeOpencodePassword = "test-password"

// fakeOpencodeTurn scripts what the fake does with one admitted prompt.
type fakeOpencodeTurn struct {
	// events are the event frames (JSON objects, no SSE framing) published
	// after the prompt is admitted. "{SID}" is replaced by the session id.
	events []string
	// messages is the JSON array served by GET /api/session/{id}/message,
	// with "{SID}" replaced. Empty serves an empty list.
	messages string
	// messagesStatus, when non-zero, is the status GET .../message answers
	// with instead of the list.
	messagesStatus int
	// messagesDelay holds the message list back, for turns whose record
	// must arrive after the stream is gone.
	messagesDelay time.Duration
	// promptStatus, when non-zero, is the status POST .../prompt answers
	// with instead of admitting the prompt.
	promptStatus int
	// dropStream closes the event stream after events instead of holding it
	// open, the way a server that died mid-turn does.
	dropStream bool
	// waitStatus, when non-zero, is the status of POST .../wait; 0 answers 204.
	waitStatus int
}

// fakeOpencode stands in for `opencode serve` 2.x in adapter tests: the
// authenticated /api tree, the data envelope, the admit-then-stream prompt
// contract and the message list. Each admitted prompt plays the next scripted
// turn; the last script repeats for any further prompt.
type fakeOpencode struct {
	*httptest.Server
	t     *testing.T
	turns []fakeOpencodeTurn

	mu            sync.Mutex
	calls         []string
	sessionBodies []map[string]any
	prompts       []string
	promptSession []string
	subscribers   []chan fakeOpencodeFrame
}

// fakeOpencodeFrame is one SSE frame or the end of the stream.
type fakeOpencodeFrame struct {
	data  string
	close bool
}

func newFakeOpencode(t *testing.T, turns ...fakeOpencodeTurn) *fakeOpencode {
	t.Helper()
	f := &fakeOpencode{t: t, turns: turns}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// agent builds an adapter already bound to the fake, the way ensureServer
// leaves it: server port and password set, no process to spawn.
func (f *fakeOpencode) agent() *opencodeAgent {
	return &opencodeAgent{
		bin:      "opencode",
		server:   &managedServer{port: mustParsePort(f.URL)},
		password: fakeOpencodePassword,
	}
}

func (f *fakeOpencode) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
}

// called reports how many requests matched "METHOD /path".
func (f *fakeOpencode) called(methodPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == methodPath {
			n++
		}
	}
	return n
}

// calledMatching reports how many requests matched METHOD and a path prefix.
func (f *fakeOpencode) calledMatching(method, pathPrefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" "+pathPrefix) {
			n++
		}
	}
	return n
}

func (f *fakeOpencode) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessionBodies)
}

func (f *fakeOpencode) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeOpencode) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *fakeOpencode) lastSessionBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessionBodies) == 0 {
		return nil
	}
	return f.sessionBodies[len(f.sessionBodies)-1]
}

func (f *fakeOpencode) turnFor(promptIndex int) fakeOpencodeTurn {
	if len(f.turns) == 0 {
		return fakeOpencodeTurn{}
	}
	if promptIndex >= len(f.turns) {
		return f.turns[len(f.turns)-1]
	}
	return f.turns[promptIndex]
}

func (f *fakeOpencode) serve(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		f.t.Errorf("fake opencode: request outside /api: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	user, password, ok := r.BasicAuth()
	if !ok || user != opencodeAuthUser || password != fakeOpencodePassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/")
	parts := strings.Split(path, "/")
	switch {
	case path == "info" && r.Method == http.MethodGet:
		fmt.Fprint(w, `{"version":"2.0.16","pid":1,"urls":[]}`)

	case path == "event" && r.Method == http.MethodGet:
		f.serveEvents(w, r)

	case path == "session" && r.Method == http.MethodPost:
		f.serveCreateSession(w, r)

	case len(parts) == 2 && parts[0] == "session" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)

	case len(parts) == 3 && parts[0] == "session" && parts[2] == "prompt" && r.Method == http.MethodPost:
		f.servePrompt(w, r, parts[1])

	case len(parts) == 3 && parts[0] == "session" && parts[2] == "message" && r.Method == http.MethodGet:
		f.serveMessages(w, r, parts[1])

	case len(parts) == 3 && parts[0] == "session" && parts[2] == "interrupt" && r.Method == http.MethodPost:
		fmt.Fprint(w, `{"interrupted":true}`)

	case len(parts) == 4 && parts[0] == "experimental" && parts[1] == "session" && parts[3] == "wait" && r.Method == http.MethodPost:
		turn := f.turnFor(f.promptCount() - 1)
		if turn.waitStatus != 0 {
			w.WriteHeader(turn.waitStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		f.t.Errorf("fake opencode: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeOpencode) serveCreateSession(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake opencode: session body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.sessionBodies = append(f.sessionBodies, body)
	id := fmt.Sprintf("ses_%d", len(f.sessionBodies))
	f.mu.Unlock()
	fmt.Fprintf(w, `{"data":{"id":%q,"projectID":"global","time":{"created":1,"updated":1}}}`, id)
}

func (f *fakeOpencode) serveEvents(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") != "text/event-stream" {
		f.t.Error("fake opencode: event stream request without Accept: text/event-stream")
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ch := make(chan fakeOpencodeFrame, 64)
	f.mu.Lock()
	f.subscribers = append(f.subscribers, ch)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		for i, sub := range f.subscribers {
			if sub == ch {
				f.subscribers = append(f.subscribers[:i], f.subscribers[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"evt_0\",\"type\":\"server.connected\",\"data\":{}}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-ch:
			if frame.close {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", frame.data)
			flusher.Flush()
		}
	}
}

func (f *fakeOpencode) publish(frame fakeOpencodeFrame) {
	f.mu.Lock()
	subs := append([]chan fakeOpencodeFrame(nil), f.subscribers...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- frame:
		case <-time.After(time.Second):
			f.t.Error("fake opencode: subscriber did not drain")
		}
	}
}

func (f *fakeOpencode) servePrompt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake opencode: prompt body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.prompts = append(f.prompts, body.Text)
	f.promptSession = append(f.promptSession, sessionID)
	index := len(f.prompts) - 1
	f.mu.Unlock()

	turn := f.turnFor(index)
	if turn.promptStatus != 0 {
		w.WriteHeader(turn.promptStatus)
		fmt.Fprintf(w, `{"_tag":"Error","message":"scripted prompt failure %d"}`, turn.promptStatus)
		return
	}
	fmt.Fprintf(w, `{"data":{"id":"msg_user_%d","sessionID":%q,"type":"user","payload":{"text":%q},"delivery":"steer"}}`,
		index+1, sessionID, body.Text)

	// The subscriber was registered before the prompt was admitted, exactly
	// as the adapter orders it, so the frames can go out synchronously.
	for _, ev := range turn.events {
		f.publish(fakeOpencodeFrame{data: strings.ReplaceAll(ev, "{SID}", sessionID)})
	}
	if turn.dropStream {
		f.publish(fakeOpencodeFrame{close: true})
	}
}

func (f *fakeOpencode) serveMessages(w http.ResponseWriter, r *http.Request, sessionID string) {
	turn := f.turnFor(f.promptCount() - 1)
	if turn.messagesDelay > 0 {
		select {
		case <-time.After(turn.messagesDelay):
		case <-r.Context().Done():
			return
		}
	}
	if turn.messagesStatus != 0 {
		w.WriteHeader(turn.messagesStatus)
		return
	}
	messages := turn.messages
	if messages == "" {
		messages = "[]"
	}
	fmt.Fprintf(w, `{"data":%s,"cursor":{"previous":null,"next":null}}`, strings.ReplaceAll(messages, "{SID}", sessionID))
}

// runFakeOpencode drives one structured turn against the fake.
func runFakeOpencode(t *testing.T, f *fakeOpencode) (*Result, error) {
	t.Helper()
	return f.agent().Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
}

// Event frame builders, in opencode 2.x's flat {"id","type","data"} shape.

func ocEvent(eventType, data string) string {
	return fmt.Sprintf(`{"id":"evt_x","type":%q,"data":%s}`, eventType, data)
}

func ocTextDelta(msgID string, ordinal int, delta string) string {
	return ocEvent("session.text.delta", fmt.Sprintf(`{"sessionID":"{SID}","assistantMessageID":%q,"ordinal":%d,"delta":%q}`, msgID, ordinal, delta))
}

func ocTextEnded(msgID string, ordinal int, text string) string {
	return ocEvent("session.text.ended", fmt.Sprintf(`{"sessionID":"{SID}","assistantMessageID":%q,"ordinal":%d,"text":%q}`, msgID, ordinal, text))
}

func ocStepEnded(msgID string, input, output int) string {
	return ocEvent("session.step.ended", fmt.Sprintf(`{"sessionID":"{SID}","assistantMessageID":%q,"finish":"stop","tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}`, msgID, input, output))
}

func ocToolCalled(msgID string) string {
	return ocEvent("session.tool.called", fmt.Sprintf(`{"sessionID":"{SID}","assistantMessageID":%q,"id":"toolu_1","name":"shell","input":{"command":"ls"},"executed":false}`, msgID))
}

func ocSucceeded() string {
	return ocEvent("session.execution.succeeded", `{"sessionID":"{SID}"}`)
}

func ocFailed(errJSON string) string {
	return ocEvent("session.execution.failed", fmt.Sprintf(`{"sessionID":"{SID}","error":%s}`, errJSON))
}

func ocInterrupted(reason string) string {
	return ocEvent("session.execution.interrupted", fmt.Sprintf(`{"sessionID":"{SID}","reason":%q}`, reason))
}

// ocTextTurn is the event sequence of a one-step text-only turn.
func ocTextTurn(msgID, text string, input, output int) []string {
	return []string{
		ocTextDelta(msgID, 0, text),
		ocTextEnded(msgID, 0, text),
		ocStepEnded(msgID, input, output),
		ocSucceeded(),
	}
}

// Message list builders. created increases with each call so the sorted
// list keeps the order the builders were called in.

type ocMessageBuilder struct {
	created int64
	items   []string
}

func ocMessages() *ocMessageBuilder { return &ocMessageBuilder{created: 1000} }

func (b *ocMessageBuilder) next() int64 {
	b.created++
	return b.created
}

func (b *ocMessageBuilder) user(text string) *ocMessageBuilder {
	b.items = append(b.items, fmt.Sprintf(`{"id":"msg_user","type":"user","text":%q,"time":{"created":%d}}`, text, b.next()))
	return b
}

func (b *ocMessageBuilder) assistantText(msgID, text string, input, output int) *ocMessageBuilder {
	b.items = append(b.items, fmt.Sprintf(`{"id":%q,"type":"assistant","agent":"build","content":[{"type":"text","text":%q}],"finish":"stop","tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":%d}}`, msgID, text, input, output, b.next()))
	return b
}

func (b *ocMessageBuilder) assistantTool(msgID string) *ocMessageBuilder {
	b.items = append(b.items, fmt.Sprintf(`{"id":%q,"type":"assistant","agent":"build","content":[{"type":"tool","id":"toolu_1","name":"shell","state":{"status":"completed"}}],"finish":"tool-calls","tokens":{"input":2,"output":9,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":%d}}`, msgID, b.next()))
	return b
}

func (b *ocMessageBuilder) assistantError(msgID, errJSON string) *ocMessageBuilder {
	b.items = append(b.items, fmt.Sprintf(`{"id":%q,"type":"assistant","agent":"build","content":[],"error":%s,"time":{"created":%d}}`, msgID, errJSON, b.next()))
	return b
}

func (b *ocMessageBuilder) idle(outcome string) *ocMessageBuilder {
	b.items = append(b.items, fmt.Sprintf(`{"id":"msg_idle","type":"idle","outcome":%q,"time":{"created":%d}}`, outcome, b.next()))
	return b
}

// newestFirst returns the list in the order the real endpoint serves it.
func (b *ocMessageBuilder) newestFirst() string {
	reversed := make([]string, 0, len(b.items))
	for i := len(b.items) - 1; i >= 0; i-- {
		reversed = append(reversed, b.items[i])
	}
	return "[" + strings.Join(reversed, ",") + "]"
}

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// withFastEvidenceWait shortens the bounded wait a failed turn spends on its
// session settling, so the unverifiable-turn paths run in test time.
func withFastEvidenceWait(t *testing.T) func() {
	t.Helper()
	previous := opencodeEvidenceWait
	opencodeEvidenceWait = 200 * time.Millisecond
	return func() { opencodeEvidenceWait = previous }
}
