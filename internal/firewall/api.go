package firewall

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const DefaultListen = "127.0.0.1:8787"

// MaxIngestBytes bounds one ingest body. An oversized body is answered 413 so
// a payload the portal refused is never mistaken for malformed JSON, and the
// runner can report it as an error rather than a Hard Rules verdict.
const MaxIngestBytes = 8 << 20

type Server struct {
	Store      *Store
	PortalBase string
	IngestTok  string
	Listen     string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/axi/home", s.getHome)
	mux.HandleFunc("GET /v1/axi/runs/{id}", s.getRun)
	mux.HandleFunc("GET /v1/axi/runs/{id}/logs", s.getLogs)
	mux.HandleFunc("POST /v1/axi/runs/{id}/respond", s.postRespond)
	mux.HandleFunc("POST /v1/axi/runs/{id}/abort", s.postAbort)
	mux.HandleFunc("GET /v1/firewall/verdicts/{id}/notice", s.getNotice)
	mux.HandleFunc("POST /v1/firewall/verdicts", s.postIngest)
	return mux
}

func (s *Server) ListenAndServe() error {
	addr := s.Listen
	if addr == "" {
		addr = DefaultListen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

type ingestRequest struct {
	Repo           string   `json:"repo"`
	Branch         string   `json:"branch"`
	HeadSHA        string   `json:"head_sha"`
	BaseSHA        string   `json:"base_sha"`
	PRURL          string   `json:"pr_url"`
	PRNumber       int      `json:"pr_number"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	CommitMessages []string `json:"commit_messages"`
	Diff           string   `json:"diff"`
	// The verdict the client already published to GitHub. The portal records
	// it verbatim; it never re-scans, so the LAN record cannot contradict the
	// conclusion the required check reported.
	Findings []Finding `json:"findings"`
	Error    string    `json:"error"`
}

func (s *Server) postIngest(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxIngestBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req ingestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	in := Input{
		Diff:           req.Diff,
		Title:          req.Title,
		CommitMessages: req.CommitMessages,
		Body:           req.Body,
		Repo:           req.Repo,
		PRURL:          req.PRURL,
		PRNumber:       req.PRNumber,
		HeadSHA:        req.HeadSHA,
		BaseSHA:        req.BaseSHA,
		Branch:         req.Branch,
	}
	res := Result{Findings: req.Findings, Error: req.Error}
	v, err := s.Store.Insert(in, res, s.PortalBase)
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, v.AxiRun())
}

// authorize guards every request that mutates stored verdict state: ingest,
// respond, and abort. Read surfaces stay open on the LAN.
func (s *Server) authorize(r *http.Request) bool {
	if s.IngestTok == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return got == s.IngestTok
}

func (s *Server) getHome(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.List(10)
	if err != nil {
		http.Error(w, "list", http.StatusInternalServerError)
		return
	}
	runs := make([]map[string]string, 0, len(list))
	for _, v := range list {
		runs = append(runs, map[string]string{
			"id":     v.ID,
			"branch": v.Branch,
			"status": v.Status,
			"head":   v.HeadSHA,
			"pr":     v.PRURL,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bin":         "no-mistakes firewall",
		"description": "LAN publish-firewall portal",
		"count":       len(runs),
		"runs":        runs,
		"help": []string{
			"GET /v1/axi/runs/{id} for axi-shaped status",
			"POST /v1/axi/runs/{id}/respond with {\"action\":\"acknowledge\"}",
		},
	})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "get", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": v.AxiRun()})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	lines, err := s.Store.Logs(r.PathValue("id"))
	if err != nil {
		http.Error(w, "logs", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": lines})
}

func (s *Server) postRespond(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	v, err := s.Store.Respond(r.PathValue("id"), body.Action)
	if err != nil {
		http.Error(w, "respond", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run":        v.AxiRun(),
		"responded":  true,
		"conclusion": v.Conclusion,
		"note":       "acknowledgement recorded; GitHub check is unchanged",
	})
}

func (s *Server) postAbort(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	v, err := s.Store.Abort(r.PathValue("id"))
	if err != nil {
		http.Error(w, "abort", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": v.AxiRun(), "aborted": true})
}

func (s *Server) getNotice(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, v.Notice())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
