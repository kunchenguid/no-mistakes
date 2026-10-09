package tui

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A finding the operator left unchecked that an earlier round of this step
// already chose to fix cannot be declined. The TUI retries once with it
// omitted, which keeps the earlier decision, so the refusal never reaches the
// operator as an error they can do nothing about.
func TestModel_RespondFixKeepsAnAlreadyFixedDeclineOutOfTheRetry(t *testing.T) {
	findings := `{"findings":[` +
		`{"id":"r1","severity":"error","description":"already fixed in round 1","action":"ask-user"},` +
		`{"id":"r2","severity":"warning","description":"new concern","action":"auto-fix"}]}`

	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)
	var mu sync.Mutex
	var calls []ipc.RespondParams
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RespondParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		mu.Lock()
		calls = append(calls, params)
		attempt := len(calls)
		mu.Unlock()
		if attempt == 1 {
			return &ipc.RespondResult{
				OK:                 false,
				Refusal:            "r1 already chosen to fix in an earlier round of this step",
				DeclinedEarlierFix: []string{"r1"},
				Help:               "Reverting an applied fix is out of scope for a gate response",
			}, nil
		}
		return &ipc.RespondResult{OK: true, Fixed: []string{"r2"}, Kept: []string{"r1"}}, nil
	})

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = &findings
	m := NewModel(sock, client, run)
	m.ensureFindingSelection(types.StepReview)
	// The operator unchecks r1, which they consider already fixed, and leaves
	// the new finding r2 selected.
	delete(m.findingSelections[types.StepReview], "r1")

	cmd := m.respondCmd(types.ActionFix)
	if cmd == nil {
		t.Fatal("expected a fix command")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("a recoverable refusal must not surface as an error message, got %#v", msg)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("respond calls = %d, want the refused attempt and its correction", len(calls))
	}
	if strings.Join(calls[0].FindingIDs, ",") != "r2" || strings.Join(calls[0].IgnoreFindingIDs, ",") != "r1" {
		t.Fatalf("first attempt = %+v, want r2 fixed and r1 declined", calls[0])
	}
	if strings.Join(calls[1].FindingIDs, ",") != "r2" || len(calls[1].IgnoreFindingIDs) != 0 {
		t.Fatalf("retry = %+v, want r1 omitted so the earlier fix is kept", calls[1])
	}
}
