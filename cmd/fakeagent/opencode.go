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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runOpencode boots a long-running HTTP server that mimics OpenCode's
// REST + SSE surface. It blocks until the parent (no-mistakes' agent
// package) signals shutdown. The OpenCode wire format is documented in
// internal/agent/opencode_types.go.
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

	mu          sync.Mutex
	subscribers []chan []byte // active /global/event listeners (one per request)
	sessionDirs map[string]string
	sessionSeq  int
	msgSeq      int
}

// opencodeFixture holds the bytes captured by recordfixture for one
// flavour. session/sse/message mirror the file layout under the
// fixture directory.
type opencodeFixture struct {
	flavour   string
	sessionID string
	session   []byte
	sse       []byte
	message   []byte
}

func newFakeOpencodeServer(scenario *Scenario) *fakeOpencodeServer {
	srv := &fakeOpencodeServer{scenario: scenario, sessionDirs: make(map[string]string)}
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
	msg, err := read("message.json")
	if err != nil {
		return nil, fmt.Errorf("message.json: %w", err)
	}
	var sessionDoc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(session, &sessionDoc); err != nil {
		return nil, fmt.Errorf("session.json: parse: %w", err)
	}
	if sessionDoc.ID == "" {
		return nil, fmt.Errorf("session.json: missing id")
	}
	return &opencodeFixture{flavour: flavour, sessionID: sessionDoc.ID, session: session, sse: sse, message: msg}, nil
}

func (s *fakeOpencodeServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", s.withFixtureGuard(s.handleHealth))
	mux.HandleFunc("/global/event", s.withFixtureGuard(s.handleEvents))
	mux.HandleFunc("/session", s.withFixtureGuard(s.handleSessionRoot))
	mux.HandleFunc("/session/", s.withFixtureGuard(s.handleSessionPath))
	return mux
}

func (s *fakeOpencodeServer) withFixtureGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.fixtureErr != nil {
			http.Error(w, s.fixtureErr.Error(), http.StatusInternalServerError)
			return
		}
		next(w, r)
	}
}

func (s *fakeOpencodeServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleEvents holds the SSE connection open and forwards anything sent
// on the per-subscriber channel. The test only opens one stream per run,
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan []byte, 32)
	s.subscribe(ch)
	defer s.unsubscribe(ch)

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

// broadcast sends a synthetic SSE event payload (just the JSON body) to
// every subscriber wrapped in proper SSE framing. Fixture-mode replay
// uses broadcastRaw instead.
func (s *fakeOpencodeServer) broadcast(event map[string]any) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	framed := []byte(fmt.Sprintf("data: %s\n\n", data))
	s.broadcastRaw(framed)
}

