// Package jev is a minimal client for TypeSafe's System One evaluation
// endpoint (POST https://api.typesafe.ai/v1/systemone). It exists for the
// opt-in review pre-brief (config key jev.review_assist): one batched
// evaluation of typed questions over a code-filtered state, consumed by the
// review step as advisory input. The package deliberately holds no policy:
// every caller decides what a failed call means, and the review step's answer
// is always to run the same cold, complete, session-free review it would have
// run without the assist.
//
// The request shape, question types (noul, choice, score), error statuses,
// and rate-limit guidance follow https://docs.typesafe.ai/api and
// https://docs.typesafe.ai/models. There is no Go SDK; the endpoint is one
// JSON POST, so this uses only the standard library.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	// DefaultEndpoint is the TypeSafe System One evaluation endpoint.
	DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"
	// Model pins the versioned model ID. Anything feeding a gate never uses
	// an alias such as jev-latest: an alias moves when TypeSafe ships a new
	// release, which would change the answers behind an unchanged
	// configuration. The response's own model field is what the caller logs,
	// so a deliberate pin bump stays attributable.
	Model = "jev-1.13.0"
	// EnvKey is the environment variable carrying the API key. The key is
	// read from the daemon's environment at turn time and is never written
	// to config, logs, or the database.
	EnvKey = "TYPESAFE_API_KEY"
	// DefaultTimeout bounds one evaluation request, including the single
	// rate-limit retry wait.
	DefaultTimeout = 30 * time.Second
	// maxRetryWait caps how long a Retry-After header can make one call
	// wait. The caller's alternative to waiting longer is its ordinary path,
	// which is cheap to fall back to.
	maxRetryWait = 5 * time.Second
	// maxErrorBodyBytes bounds how much of an error response body is read
	// before it is discarded. Remote body text is never embedded in the
	// returned error: an error message reaches operator logs, and a remote
	// service's prose has no place there - the status code is the evidence.
	maxErrorBodyBytes = 4096
	// maxResponseBodyBytes bounds a success body. Answers are small typed
	// maps, so anything larger is not a valid response for this client and
	// is rejected rather than materialized.
	maxResponseBodyBytes = 1 << 20
)

// Question is one typed question in an evaluation request. Type is one of
// "noul", "choice", or "score"; Criteria is the per-type rubric (a
// map[string]*string for choice, a []string of ordered levels for score, a
// {true,false} object for noul) and Instructions is the question text.
// Structured instructions/criteria values are supported by the API, which is
// why both are any.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is the typed answer to one question. Only the fields matching the
// question's type are meaningful: Noul for noul, Choice for choice, Score for
// score; Confidence exists on choice and score answers only.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Usage is the token accounting of one request. Output tokens are free at
// the current pricing; input tokens are billed.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is one evaluation's result.
type Response struct {
	// Model is the versioned model ID that answered, as reported by the API.
	Model   string
	Answers map[string]Answer
	Usage   Usage
}

// Client calls the evaluation endpoint. The zero value is not usable;
// construct with NewClientFromEnv or set the fields directly in tests.
type Client struct {
	// Endpoint is the full URL of the evaluation endpoint. Empty uses
	// DefaultEndpoint.
	Endpoint string
	// Key is the bearer token. Evaluate refuses to send an empty key.
	Key string
	// HTTPClient overrides the HTTP client, for tests. Nil uses a client
	// with DefaultTimeout.
	HTTPClient *http.Client
	// Sleep replaces the rate-limit retry wait in tests. Nil waits on a
	// timer that a context cancellation interrupts.
	Sleep func(time.Duration)
}

// NewClientFromEnv returns a client authenticated from EnvKey, or nil when
// the variable is unset or blank. A nil client is the caller's signal to take
// its ordinary path; it is not an error.
func NewClientFromEnv() *Client {
	key := os.Getenv(EnvKey)
	if key == "" {
		return nil
	}
	return &Client{Key: key}
}

type request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

type responseBody struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Evaluate runs one batched evaluation: the state plus every question, in a
// single request, because the API bills the state once per request and
// evaluates all questions against it in parallel. One retry is made on 429 or
// 529, honoring a bounded Retry-After; every other failure returns an error.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (*Response, error) {
	if c == nil || c.Key == "" {
		return nil, fmt.Errorf("jev: no API key configured (set %s)", EnvKey)
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("jev: at least one question is required")
	}
	body, err := json.Marshal(request{State: state, Model: Model, Questions: questions})
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	resp, retryAfter, err := c.do(ctx, body)
	if err == nil {
		return resp, nil
	}
	if retryAfter <= 0 {
		return nil, err
	}
	if waitErr := c.wait(ctx, retryAfter); waitErr != nil {
		return nil, fmt.Errorf("jev: retry wait interrupted: %w", waitErr)
	}
	resp, _, err = c.do(ctx, body)
	return resp, err
}

// do performs one HTTP attempt. retryAfter is positive only for 429/529
// responses, telling the caller a bounded retry is worthwhile.
func (c *Client) do(ctx context.Context, body []byte) (*Response, time.Duration, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	httpResp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("jev: request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode == 529 {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, maxErrorBodyBytes))
		return nil, parseRetryAfter(httpResp.Header.Get("Retry-After")), fmt.Errorf("jev: %s", httpResp.Status)
	}
	if httpResp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, maxErrorBodyBytes))
		return nil, 0, fmt.Errorf("jev: %s", httpResp.Status)
	}
	var decoded responseBody
	limited := io.LimitReader(httpResp.Body, maxResponseBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, 0, fmt.Errorf("jev: read response: %w", err)
	}
	if len(raw) > maxResponseBodyBytes {
		return nil, 0, fmt.Errorf("jev: response exceeds %d bytes", maxResponseBodyBytes)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, 0, fmt.Errorf("jev: decode response: %w", err)
	}
	if decoded.Answers == nil {
		return nil, 0, fmt.Errorf("jev: response carries no answers")
	}
	return &Response{Model: decoded.Model, Answers: decoded.Answers, Usage: decoded.Usage}, 0, nil
}

// wait is the rate-limit retry wait. It is context-aware so a cancellation
// during the wait takes the caller's ordinary fallback path immediately
// instead of sleeping through it. The Sleep hook substitutes the timer in
// tests.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		c.Sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseRetryAfter reads a Retry-After header in its seconds form, bounded by
// maxRetryWait. An absent or unparsable header still permits one retry after
// a short fixed wait.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return time.Second
	}
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds < 0 {
		return time.Second
	}
	d := time.Duration(seconds) * time.Second
	if d > maxRetryWait {
		return maxRetryWait
	}
	return d
}
