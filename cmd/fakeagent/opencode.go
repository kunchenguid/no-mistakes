package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runOpencode boots a long-running HTTP server that mimics opencode 2.x's
// `/api` surface: basic-auth guarded routes, `data` envelopes, a prompt that
// is admitted and then played out on the event stream, and a message list
// the adapter reads the turn back from. It blocks until the parent
// (no-mistakes' agent package) signals shutdown. The wire format is
// documented in internal/agent/opencode_types.go.
func runOpencode(args []string, scenario *Scenario) int {
	port, err := extractOpencodePort(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	srv := newFakeOpencodeServer(scenario)
	httpServer := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "fakeagent: opencode listen: %v\n", err)
		return 1
	}
	return 0
}

func extractOpencodePort(args []string) (int, error) {
	for i, a := range args {
		switch {
		case a == "--port" && i+1 < len(args):
			return strconv.Atoi(args[i+1])
		case strings.HasPrefix(a, "--port="):
			return strconv.Atoi(strings.TrimPrefix(a, "--port="))
		}
	}
	return 0, fmt.Errorf("fakeagent: opencode: --port not provided")
}

type fakeOpencodeServer struct {
	scenario   *Scenario
	fixture    *opencodeFixture // nil = synthetic mode
	fixtureErr error
	// password is the basic-auth secret the real server would have adopted
	// from OPENCODE_SERVER_PASSWORD. Empty (no variable set) disables the
	// check, which only in-process tests rely on; the daemon always sets it.
	password string

	mu          sync.Mutex
	subscribers []chan []byte // active /api/event listeners (one per request)
	sessionDirs map[string]string
	sessionSeq  int
	msgSeq      int
	// lastTurn holds what each session's message list should report: the
	// prompt text and the response text of its last admitted prompt.
	lastTurn map[string]fakeOpencodeTurnRecord
}

type fakeOpencodeTurnRecord struct {
	prompt   string
	response string
	action   Action
}

// opencodeFixture holds the bytes captured by recordfixture for one
// flavour. session/sse/messages mirror the file layout under the fixture
// directory: the POST /api/session response, the raw /api/event bytes up to
// the turn's execution end, and the GET /api/session/{id}/message response.
type opencodeFixture struct {
	flavour   string
	sessionID string
	session   []byte
	sse       []byte
	messages  []byte
}

func newFakeOpencodeServer(scenario *Scenario) *fakeOpencodeServer {
	srv := &fakeOpencodeServer{
		scenario:    scenario,
		sessionDirs: make(map[string]string),
		lastTurn:    make(map[string]fakeOpencodeTurnRecord),
		password:    os.Getenv("OPENCODE_SERVER_PASSWORD"),
	}
	if dir := fixtureDir("opencode"); dir != "" {
		if fx, err := loadOpencodeFixture(dir, "structured"); err == nil {
			srv.fixture = fx
		} else {
			srv.fixtureErr = fmt.Errorf("opencode fixture load: %w", err)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", srv.fixtureErr)
		}
	}
	return srv
}

func loadOpencodeFixture(dir, flavour string) (*opencodeFixture, error) {
	read := func(name string) ([]byte, error) {
		return os.ReadFile(fmt.Sprintf("%s/%s/%s", dir, flavour, name))
	}
	session, err := read("session.json")
	if err != nil {
		return nil, fmt.Errorf("session.json: %w", err)
	}
	sse, err := read("sse.txt")
	if err != nil {
		return nil, fmt.Errorf("sse.txt: %w", err)
	}
	msgs, err := read("messages.json")
	if err != nil {
		return nil, fmt.Errorf("messages.json: %w", err)
	}
	var sessionDoc struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(session, &sessionDoc); err != nil {
		return nil, fmt.Errorf("session.json: parse: %w", err)
	}
	if sessionDoc.Data.ID == "" {
		return nil, fmt.Errorf("session.json: missing data.id")
	}
	return &opencodeFixture{flavour: flavour, sessionID: sessionDoc.Data.ID, session: session, sse: sse, messages: msgs}, nil
}

