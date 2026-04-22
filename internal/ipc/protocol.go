package ipc

import (
	"encoding/json"
	"sync/atomic"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// JSON-RPC 2.0 method names.
const (
	MethodPushReceived = "push_received"
	MethodGetRun       = "get_run"
	MethodGetRuns      = "get_runs"
	MethodGetActiveRun = "get_active_run"
	MethodRerun        = "rerun"
	MethodSubscribe    = "subscribe"
	MethodRespond      = "respond"
	MethodCancelRun    = "cancel_run"
	MethodHealth       = "health"
	MethodShutdown     = "shutdown"
)

// JSON-RPC 2.0 error codes.
const (
	ErrParseError     = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int64           `json:"id"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      int64           `json:"id"`
}

// RPCError represents a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// --- Method parameters ---

// PushReceivedParams are sent by the post-receive hook when a push arrives.
type PushReceivedParams struct {
	Gate string `json:"gate"`
	Ref  string `json:"ref"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// GetRunParams requests a single run by ID.
type GetRunParams struct {
	RunID string `json:"run_id"`
}

// GetRunsParams requests all runs for a repo.
type GetRunsParams struct {
	RepoID string `json:"repo_id"`
}

// GetActiveRunParams requests the active run for a repo.
// When Branch is set, runs on that branch are preferred.
type GetActiveRunParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch,omitempty"`
}

// RerunParams requests a new run for the latest gate head on a branch.
type RerunParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

// SubscribeParams starts an event stream for a run.
type SubscribeParams struct {
	RunID string `json:"run_id"`
}

// RespondParams sends a user action for a step awaiting approval.
//
// Instructions carries optional per-finding notes keyed by finding ID, which
// the daemon attaches to the corresponding finding before dispatching a fix.
// AddedFindings carries user-authored findings that are merged into the round
// alongside agent-produced ones. Both fields only apply when Action triggers
// a fix round.
type RespondParams struct {
	RunID         string               `json:"run_id"`
	Step          types.StepName       `json:"step"`
	Action        types.ApprovalAction `json:"action"`
	FindingIDs    []string             `json:"finding_ids,omitempty"`
	Instructions  map[string]string    `json:"instructions,omitempty"`
	AddedFindings []types.Finding      `json:"added_findings,omitempty"`
}

// CancelRunParams cancels an active pipeline run.
type CancelRunParams struct {
	RunID string `json:"run_id"`
}

// HealthParams has no fields but exists for consistency.
type HealthParams struct{}

// ShutdownParams has no fields but exists for consistency.
type ShutdownParams struct{}

// --- Method results ---

// PushReceivedResult confirms the push was accepted.
type PushReceivedResult struct {
	RunID string `json:"run_id"`
}

// GetRunResult wraps a single run.
type GetRunResult struct {
	Run *RunInfo `json:"run"`
}

// GetRunsResult wraps a list of runs.
type GetRunsResult struct {
	Runs []RunInfo `json:"runs"`
}

// GetActiveRunResult wraps the active run (nil if none).
type GetActiveRunResult struct {
	Run *RunInfo `json:"run,omitempty"`
}

// RerunResult confirms a rerun was created.
type RerunResult struct {
	RunID string `json:"run_id"`
}

// RespondResult confirms the action was accepted.
type RespondResult struct {
	OK bool `json:"ok"`
}

// CancelRunResult confirms the run cancellation request was accepted.
type CancelRunResult struct {
	OK bool `json:"ok"`
}

// HealthResult confirms the daemon is alive.
type HealthResult struct {
	Status string `json:"status"`
}

// ShutdownResult confirms shutdown was initiated.
type ShutdownResult struct {
	OK bool `json:"ok"`
}

// --- Wire types ---

// RunInfo is the IPC representation of a pipeline run.
type RunInfo struct {
	ID        string           `json:"id"`
	RepoID    string           `json:"repo_id"`
	Branch    string           `json:"branch"`
	HeadSHA   string           `json:"head_sha"`
	BaseSHA   string           `json:"base_sha"`
	Status    types.RunStatus  `json:"status"`
	PRURL     *string          `json:"pr_url,omitempty"`
	Error     *string          `json:"error,omitempty"`
	Steps     []StepResultInfo `json:"steps,omitempty"`
	CreatedAt int64            `json:"created_at"`
	UpdatedAt int64            `json:"updated_at"`
}

// StepResultInfo is the IPC representation of a step result.
type StepResultInfo struct {
	ID           string           `json:"id"`
	RunID        string           `json:"run_id"`
	StepName     types.StepName   `json:"step_name"`
	StepOrder    int              `json:"step_order"`
	Status       types.StepStatus `json:"status"`
	ExitCode     *int             `json:"exit_code,omitempty"`
	DurationMS   *int64           `json:"duration_ms,omitempty"`
	FindingsJSON *string          `json:"findings_json,omitempty"`
	Error        *string          `json:"error,omitempty"`
	StartedAt    *int64           `json:"started_at,omitempty"`
	CompletedAt  *int64           `json:"completed_at,omitempty"`
}

// --- Events (for subscribe stream) ---

// EventType identifies the kind of event.
type EventType string

const (
	EventRunCreated    EventType = "run_created"
	EventRunUpdated    EventType = "run_updated"
	EventRunCompleted  EventType = "run_completed"
	EventStepStarted   EventType = "step_started"
	EventStepCompleted EventType = "step_completed"
	EventLogChunk      EventType = "log_chunk"
)

// Event is a real-time update sent to subscribers.
type Event struct {
	Type       EventType       `json:"type"`
	RunID      string          `json:"run_id"`
	RepoID     string          `json:"repo_id"`
	StepName   *types.StepName `json:"step_name,omitempty"`
	Status     *string         `json:"status,omitempty"`
	Error      *string         `json:"error,omitempty"`
	Stream     *string         `json:"stream,omitempty"`
	Content    *string         `json:"content,omitempty"`
	Branch     *string         `json:"branch,omitempty"`
	Findings   *string         `json:"findings,omitempty"`    // JSON-encoded findings for step_completed events
	Diff       *string         `json:"diff,omitempty"`        // unified diff for fix_review events
	DurationMS *int64          `json:"duration_ms,omitempty"` // execution-only duration for step events
	PRURL      *string         `json:"pr_url,omitempty"`      // PR URL for run_updated/run_completed events
}

// --- Helpers ---

var reqID atomic.Int64

// NewRequest creates a JSON-RPC 2.0 request with an auto-incremented ID.
func NewRequest(method string, params interface{}) (*Request, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
		ID:      reqID.Add(1),
	}, nil
}

// NewResponse creates a successful JSON-RPC 2.0 response.
func NewResponse(id int64, result interface{}) (*Response, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Response{
		JSONRPC: "2.0",
		Result:  raw,
		ID:      id,
	}, nil
}

// NewErrorResponse creates an error JSON-RPC 2.0 response.
func NewErrorResponse(id int64, code int, message string) *Response {
	return &Response{
		JSONRPC: "2.0",
		Error:   &RPCError{Code: code, Message: message},
		ID:      id,
	}
}
