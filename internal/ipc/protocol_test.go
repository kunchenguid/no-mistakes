package ipc

import (
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRequestMarshal(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		Method:  MethodHealth,
		ID:      1,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", got.JSONRPC, "2.0")
	}
	if got.Method != MethodHealth {
		t.Errorf("method = %q, want %q", got.Method, MethodHealth)
	}
	if got.ID != 1 {
		t.Errorf("id = %v, want 1", got.ID)
	}
}

func TestRequestWithParams(t *testing.T) {
	params := PushReceivedParams{
		Gate: "/home/user/.no-mistakes/repos/abc123.git",
		Ref:  "refs/heads/feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  "abc123def456",
	}
	raw, _ := json.Marshal(params)
	req := Request{
		JSONRPC: "2.0",
		Method:  MethodPushReceived,
		Params:  raw,
		ID:      42,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var gotParams PushReceivedParams
	if err := json.Unmarshal(got.Params, &gotParams); err != nil {
		t.Fatal(err)
	}
	if gotParams.Gate != params.Gate {
		t.Errorf("gate = %q, want %q", gotParams.Gate, params.Gate)
	}
	if gotParams.Ref != params.Ref {
		t.Errorf("ref = %q, want %q", gotParams.Ref, params.Ref)
	}
}

func TestResponseSuccess(t *testing.T) {
	result := HealthResult{Status: "ok"}
	raw, _ := json.Marshal(result)
	resp := Response{
		JSONRPC: "2.0",
		Result:  raw,
		ID:      1,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != nil {
		t.Errorf("error should be nil")
	}
	var gotResult HealthResult
	if err := json.Unmarshal(got.Result, &gotResult); err != nil {
		t.Fatal(err)
	}
	if gotResult.Status != "ok" {
		t.Errorf("status = %q, want %q", gotResult.Status, "ok")
	}
}

func TestResponseError(t *testing.T) {
	resp := Response{
		JSONRPC: "2.0",
		Error: &RPCError{
			Code:    ErrMethodNotFound,
			Message: "method not found",
		},
		ID: 1,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil {
		t.Fatal("error should not be nil")
	}
	if got.Error.Code != ErrMethodNotFound {
		t.Errorf("code = %d, want %d", got.Error.Code, ErrMethodNotFound)
	}
	if got.Error.Message != "method not found" {
		t.Errorf("message = %q, want %q", got.Error.Message, "method not found")
	}
}

func TestPushReceivedParams(t *testing.T) {
	params := PushReceivedParams{
		Gate: "/path/to/gate.git",
		Ref:  "refs/heads/main",
		Old:  "aaa",
		New:  "bbb",
	}
	data, _ := json.Marshal(params)
	var got PushReceivedParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Gate != params.Gate || got.Ref != params.Ref || got.Old != params.Old || got.New != params.New {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, params)
	}
}

func TestGetRunParams(t *testing.T) {
	params := GetRunParams{RunID: "01ABCDEF"}
	data, _ := json.Marshal(params)
	var got GetRunParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "01ABCDEF" {
		t.Errorf("run_id = %q, want %q", got.RunID, "01ABCDEF")
	}
}

func TestGetRunsParams(t *testing.T) {
	params := GetRunsParams{RepoID: "repo123"}
	data, _ := json.Marshal(params)
	var got GetRunsParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RepoID != "repo123" {
		t.Errorf("repo_id = %q, want %q", got.RepoID, "repo123")
	}
}

func TestGetActiveRunParams(t *testing.T) {
	params := GetActiveRunParams{RepoID: "repo456"}
	data, _ := json.Marshal(params)
	var got GetActiveRunParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RepoID != "repo456" {
		t.Errorf("repo_id = %q, want %q", got.RepoID, "repo456")
	}
}

func TestRerunParams(t *testing.T) {
	params := RerunParams{RepoID: "repo456", Branch: "feature"}
	data, _ := json.Marshal(params)
	var got RerunParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RepoID != "repo456" {
		t.Errorf("repo_id = %q, want %q", got.RepoID, "repo456")
	}
	if got.Branch != "feature" {
		t.Errorf("branch = %q, want %q", got.Branch, "feature")
	}
}

func TestSubscribeParams(t *testing.T) {
	params := SubscribeParams{RunID: "run789"}
	data, _ := json.Marshal(params)
	var got SubscribeParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run789" {
		t.Errorf("run_id = %q, want %q", got.RunID, "run789")
	}
}

func TestRespondParams(t *testing.T) {
	params := RespondParams{
		RunID:      "run123",
		Step:       types.StepReview,
		Action:     types.ActionApprove,
		FindingIDs: []string{"review-1", "review-2"},
	}
	data, _ := json.Marshal(params)
	var got RespondParams
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run123" {
		t.Errorf("run_id = %q, want %q", got.RunID, "run123")
	}
	if got.Step != types.StepReview {
		t.Errorf("step = %q, want %q", got.Step, types.StepReview)
	}
	if got.Action != types.ActionApprove {
		t.Errorf("action = %q, want %q", got.Action, types.ActionApprove)
	}
	if len(got.FindingIDs) != 2 || got.FindingIDs[0] != "review-1" || got.FindingIDs[1] != "review-2" {
		t.Errorf("finding_ids = %#v, want %#v", got.FindingIDs, []string{"review-1", "review-2"})
	}
}

