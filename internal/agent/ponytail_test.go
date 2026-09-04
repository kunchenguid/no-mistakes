package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type ponytailTestAgent struct {
	name        string
	calls       []RunOpts
	handoffErr  error
	invalidAck  bool
	workResults []*Result
	workErrors  []error
}

func (a *ponytailTestAgent) Name() string {
	if a.name != "" {
		return a.name
	}
	return "test-agent"
}

func (a *ponytailTestAgent) Close() error { return nil }

func (a *ponytailTestAgent) SupportsSessionResume() bool { return true }

func (a *ponytailTestAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	a.calls = append(a.calls, opts)
	if opts.Purpose == "ponytail-handoff" {
		if a.handoffErr != nil {
			return nil, a.handoffErr
		}
		challenge := acknowledgmentSchemaValue(opts.JSONSchema, "challenge")
		if a.invalidAck {
			challenge = "wrong"
		}
		output, _ := json.Marshal(ponytailAcknowledgment{
			Protocol: PonytailHandoffProtocol, Mode: ponytailMode,
			Challenge: challenge, Acknowledged: true,
		})
		return &Result{Output: output}, nil
	}
	index := 0
	for _, call := range a.calls[:len(a.calls)-1] {
		if call.Purpose != "ponytail-handoff" {
			index++
		}
	}
	if index < len(a.workErrors) && a.workErrors[index] != nil {
		return nil, a.workErrors[index]
	}
	if index < len(a.workResults) {
		return a.workResults[index], nil
	}
	return &Result{Text: "done"}, nil
}

func acknowledgmentSchemaValue(schema json.RawMessage, property string) string {
	var decoded struct {
		Properties map[string]struct {
			Enum []any `json:"enum"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &decoded) != nil || len(decoded.Properties[property].Enum) != 1 {
		return ""
	}
	value, _ := decoded.Properties[property].Enum[0].(string)
	return value
}

func TestPonytailHandoff_AcknowledgesBeforeProjectWork(t *testing.T) {
	inner := &ponytailTestAgent{}
	wrapped := WithPonytailHandoff(inner)
	session := &SessionRef{ID: "session-1", Agent: "test-agent"}

	result, err := wrapped.Run(context.Background(), RunOpts{
		Prompt:  "perform project work",
		CWD:     "/private/project",
		Env:     []string{"SECRET=hidden"},
		Session: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Text != "done" || len(inner.calls) != 2 {
		t.Fatalf("result/calls = %+v/%d, want done/2", result, len(inner.calls))
	}
	handoff, work := inner.calls[0], inner.calls[1]
	if handoff.Session != nil || handoff.Env != nil || handoff.OnChunk != nil || handoff.OnLifecycle != nil || handoff.OnAttempt != nil || handoff.CWD == "/private/project" || strings.Contains(handoff.Prompt, "perform project work") || strings.Contains(handoff.Prompt, "/private/project") {
		t.Fatalf("handoff received project/session/secret context: %+v", handoff)
	}
	if acknowledgmentSchemaValue(handoff.JSONSchema, "protocol") != PonytailHandoffProtocol ||
		acknowledgmentSchemaValue(handoff.JSONSchema, "mode") != ponytailMode {
		t.Fatalf("handoff schema is not the %s full contract: %s", PonytailHandoffProtocol, handoff.JSONSchema)
	}
	if work.Session != session || !strings.Contains(work.Prompt, ponytailOperatingContext) || !strings.HasSuffix(work.Prompt, "perform project work") {
		t.Fatalf("project invocation did not preserve session and Ponytail context: %+v", work)
	}
}

func TestPonytailHandoff_MissingPonytailFailsClosedWithoutLeakage(t *testing.T) {
	inner := &ponytailTestAgent{handoffErr: errors.New("activation missing at /private/plugin with token=secret")}
	_, err := WithPonytailHandoff(inner).Run(context.Background(), RunOpts{
		Prompt: "private project request", CWD: "/private/project", Env: []string{"TOKEN=secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "failed before project work") {
		t.Fatalf("error = %v, want fail-closed handoff diagnostic", err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") || len(inner.calls) != 1 {
		t.Fatalf("error leaked input or project work ran: error=%q calls=%d", err, len(inner.calls))
	}
}

func TestPonytailHandoff_InvalidAcknowledgmentFailsClosed(t *testing.T) {
	inner := &ponytailTestAgent{invalidAck: true}
	_, err := WithPonytailHandoff(inner).Run(context.Background(), RunOpts{Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "invalid acknowledgment") {
		t.Fatalf("error = %v, want invalid acknowledgment", err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("calls = %d, project work ran after a bad acknowledgment", len(inner.calls))
	}
}

func TestPonytailHandoff_ReacknowledgesResumeAndFreshRetry(t *testing.T) {
	inner := &ponytailTestAgent{workErrors: []error{errors.New("resume failed")}}
	wrapped := WithPonytailHandoff(inner)
	if _, err := wrapped.Run(context.Background(), RunOpts{Prompt: "work", Session: &SessionRef{ID: "stale", Agent: "test-agent"}}); err == nil {
		t.Fatal("resume attempt unexpectedly succeeded")
	}
	if _, err := wrapped.Run(context.Background(), RunOpts{Prompt: "work", Session: &SessionRef{}, SessionFallback: true}); err != nil {
		t.Fatalf("fresh retry: %v", err)
	}
	if len(inner.calls) != 4 || inner.calls[0].Session != nil || inner.calls[2].Session != nil {
		t.Fatalf("calls = %+v, want a cold handshake before both project attempts", inner.calls)
	}
}

func TestPonytailHandoff_FallbackAcknowledgesEachProvider(t *testing.T) {
	missing := &ponytailTestAgent{name: "missing", handoffErr: errors.New("missing start: executable not found")}
	available := &ponytailTestAgent{name: "available"}

	result, err := WithPonytailHandoff(NewFallback([]Agent{missing, available})).Run(
		context.Background(), RunOpts{Prompt: "project work"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(missing.calls) != 1 || len(available.calls) != 2 {
		t.Fatalf("result/calls = %#v/%d/%d, want success/1/2", result, len(missing.calls), len(available.calls))
	}
	if missing.calls[0].Purpose != "ponytail-handoff" || available.calls[0].Purpose != "ponytail-handoff" {
		t.Fatalf("fallback providers did not receive independent handshakes: %#v / %#v", missing.calls, available.calls)
	}
}
