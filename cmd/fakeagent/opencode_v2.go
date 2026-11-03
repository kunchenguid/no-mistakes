package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

// The V2 fixture is intentionally independent of the recorded V1 envelopes.
// Its prompt response is only admission; the wait and projection endpoints
// expose the eventual scenario result through the published V2 contract.
type fakeOpencodeV2Server struct {
	base     *fakeOpencodeServer
	password string
	mu       sync.Mutex
	sessions map[string]*fakeOpencodeV2Session
}

type fakeOpencodeV2Session struct {
	directory string
	done      chan struct{}
	outcome   string
	message   map[string]any
}

func newFakeOpencodeV2Server(scenario *Scenario) *fakeOpencodeV2Server {
	password := os.Getenv("OPENCODE_PASSWORD")
	_ = os.Unsetenv("OPENCODE_PASSWORD")
	_ = os.Unsetenv("OPENCODE_SERVER_PASSWORD")
	return &fakeOpencodeV2Server{base: &fakeOpencodeServer{scenario: scenario, sessionDirs: make(map[string]string)}, password: password, sessions: make(map[string]*fakeOpencodeV2Session)}
}

func (s *fakeOpencodeV2Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"version": "2.0.26"}) })
	mux.HandleFunc("GET /api/event", s.base.handleEvents)
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Location.Directory == "" {
			http.Error(w, "missing location", 400)
			return
		}
		id := s.base.nextSessionID()
		s.mu.Lock()
		s.sessions[id] = &fakeOpencodeV2Session{directory: body.Location.Directory}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": map[string]string{"id": id}})
	})
	mux.HandleFunc("/api/session/", s.session)
	mux.HandleFunc("POST /api/experimental/session/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/experimental/session/"), "/wait")
		s.mu.Lock()
		session := s.sessions[id]
		var done chan struct{}
		if session != nil {
			done = session.done
		}
		s.mu.Unlock()
		if done == nil {
			http.NotFound(w, r)
			return
		}
		select {
		case <-done:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "opencode" || password != s.password || s.password == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *fakeOpencodeV2Server) session(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/")
	id := parts[0]
	s.mu.Lock()
	session := s.sessions[id]
	s.mu.Unlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.mu.Lock()
		outcome := session.outcome
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": map[string]string{"id": id, "outcome": outcome}})
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "interrupt" && r.Method == http.MethodPost:
		writeJSON(w, map[string]bool{"interrupted": false})
	case len(parts) == 2 && parts[1] == "prompt" && r.Method == http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad prompt", 400)
			return
		}
		s.mu.Lock()
		if session.done != nil {
			s.mu.Unlock()
			http.Error(w, "already admitted", 409)
			return
		}
		session.done = make(chan struct{})
		s.mu.Unlock()
		logInvocation("opencode", body.Text, []string{"session", id})
		go s.execute(id, session, body.Text)
		writeJSON(w, map[string]any{"data": map[string]string{"id": "msg_prompt", "type": "user"}})
	case len(parts) == 2 && parts[1] == "message" && r.Method == http.MethodGet:
		s.mu.Lock()
		message := session.message
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": []any{message}, "cursor": map[string]any{"next": nil}})
	default:
		http.NotFound(w, r)
	}
}

func (s *fakeOpencodeV2Server) execute(id string, session *fakeOpencodeV2Session, prompt string) {
	defer close(session.done)
	action := s.base.scenario.MatchInDir(session.directory, prompt)
	err := applyActionInDir(session.directory, action)
	text := action.textOrDefault()
	if action.hasStructuredOutput() {
		text = string(action.structuredJSON())
	}
	messageID := "msg_" + id
	message := map[string]any{"id": messageID, "type": "assistant", "content": []any{map[string]string{"type": "text", "text": text}}, "tokens": map[string]any{"input": 100, "output": 50, "cache": map[string]int{"read": 0, "write": 0}}}
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
		message["error"] = map[string]string{"type": "fixture", "message": fmt.Sprint(err)}
	}
	s.base.broadcast(map[string]any{"type": "session.text.ended", "data": map[string]any{"sessionID": id, "assistantMessageID": messageID, "ordinal": 0, "text": text}})
	s.mu.Lock()
	session.message = message
	session.outcome = outcome
	s.mu.Unlock()
}