func TestRunInfoRoundTrip(t *testing.T) {
	prURL := "https://github.com/user/repo/pull/42"
	info := RunInfo{
		ID:        "run001",
		RepoID:    "repo001",
		Branch:    "feature",
		HeadSHA:   "abc123",
		BaseSHA:   "def456",
		Status:    types.RunRunning,
		PRURL:     &prURL,
		CreatedAt: 1700000000,
		UpdatedAt: 1700000001,
	}
	data, _ := json.Marshal(info)
	var got RunInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != info.ID || got.Branch != info.Branch || got.Status != info.Status {
		t.Errorf("mismatch: got %+v", got)
	}
	if got.PRURL == nil || *got.PRURL != prURL {
		t.Errorf("pr_url = %v, want %q", got.PRURL, prURL)
	}
}

func TestStepResultInfoRoundTrip(t *testing.T) {
	exitCode := 0
	dur := int64(1234)
	info := StepResultInfo{
		ID:         "step001",
		RunID:      "run001",
		StepName:   types.StepTest,
		StepOrder:  2,
		Status:     types.StepStatusCompleted,
		ExitCode:   &exitCode,
		DurationMS: &dur,
	}
	data, _ := json.Marshal(info)
	var got StepResultInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.StepName != types.StepTest || got.StepOrder != 2 {
		t.Errorf("mismatch: got %+v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", got.ExitCode)
	}
}

func TestEventTypes(t *testing.T) {
	tests := []struct {
		name  string
		event Event
	}{
		{
			name: "run_updated",
			event: Event{
				Type:   EventRunUpdated,
				RunID:  "run001",
				RepoID: "repo001",
				Status: ptrStr(string(types.RunRunning)),
			},
		},
		{
			name: "step_started",
			event: Event{
				Type:     EventStepStarted,
				RunID:    "run001",
				RepoID:   "repo001",
				StepName: ptrStepName(types.StepReview),
			},
		},
		{
			name: "step_completed",
			event: Event{
				Type:     EventStepCompleted,
				RunID:    "run001",
				RepoID:   "repo001",
				StepName: ptrStepName(types.StepLint),
				Status:   ptrStr(string(types.StepStatusCompleted)),
			},
		},
		{
			name: "run_completed_with_error",
			event: Event{
				Type:   EventRunCompleted,
				RunID:  "run001",
				RepoID: "repo001",
				Status: ptrStr(string(types.RunFailed)),
				Error:  ptrStr("step review failed"),
			},
		},
		{
			name: "log_chunk",
			event: Event{
				Type:    EventLogChunk,
				RunID:   "run001",
				RepoID:  "repo001",
				Stream:  ptrStr("stdout"),
				Content: ptrStr("test output line\n"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.event)
			if err != nil {
				t.Fatal(err)
			}
			var got Event
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Type != tt.event.Type {
				t.Errorf("type = %q, want %q", got.Type, tt.event.Type)
			}
			if got.RunID != tt.event.RunID {
				t.Errorf("run_id = %q, want %q", got.RunID, tt.event.RunID)
			}
			if tt.event.Error != nil {
				if got.Error == nil || *got.Error != *tt.event.Error {
					t.Errorf("error = %v, want %q", got.Error, *tt.event.Error)
				}
			}
		})
	}
}

