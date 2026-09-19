package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvaluate_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q, want bearer test-key", got)
		}
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != Model {
			t.Errorf("model = %q, want pinned %q", req.Model, Model)
		}
		if len(req.Questions) != 1 {
			t.Errorf("questions = %d, want 1", len(req.Questions))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {"is_urgent": {"type": "noul", "noul": 0.92}},
			"usage": {"input_tokens": 312, "output_tokens": 48}
		}`))
	}))
	defer server.Close()

	client := &Client{Endpoint: server.URL, Key: "test-key"}
	resp, err := client.Evaluate(context.Background(), "some state", map[string]Question{
		"is_urgent": {Type: "noul", Instructions: "Does this convey urgency?"},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if resp.Model != "jev-1.13.0" {
		t.Errorf("response model = %q", resp.Model)
	}
	answer := resp.Answers["is_urgent"]
	if answer.Type != "noul" || answer.Noul != 0.92 {
		t.Errorf("answer = %+v", answer)
	}
	if resp.Usage.InputTokens != 312 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestEvaluate_ChoiceAndScoreDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {
				"route": {"type": "choice", "choice": "technical", "probabilities": {"technical": 0.85, "billing": 0.15}, "confidence": 0.82},
				"relevance": {"type": "score", "score": 2.4, "probabilities": {"2": 0.6, "3": 0.4}, "confidence": 0.7}
			},
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`))
	}))
	defer server.Close()

	client := &Client{Endpoint: server.URL, Key: "k"}
	resp, err := client.Evaluate(context.Background(), "s", map[string]Question{"route": {Type: "choice", Instructions: "i", Criteria: map[string]any{"technical": nil}}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := resp.Answers["route"]; got.Choice != "technical" || got.Confidence != 0.82 {
		t.Errorf("choice answer = %+v", got)
	}
	if got := resp.Answers["relevance"]; got.Score != 2.4 || got.Confidence != 0.7 {
		t.Errorf("score answer = %+v", got)
	}
}

func TestEvaluate_NoKeyRefuses(t *testing.T) {
	client := &Client{Endpoint: "http://127.0.0.1:1"}
	if _, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}}); err == nil {
		t.Fatal("expected error without a key")
	}
}

func TestEvaluate_NoQuestionsRefuses(t *testing.T) {
	client := &Client{Endpoint: "http://127.0.0.1:1", Key: "k"}
	if _, err := client.Evaluate(context.Background(), "s", nil); err == nil {
		t.Fatal("expected error without questions")
	}
}

func TestEvaluate_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "bad key"}`))
	}))
	defer server.Close()
	client := &Client{Endpoint: server.URL, Key: "k"}
	_, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 surfaced", err)
	}
	if strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err embeds the remote response body, which reaches operator logs: %v", err)
	}
}

// TestEvaluate_RetryWaitIsContextAware pins that a cancellation during the
// rate-limit wait returns immediately instead of sleeping through it, so the
// caller's ordinary cold-review fallback starts at once.
func TestEvaluate_RetryWaitIsContextAware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.AfterFunc(100*time.Millisecond, cancel)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := &Client{Endpoint: server.URL, Key: "k"}
	start := time.Now()
	_, err := client.Evaluate(ctx, "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation during the retry wait took %v", time.Since(start))
	}
}

// TestEvaluate_OversizedSuccessBodyRejected pins the bound on success bodies:
// answers are small typed maps, so an oversized body is rejected rather than
// materialized.
func TestEvaluate_OversizedSuccessBodyRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1},"padding":"` + strings.Repeat("x", maxResponseBodyBytes) + `"}`))
	}))
	defer server.Close()
	client := &Client{Endpoint: server.URL, Key: "k"}
	_, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want oversized body rejected", err)
	}
}

func TestEvaluate_RateLimitedThenRetriedOnce(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"model": "jev-1.13.0", "answers": {"q": {"type": "noul", "noul": 0.1}}, "usage": {"input_tokens": 1, "output_tokens": 1}}`))
	}))
	defer server.Close()

	var slept []time.Duration
	client := &Client{Endpoint: server.URL, Key: "k", Sleep: func(d time.Duration) { slept = append(slept, d) }}
	resp, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if resp.Answers["q"].Noul != 0.1 {
		t.Errorf("answer = %+v", resp.Answers["q"])
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Errorf("slept = %v, want one 3s wait from Retry-After", slept)
	}
}

func TestEvaluate_OverloadedRetriedOnceThenFails(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(529)
	}))
	defer server.Close()
	client := &Client{Endpoint: server.URL, Key: "k", Sleep: func(time.Duration) {}}
	_, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}})
	if err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("err = %v, want 529 surfaced", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want exactly one retry", calls)
	}
}

func TestEvaluate_MalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()
	client := &Client{Endpoint: server.URL, Key: "k"}
	if _, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestEvaluate_MissingAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model": "jev-1.13.0", "usage": {"input_tokens": 1}}`))
	}))
	defer server.Close()
	client := &Client{Endpoint: server.URL, Key: "k"}
	if _, err := client.Evaluate(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}}); err == nil {
		t.Fatal("expected error for a response without answers")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter(""); got != time.Second {
		t.Errorf("empty = %v", got)
	}
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Errorf("2 = %v", got)
	}
	if got := parseRetryAfter("3600"); got != maxRetryWait {
		t.Errorf("3600 = %v, want capped at %v", got, maxRetryWait)
	}
	if got := parseRetryAfter("garbage"); got != time.Second {
		t.Errorf("garbage = %v", got)
	}
}

func TestNewClientFromEnv(t *testing.T) {
	t.Setenv(EnvKey, "")
	if NewClientFromEnv() != nil {
		t.Fatal("expected nil client without a key")
	}
	t.Setenv(EnvKey, "abc")
	client := NewClientFromEnv()
	if client == nil || client.Key != "abc" {
		t.Fatalf("client = %+v", client)
	}
}

func TestEvaluate_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &Client{Endpoint: server.URL, Key: "k"}
	_, err := client.Evaluate(ctx, "s", map[string]Question{"q": {Type: "noul", Instructions: "i"}})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