func (s *fakeOpencodeServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", s.guard(s.handleInfo))
	mux.HandleFunc("/api/event", s.guard(s.handleEvents))
	mux.HandleFunc("/api/session", s.guard(s.handleSessionRoot))
	mux.HandleFunc("/api/session/", s.guard(s.handleSessionPath))
	mux.HandleFunc("/api/experimental/session/", s.guard(s.handleExperimentalSessionPath))
	return mux
}

// guard applies what every real /api route does before its handler: the
// basic-auth check with the fixed "opencode" username, then the fixture
// guard so a misconfigured fixture fails loudly rather than replaying nothing.
func (s *fakeOpencodeServer) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.password != "" {
			user, password, ok := r.BasicAuth()
			if !ok || user != "opencode" || password != s.password {
				w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if s.fixtureErr != nil {
			http.Error(w, s.fixtureErr.Error(), http.StatusInternalServerError)
			return
		}
		next(w, r)
	}
}

func (s *fakeOpencodeServer) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"version": "2.0.16-fake", "pid": os.Getpid(), "urls": []string{}})
}

// handleEvents holds the SSE connection open and forwards anything sent
// on the per-subscriber channel. The adapter opens one stream per turn,
// but the broadcast model keeps us honest if that ever changes.
//
// In fixture mode the bytes are already SSE-formatted (the recording
// captured raw SSE from the real opencode), so we forward them verbatim.
// In synthetic mode the broadcaster sends just the data payload and we
// wrap it in `data: ...\n\n` framing here.
func (s *fakeOpencodeServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 32)
	s.subscribe(ch)
	defer s.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"evt_connected\",\"type\":\"server.connected\",\"data\":{}}\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			w.Write(data)
			flusher.Flush()
		}
	}
}

func (s *fakeOpencodeServer) subscribe(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, ch)
}

func (s *fakeOpencodeServer) unsubscribe(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sub := range s.subscribers {
		if sub == ch {
			s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
			break
		}
	}
}

// broadcast sends a synthetic event (just the JSON body) to every
// subscriber wrapped in proper SSE framing. Fixture-mode replay uses
// broadcastRaw instead.
func (s *fakeOpencodeServer) broadcast(event map[string]any) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.broadcastRaw([]byte(fmt.Sprintf("data: %s\n\n", data)))
}

// broadcastRaw forwards already-SSE-framed bytes to every subscriber.
// Used for replaying the recorded SSE stream byte-for-byte.
func (s *fakeOpencodeServer) broadcastRaw(framed []byte) {
	s.mu.Lock()
	subs := append([]chan []byte(nil), s.subscribers...)
	s.mu.Unlock()
	for _, ch := range subs {
		buf := make([]byte, len(framed))
		copy(buf, framed)
		select {
		case ch <- buf:
		default:
		}
	}
}

func (s *fakeOpencodeServer) handleSessionRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, context.Canceled) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	dir := body.Location.Directory
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dir = wd
	}
	id := s.nextSessionID()
	s.recordSessionDir(id, dir)
	if s.fixture != nil {
		patched, err := rewriteOpencodeFixtureSession(s.fixture, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: opencode session patch: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(patched)
		return
	}
	writeJSON(w, map[string]any{"data": map[string]any{
		"id":        id,
		"projectID": "fake",
		"time":      map[string]int64{"created": time.Now().UnixMilli(), "updated": time.Now().UnixMilli()},
		"location":  map[string]string{"directory": dir},
	}})
}

func (s *fakeOpencodeServer) nextSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionSeq++
	return fmt.Sprintf("ses_%d", s.sessionSeq)
}

// handleSessionPath dispatches /api/session/{id} (DELETE),
// /api/session/{id}/prompt, /api/session/{id}/message and
// /api/session/{id}/interrupt.
func (s *fakeOpencodeServer) handleSessionPath(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	sessionID := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.forgetSession(sessionID)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "interrupt" && r.Method == http.MethodPost:
		writeJSON(w, map[string]any{"interrupted": false})
	case len(parts) == 2 && parts[1] == "prompt" && r.Method == http.MethodPost:
		s.handlePrompt(w, r, sessionID)
	case len(parts) == 2 && parts[1] == "message" && r.Method == http.MethodGet:
		s.handleMessages(w, r, sessionID)
	default:
		http.NotFound(w, r)
	}
}