// broadcastRaw forwards already-SSE-framed bytes to every subscriber.
// Used for replaying the recorded SSE stream byte-for-byte.
func (s *fakeOpencodeServer) broadcastRaw(framed []byte) {
	s.mu.Lock()
	subs := append([]chan []byte(nil), s.subscribers...)
	s.mu.Unlock()
	for _, ch := range subs {
		// Copy so each subscriber owns its slice.
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
		Directory string `json:"directory"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, context.Canceled) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if body.Directory == "" {
		wd, err := os.Getwd()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body.Directory = wd
	}
	id := s.nextSessionID()
	s.recordSessionDir(id, body.Directory)
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
	writeJSON(w, map[string]string{"id": id})
}

func (s *fakeOpencodeServer) nextSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionSeq++
	return fmt.Sprintf("ses_%d", s.sessionSeq)
}

// handleSessionPath dispatches /session/{id}, /session/{id}/message, and
// /session/{id}/abort. The DELETE variant just responds 200; abort and
// delete don't need scenario interaction.
func (s *fakeOpencodeServer) handleSessionPath(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/session/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	sessionID := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.forgetSessionDir(sessionID)
		w.WriteHeader(http.StatusOK)
	case len(parts) == 2 && parts[1] == "abort" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusOK)
	case len(parts) == 2 && parts[1] == "message" && r.Method == http.MethodPost:
		s.handleMessage(w, r, sessionID)
	default:
		http.NotFound(w, r)
	}
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

func (s *fakeOpencodeServer) forgetSessionDir(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessionDirs, sessionID)
}

// handleMessage is the heart of the fake. It pulls the prompt out of the
// request, runs the scenario, broadcasts the streaming events the real
// OpenCode would emit, and then returns the synchronous message response
// with the structured payload.
func (s *fakeOpencodeServer) handleMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body struct {
		Role  string `json:"role"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := ""
	for _, p := range body.Parts {
		if p.Type == "text" {
			prompt += p.Text
		}
	}
	logInvocation("opencode", prompt, []string{"session", sessionID})

	if s.fixture != nil {
		// Stream the recorded SSE bytes verbatim, then return the
		// recorded message response with info.structured patched to
		// match the scenario. The wire envelope (events, parts shape,
		// info field set) stays real; only the structured payload is
		// substituted so happy-path tests don't depend on whatever
		// the live model returned at recording time.
		action := s.scenario.Match(prompt)
		if err := applyActionInDir(s.sessionDir(sessionID), action); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		framed, err := rewriteOpencodeFixtureSSE(s.fixture, action, sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: opencode sse patch: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.broadcastRaw(framed)
		patched, err := rewriteOpencodeFixtureMessage(s.fixture, action, sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: opencode patch: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(patched)
		return
	}

	action := s.scenario.Match(prompt)
	if err := applyActionInDir(s.sessionDir(sessionID), action); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.msgSeq++
	userID := fmt.Sprintf("msg_user_%d", s.msgSeq)
	asstID := fmt.Sprintf("msg_asst_%d", s.msgSeq)
	asstPartID := fmt.Sprintf("part_text_%d", s.msgSeq)
	s.mu.Unlock()

	// Mark the user message so the parser can filter its echoes.
	s.broadcast(eventMessageUpdated(sessionID, userID, "user"))

	// Stream a single text part with the response body, then mark the
	// assistant message updated with token usage so the parser captures
	// the same shape the real OpenCode emits.
	respText := action.textOrDefault()
	s.broadcast(eventMessagePartUpdated(sessionID, asstID, asstPartID, respText))
	s.broadcast(eventMessageUpdatedAssistant(sessionID, asstID))
	s.broadcast(eventSessionIdle(sessionID))

	// The synchronous message response carries the structured output.
	resp := map[string]any{
		"info": map[string]any{
			"id":   asstID,
			"role": "assistant",
			"tokens": map[string]any{
				"input":  100,
				"output": 50,
				"cache":  map[string]int{"read": 0, "write": 0},
			},
		},
		"parts": []map[string]any{
			{"type": "text", "text": respText},
		},
	}
	if action.hasStructuredOutput() {
		resp["info"].(map[string]any)["structured"] = json.RawMessage(action.structuredJSON())
	}
	writeJSON(w, resp)
}

func eventMessagePartUpdated(sessionID, msgID, partID, text string) map[string]any {
	return map[string]any{
		"payload": map[string]any{
			"type": "message.part.updated",
			"properties": map[string]any{
				"sessionID": sessionID,
				"part": map[string]any{
					"id":        partID,
					"messageID": msgID,
					"type":      "text",
					"text":      text,
				},
			},
		},
	}
}

func eventMessageUpdated(sessionID, msgID, role string) map[string]any {
	return map[string]any{
		"payload": map[string]any{
			"type": "message.updated",
			"properties": map[string]any{
				"sessionID": sessionID,
				"info": map[string]any{
					"id":   msgID,
					"role": role,
				},
			},
		},
	}
}

func eventMessageUpdatedAssistant(sessionID, msgID string) map[string]any {
	return map[string]any{
		"payload": map[string]any{
			"type": "message.updated",
			"properties": map[string]any{
				"sessionID": sessionID,
				"info": map[string]any{
					"id":   msgID,
					"role": "assistant",
					"tokens": map[string]any{
						"input":  100,
						"output": 50,
						"cache":  map[string]int{"read": 0, "write": 0},
					},
				},
			},
		},
	}
}

func eventSessionIdle(sessionID string) map[string]any {
	return map[string]any{
		"payload": map[string]any{
			"type": "session.idle",
			"properties": map[string]any{
				"sessionID": sessionID,
			},
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// patchOpencodeMessage rewrites info.structured on the recorded message
// response so the scenario controls the structured payload while the
// rest of the response (info.id, role, tokens, parts shape) stays
// faithful to what real opencode emitted.
func patchOpencodeMessage(raw []byte, action Action) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}
	info, _ := resp["info"].(map[string]any)
	if info == nil {
		return nil, fmt.Errorf("parse message: missing info object")
	}
	if action.hasStructuredOutput() {
		info["structured"] = json.RawMessage(action.structuredJSON())
	}
	resp["info"] = info
	patchOpencodeMessageParts(resp, action.textOrDefault())
	return json.Marshal(resp)
}

func rewriteOpencodeFixtureSession(fixture *opencodeFixture, sessionID string) ([]byte, error) {
	if fixture == nil {
		return nil, fmt.Errorf("rewrite session: missing fixture")
	}
	return rewriteOpencodeFixtureJSON(fixture.session, fixture.sessionID, sessionID)
}

func rewriteOpencodeFixtureSSE(fixture *opencodeFixture, action Action, sessionID string) ([]byte, error) {
	if fixture == nil {
		return nil, fmt.Errorf("rewrite sse: missing fixture")
	}
	textPatcher, err := newOpencodeSSETextPatcher(fixture.sse)
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
		patched, err := textPatcher.patch(payload, action.textOrDefault())
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

func rewriteOpencodeFixtureMessage(fixture *opencodeFixture, action Action, sessionID string) ([]byte, error) {
	if fixture == nil {
		return nil, fmt.Errorf("rewrite message: missing fixture")
	}
	patched, err := patchOpencodeMessage(fixture.message, action)
	if err != nil {
		return nil, err
	}
	return rewriteOpencodeFixtureJSON(patched, fixture.sessionID, sessionID)
}

type opencodeSSETextPatcher struct {
	targetPartIDs map[string]bool
	deltaCounts   map[string]int
	updateTotals  map[string]int
	updateSeen    map[string]int
	deltaSeen     map[string]int
}

func newOpencodeSSETextPatcher(raw []byte) (*opencodeSSETextPatcher, error) {
	assistantMsgIDs := make(map[string]bool)
	assistantTextPartIDs := make(map[string]bool)
	finalAnswerPartIDs := make(map[string]bool)
	deltaCounts := make(map[string]int)
	updateTotals := make(map[string]int)

	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(trimmed[len("data:"):]), &event); err != nil {
			return nil, fmt.Errorf("parse sse event: %w", err)
		}
		payload, _ := event["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		switch payload["type"] {
		case "message.updated":
			props, _ := payload["properties"].(map[string]any)
			info, _ := props["info"].(map[string]any)
			if info == nil || info["role"] != "assistant" {
				continue
			}
			if id, _ := info["id"].(string); id != "" {
				assistantMsgIDs[id] = true
			}
		case "message.part.updated":
			props, _ := payload["properties"].(map[string]any)
			part, _ := props["part"].(map[string]any)
			if part == nil || part["type"] != "text" {
				continue
			}
			messageID, _ := part["messageID"].(string)
			partID, _ := part["id"].(string)
			if partID == "" {
				continue
			}
			metadata, _ := part["metadata"].(map[string]any)
			openai, _ := metadata["openai"].(map[string]any)
			phase, _ := openai["phase"].(string)
			if assistantMsgIDs[messageID] || phase == "final_answer" {
				assistantTextPartIDs[partID] = true
				updateTotals[partID]++
			}
			if phase == "final_answer" {
				finalAnswerPartIDs[partID] = true
			}
		case "message.part.delta":
			props, _ := payload["properties"].(map[string]any)
			partID, _ := props["partID"].(string)
			if partID != "" {
				deltaCounts[partID]++
			}
		}
	}

	targetPartIDs := make(map[string]bool)
	if len(finalAnswerPartIDs) > 0 {
		for partID := range finalAnswerPartIDs {
			targetPartIDs[partID] = true
		}
	} else {
		for partID := range assistantTextPartIDs {
			targetPartIDs[partID] = true
		}
	}

	return &opencodeSSETextPatcher{
		targetPartIDs: targetPartIDs,
		deltaCounts:   deltaCounts,
		updateTotals:  updateTotals,
		updateSeen:    make(map[string]int),
		deltaSeen:     make(map[string]int),
	}, nil
}

func (p *opencodeSSETextPatcher) patch(raw []byte, text string) ([]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	payload, _ := event["payload"].(map[string]any)
	if payload == nil {
		return raw, nil
	}
	props, _ := payload["properties"].(map[string]any)
	switch payload["type"] {
	case "message.part.updated":
		part, _ := props["part"].(map[string]any)
		partID, _ := part["id"].(string)
		if !p.targetPartIDs[partID] || part["type"] != "text" {
			return raw, nil
		}
		p.updateSeen[partID]++
		if p.deltaCounts[partID] > 0 {
			if p.updateTotals[partID] == 1 || p.updateSeen[partID] < p.updateTotals[partID] {
				part["text"] = ""
			} else {
				part["text"] = text
			}
		} else {
			part["text"] = text
		}
	case "message.part.delta":
		partID, _ := props["partID"].(string)
		field, _ := props["field"].(string)
		if !p.targetPartIDs[partID] || field != "text" {
			return raw, nil
		}
		p.deltaSeen[partID]++
		if p.deltaSeen[partID] == 1 {
			props["delta"] = text
		} else {
			props["delta"] = ""
		}
	}
	return json.Marshal(event)
}

func patchOpencodeMessageParts(resp map[string]any, text string) {
	parts, _ := resp["parts"].([]any)
	if len(parts) == 0 {
		return
	}
	targetIndexes := make([]int, 0)
	finalAnswerIndexes := make([]int, 0)
	for i, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if part == nil || part["type"] != "text" {
			continue
		}
		targetIndexes = append(targetIndexes, i)
		metadata, _ := part["metadata"].(map[string]any)
		openai, _ := metadata["openai"].(map[string]any)
		if phase, _ := openai["phase"].(string); phase == "final_answer" {
			finalAnswerIndexes = append(finalAnswerIndexes, i)
		}
	}
	if len(finalAnswerIndexes) > 0 {
		targetIndexes = finalAnswerIndexes
	}
	for i, idx := range targetIndexes {
		part, _ := parts[idx].(map[string]any)
		if part == nil {
			continue
		}
		if i == len(targetIndexes)-1 {
			part["text"] = text
		} else {
			part["text"] = ""
		}
		parts[idx] = part
	}
	resp["parts"] = parts
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