func TestNullableFieldsOmitted(t *testing.T) {
	info := RunInfo{
		ID:        "run001",
		RepoID:    "repo001",
		Branch:    "main",
		HeadSHA:   "abc",
		BaseSHA:   "def",
		Status:    types.RunPending,
		CreatedAt: 1700000000,
		UpdatedAt: 1700000000,
	}
	data, _ := json.Marshal(info)
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if _, ok := raw["pr_url"]; ok {
		t.Error("pr_url should be omitted when nil")
	}
	if _, ok := raw["error"]; ok {
		t.Error("error should be omitted when nil")
	}
}

func TestMethodConstants(t *testing.T) {
	methods := []string{
		MethodPushReceived,
		MethodGetRun,
		MethodGetRuns,
		MethodGetActiveRun,
		MethodRerun,
		MethodSubscribe,
		MethodRespond,
		MethodCancelRun,
		MethodHealth,
		MethodShutdown,
	}
	seen := make(map[string]bool)
	for _, m := range methods {
		if m == "" {
			t.Error("method constant should not be empty")
		}
		if seen[m] {
			t.Errorf("duplicate method: %q", m)
		}
		seen[m] = true
	}
	if len(methods) != 10 {
		t.Errorf("expected 10 methods, got %d", len(methods))
	}
}

func TestErrorCodes(t *testing.T) {
	codes := []int{ErrParseError, ErrInvalidRequest, ErrMethodNotFound, ErrInvalidParams, ErrInternal}
	seen := make(map[int]bool)
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate error code: %d", c)
		}
		seen[c] = true
	}
}

func TestNewRequest(t *testing.T) {
	params := HealthParams{}
	req, err := NewRequest(MethodHealth, params)
	if err != nil {
		t.Fatal(err)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", req.JSONRPC, "2.0")
	}
	if req.Method != MethodHealth {
		t.Errorf("method = %q, want %q", req.Method, MethodHealth)
	}
	if req.ID == 0 {
		t.Error("id should be non-zero")
	}
}

func TestNewResponse(t *testing.T) {
	result := HealthResult{Status: "ok"}
	resp, err := NewResponse(42, result)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.ID != 42 {
		t.Errorf("id = %d, want 42", resp.ID)
	}
	if resp.Error != nil {
		t.Error("error should be nil for success response")
	}
}

func TestNewErrorResponse(t *testing.T) {
	resp := NewErrorResponse(42, ErrMethodNotFound, "unknown method")
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.ID != 42 {
		t.Errorf("id = %d, want 42", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("error should not be nil")
	}
	if resp.Error.Code != ErrMethodNotFound {
		t.Errorf("code = %d, want %d", resp.Error.Code, ErrMethodNotFound)
	}
	if resp.Result != nil {
		t.Error("result should be nil for error response")
	}
}

func TestRPCErrorError(t *testing.T) {
	e := &RPCError{Code: ErrInternal, Message: "something broke"}
	if got := e.Error(); got != "something broke" {
		t.Errorf("Error() = %q, want %q", got, "something broke")
	}
}

func ptrStr(s string) *string                      { return &s }
func ptrStepName(s types.StepName) *types.StepName { return &s }