// handleExperimentalSessionPath answers /api/experimental/session/{id}/wait:
// the fake plays every turn out synchronously inside the prompt handler, so
// by the time anyone waits the session is idle.
func (s *fakeOpencodeServer) handleExperimentalSessionPath(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/experimental/session/"), "/")
	if len(parts) == 2 && parts[1] == "wait" && r.Method == http.MethodPost {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

func (s *fakeOpencodeServer) recordSessionDir(sessionID, dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionDirs[sessionID] = dir
}

func (s *fakeOpencodeServer) sessionDir(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionDirs[sessionID]
}

func (s *fakeOpencodeServer) forgetSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessionDirs, sessionID)
	delete(s.lastTurn, sessionID)
}

// responseText is what the fake's assistant "writes" for an action. opencode
// 2.x has no structured-output slot, so a scenario's structured payload IS
// the assistant text, exactly as the real model writes the JSON the prompt
// asked for; only a plain action uses its human text.
func responseText(action Action) string {
	if action.hasStructuredOutput() {
		return string(action.structuredJSON())
	}
	return action.textOrDefault()
}

// handlePrompt is the heart of the fake. It pulls the prompt out of the
// request, runs the scenario, answers with the admitted inbox entry the way
// the real server does, and then broadcasts the events of the turn - the
// adapter subscribed before it prompted, so nothing is missed.
func (s *fakeOpencodeServer) handlePrompt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := body.Text
	logInvocation("opencode", prompt, []string{"session", sessionID})

	action := s.scenario.MatchInDir(s.sessionDir(sessionID), prompt)
	if err := applyActionInDir(s.sessionDir(sessionID), action); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respText := responseText(action)

	s.mu.Lock()
	s.msgSeq++
	seq := s.msgSeq
	s.lastTurn[sessionID] = fakeOpencodeTurnRecord{prompt: prompt, response: respText, action: action}
	s.mu.Unlock()
	userID := fmt.Sprintf("msg_user_%d", seq)
	asstID := fmt.Sprintf("msg_asst_%d", seq)

	writeJSON(w, map[string]any{"data": map[string]any{
		"id":        userID,
		"sessionID": sessionID,
		"time":      map[string]int64{"created": time.Now().UnixMilli()},
		"type":      "user",
		"payload":   map[string]string{"text": prompt},
		"delivery":  "steer",
	}})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if s.fixture != nil {
		// Replay the recorded SSE bytes verbatim with the session id
		// rewritten and the assistant text substituted, so the wire
		// envelope stays real while the scenario controls the payload.
		framed, err := rewriteOpencodeFixtureSSE(s.fixture, respText, sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: opencode sse patch: %v\n", err)
			return
		}
		s.broadcastRaw(framed)
		return
	}

	s.broadcast(eventSessionStarted(sessionID))
	s.broadcast(eventTextEnded(sessionID, asstID, respText))
	s.broadcast(eventStepEnded(sessionID, asstID))
	s.broadcast(eventExecutionSucceeded(sessionID))
}

// handleMessages serves the session's message list, newest first as the
// real endpoint does, carrying the last turn's assistant text.
func (s *fakeOpencodeServer) handleMessages(w http.ResponseWriter, _ *http.Request, sessionID string) {
	s.mu.Lock()
	turn, ok := s.lastTurn[sessionID]
	seq := s.msgSeq
	s.mu.Unlock()

	if s.fixture != nil {
		if !ok {
			writeJSON(w, map[string]any{"data": []any{}, "cursor": map[string]any{}})
			return
		}
		patched, err := rewriteOpencodeFixtureMessages(s.fixture, turn.response, sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: opencode messages patch: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(patched)
		return
	}

	if !ok {
		writeJSON(w, map[string]any{"data": []any{}, "cursor": map[string]any{}})
		return
	}
	base := time.Now().UnixMilli() - 10
	writeJSON(w, map[string]any{
		"data": []map[string]any{
			{"id": fmt.Sprintf("msg_idle_%d", seq), "type": "idle", "outcome": "succeeded", "time": map[string]int64{"created": base + 3}},
			{
				"id": fmt.Sprintf("msg_asst_%d", seq), "type": "assistant", "agent": "build",
				"content": []map[string]any{{"type": "text", "text": turn.response}},
				"finish":  "stop",
				"tokens":  fakeTokens(),
				"time":    map[string]int64{"created": base + 2},
			},
			{"id": fmt.Sprintf("msg_user_%d", seq), "type": "user", "text": turn.prompt, "time": map[string]int64{"created": base + 1}},
		},
		"cursor": map[string]any{"previous": nil, "next": nil},
	})
}

func fakeTokens() map[string]any {
	return map[string]any{
		"input":     100,
		"output":    50,
		"reasoning": 0,
		"cache":     map[string]int{"read": 0, "write": 0},
	}
}

func eventSessionStarted(sessionID string) map[string]any {
	return map[string]any{
		"id":   "evt_started",
		"type": "session.execution.started",
		"data": map[string]any{"sessionID": sessionID},
	}
}

func eventTextEnded(sessionID, msgID, text string) map[string]any {
	return map[string]any{
		"id":   "evt_text",
		"type": "session.text.ended",
		"data": map[string]any{
			"sessionID":          sessionID,
			"assistantMessageID": msgID,
			"ordinal":            0,
			"text":               text,
		},
	}
}

func eventStepEnded(sessionID, msgID string) map[string]any {
	return map[string]any{
		"id":   "evt_step",
		"type": "session.step.ended",
		"data": map[string]any{
			"sessionID":          sessionID,
			"assistantMessageID": msgID,
			"finish":             "stop",
			"tokens":             fakeTokens(),
		},
	}
}

func eventExecutionSucceeded(sessionID string) map[string]any {
	return map[string]any{
		"id":   "evt_done",
		"type": "session.execution.succeeded",
		"data": map[string]any{"sessionID": sessionID},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func rewriteOpencodeFixtureSession(fixture *opencodeFixture, sessionID string) ([]byte, error) {
	if fixture == nil {
		return nil, fmt.Errorf("rewrite session: missing fixture")
	}
	return rewriteOpencodeFixtureJSON(fixture.session, fixture.sessionID, sessionID)
}

// rewriteOpencodeFixtureSSE replays the recorded stream with the session id
// rewritten and the assistant text replaced. The recorded turn may have
// several text blocks (one per model step); every block but the last is
// blanked and the last carries the scenario text, so the joined output the
// adapter reads is exactly the scenario's.
func rewriteOpencodeFixtureSSE(fixture *opencodeFixture, text, sessionID string) ([]byte, error) {
	if fixture == nil {
		return nil, fmt.Errorf("rewrite sse: missing fixture")
	}
	patcher, err := newOpencodeSSETextPatcher(fixture.sse)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(fixture.sse, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		patched, err := patcher.patch(payload, text)
		if err != nil {
			return nil, fmt.Errorf("patch sse event: %w", err)
		}
		patched, err = rewriteOpencodeFixtureJSON(patched, fixture.sessionID, sessionID)
		if err != nil {
			return nil, fmt.Errorf("rewrite sse event: %w", err)
		}
		out.WriteString("data: ")
		out.Write(patched)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// rewriteOpencodeFixtureMessages patches the recorded message list the same
// way: session ids rewritten, every assistant text content blanked except the
// last, which carries the scenario text.
func rewriteOpencodeFixtureMessages(fixture *opencodeFixture, text, sessionID string) ([]byte, error) {
	if fixture == nil {
		return nil, fmt.Errorf("rewrite messages: missing fixture")
	}
	patched, err := patchOpencodeMessages(fixture.messages, text)
	if err != nil {
		return nil, err
	}
	return rewriteOpencodeFixtureJSON(patched, fixture.sessionID, sessionID)
}

// opencodeTextBlock identifies one assistant text block on the stream.
type opencodeTextBlock struct {
	messageID string
	ordinal   int
}

func (b opencodeTextBlock) key() string { return b.messageID + "/" + strconv.Itoa(b.ordinal) }

// opencodeSSETextPatcher rewrites the text of the recorded turn's text blocks
// (session.text.delta / session.text.ended) so the last block carries the
// scenario text and every other block is blank. Deltas of the last block are
// collapsed into its first delta so the streamed text and the ended snapshot
// agree, as they do on a real stream.
type opencodeSSETextPatcher struct {
	last      string
	deltaSeen map[string]int
}

func newOpencodeSSETextPatcher(raw []byte) (*opencodeSSETextPatcher, error) {
	var last string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Data struct {
				AssistantMessageID string `json:"assistantMessageID"`
				Ordinal            int    `json:"ordinal"`
			} `json:"data"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(trimmed[len("data:"):]), &event); err != nil {
			return nil, fmt.Errorf("parse sse event: %w", err)
		}
		if event.Type == "session.text.ended" || event.Type == "session.text.delta" {
			last = opencodeTextBlock{messageID: event.Data.AssistantMessageID, ordinal: event.Data.Ordinal}.key()
		}
	}
	return &opencodeSSETextPatcher{last: last, deltaSeen: make(map[string]int)}, nil
}

func (p *opencodeSSETextPatcher) patch(raw []byte, text string) ([]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	eventType, _ := event["type"].(string)
	if eventType != "session.text.ended" && eventType != "session.text.delta" {
		return raw, nil
	}
	data, _ := event["data"].(map[string]any)
	if data == nil {
		return raw, nil
	}
	messageID, _ := data["assistantMessageID"].(string)
	ordinal, _ := data["ordinal"].(float64)
	key := opencodeTextBlock{messageID: messageID, ordinal: int(ordinal)}.key()
	replacement := ""
	if key == p.last {
		replacement = text
	}
	switch eventType {
	case "session.text.ended":
		data["text"] = replacement
	case "session.text.delta":
		p.deltaSeen[key]++
		if p.deltaSeen[key] == 1 {
			data["delta"] = replacement
		} else {
			data["delta"] = ""
		}
	}
	return json.Marshal(event)
}

// patchOpencodeMessages rewrites the assistant text of the recorded message
// list so the last text content (oldest-first order) carries the scenario
// text and every earlier one is blank. The rest of the record - ids, tokens,
// tool content, the idle marker - stays exactly as recorded.
func patchOpencodeMessages(raw []byte, text string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}
	items, ok := resp["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("parse messages: missing data array")
	}
	type textSlot struct {
		created float64
		content map[string]any
	}
	var slots []textSlot
	for _, item := range items {
		msg, _ := item.(map[string]any)
		if msg == nil || msg["type"] != "assistant" {
			continue
		}
		var created float64
		if tm, _ := msg["time"].(map[string]any); tm != nil {
			created, _ = tm["created"].(float64)
		}
		content, _ := msg["content"].([]any)
		for _, rawContent := range content {
			c, _ := rawContent.(map[string]any)
			if c != nil && c["type"] == "text" {
				slots = append(slots, textSlot{created: created, content: c})
			}
		}
	}
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].created < slots[j].created })
	for i, slot := range slots {
		if i == len(slots)-1 {
			slot.content["text"] = text
		} else {
			slot.content["text"] = ""
		}
	}
	return json.Marshal(resp)
}

func rewriteOpencodeFixtureJSON(raw []byte, recordedSessionID, sessionID string) ([]byte, error) {
	if recordedSessionID == "" {
		return nil, fmt.Errorf("missing recorded session id")
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	rewriteSessionStrings(doc, recordedSessionID, sessionID)
	return json.Marshal(doc)
}

func rewriteSessionStrings(v any, recordedSessionID, sessionID string) {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if s, ok := value.(string); ok && s == recordedSessionID {
				x[k] = sessionID
				continue
			}
			rewriteSessionStrings(value, recordedSessionID, sessionID)
		}
	case []any:
		for i, value := range x {
			if s, ok := value.(string); ok && s == recordedSessionID {
				x[i] = sessionID
				continue
			}
			rewriteSessionStrings(value, recordedSessionID, sessionID)
		}
	}
}
