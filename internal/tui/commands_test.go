package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOpenBrowserCmd_WaitsForBrowserCommand(t *testing.T) {
	original := runBrowserCommand
	t.Cleanup(func() {
		runBrowserCommand = original
	})

	called := false
	finished := false
	runBrowserCommand = func(name string, args ...string) error {
		called = true
		time.Sleep(50 * time.Millisecond)
		finished = true
		return nil
	}

	start := time.Now()
	msg := openBrowserCmd("https://example.com")()
	if msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}
	if !called {
		t.Fatal("expected browser command to be invoked")
	}
	if !finished {
		t.Fatal("expected browser command to finish before return")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("expected command to block until completion, returned after %v", elapsed)
	}
}

func TestOpenBrowserCmd_ReturnsErrMsgOnFailure(t *testing.T) {
	original := runBrowserCommand
	t.Cleanup(func() {
		runBrowserCommand = original
	})

	wantErr := errors.New("launcher missing")
	runBrowserCommand = func(name string, args ...string) error {
		return wantErr
	}

	msg := openBrowserCmd("https://example.com")()
	errMsg, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("expected errMsg, got %#v", msg)
	}
	if !errors.Is(errMsg.err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, errMsg.err)
	}
}

func TestBrowserCommandSpec_WindowsUsesRundll32(t *testing.T) {
	name, args := browserCommandSpec("windows", "https://example.com/pull/1?foo=1&bar=2")

	if name != "rundll32" {
		t.Fatalf("expected rundll32 launcher, got %q", name)
	}

	wantArgs := []string{"url.dll,FileProtocolHandler", "https://example.com/pull/1?foo=1&bar=2"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("unexpected args: got %v want %v", args, wantArgs)
	}
}

func TestModel_Update_OpenPRKeyRunsBrowserCommand(t *testing.T) {
	original := runBrowserCommand
	t.Cleanup(func() {
		runBrowserCommand = original
	})

	prURL := "https://github.com/test/repo/pull/42"
	run := testRun()
	run.PRURL = &prURL
	m := NewModel("/tmp/sock", nil, run)

	called := false
	var gotName string
	var gotArgs []string
	runBrowserCommand = func(name string, args ...string) error {
		called = true
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if cmd == nil {
		t.Fatal("expected browser open command")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}
	if !called {
		t.Fatal("expected browser launcher to be called")
	}

	wantName, wantArgs := browserCommandSpec(runtime.GOOS, prURL)
	if gotName != wantName {
		t.Fatalf("unexpected command name: got %q want %q", gotName, wantName)
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("unexpected command args: got %v want %v", gotArgs, wantArgs)
	}
}

func TestModel_Update_RerunKeyStartsNewRunAndSwitchesModel(t *testing.T) {
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)

	newRun := testRun()
	newRun.ID = "run-002"
	newRun.Status = types.RunRunning
	newRun.Error = nil

	srv.Handle(ipc.MethodRerun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RerunParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.RepoID != "repo-001" || params.Branch != "feature/foo" {
			return nil, fmt.Errorf("unexpected rerun params: %#v", params)
		}
		return &ipc.RerunResult{RunID: newRun.ID}, nil
	})
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.GetRunParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.RunID != newRun.ID {
			return nil, fmt.Errorf("unexpected get_run id: %s", params.RunID)
		}
		return &ipc.GetRunResult{Run: newRun}, nil
	})
	srv.HandleStream(ipc.MethodSubscribe, func(_ context.Context, raw json.RawMessage, send func(interface{}) error) error {
		var params ipc.SubscribeParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return err
		}
		if params.RunID != newRun.ID {
			return fmt.Errorf("unexpected subscribe id: %s", params.RunID)
		}
		return nil
	})

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	run := testRun()
	run.Status = types.RunFailed
	run.Error = ptr("push failed")
	m := NewModel(sock, client, run)
	m.width = 80
	m.height = 24
	m.err = errors.New("old error")
	m.logs = []string{"old log"}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("expected rerun command")
	}
	msg := cmd()
	if _, ok := msg.(rerunStartedMsg); !ok {
		t.Fatalf("expected rerunStartedMsg, got %T", msg)
	}

	updated, nextCmd := updated.(Model).Update(msg)
	model := updated.(Model)
	if model.runID != newRun.ID {
		t.Fatalf("runID = %s, want %s", model.runID, newRun.ID)
	}
	if model.run == nil || model.run.ID != newRun.ID {
		t.Fatalf("run = %#v, want new run %#v", model.run, newRun)
	}
	if model.run.Status != types.RunRunning {
		t.Fatalf("run status = %s, want %s", model.run.Status, types.RunRunning)
	}
	if model.done {
		t.Fatal("expected rerun model to no longer be done")
	}
	if model.err != nil {
		t.Fatalf("expected rerun to clear error, got %v", model.err)
	}
	if len(model.logs) != 0 {
		t.Fatalf("expected rerun to clear logs, got %v", model.logs)
	}
	if nextCmd == nil {
		t.Fatal("expected subscribe command after rerun")
	}
}

func TestModel_Update_RerunStartedSkipsSubscribeForTerminalRun(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	terminalRun := testRun()
	terminalRun.ID = "run-002"
	terminalRun.Status = types.RunFailed
	terminalRun.Error = ptr("fast failure")

	updated, cmd := m.Update(rerunStartedMsg{run: terminalRun})
	model := updated.(Model)

	if model.runID != terminalRun.ID {
		t.Fatalf("runID = %s, want %s", model.runID, terminalRun.ID)
	}
	if !model.done {
		t.Fatal("expected terminal rerun to mark model done")
	}
	if cmd != nil {
		t.Fatal("expected no subscribe command for terminal rerun")
	}
}

func TestModel_Update_RerunPreservesLatestVersion(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	m := NewModel("/tmp/sock", nil, run)
	m.latestVersion = "v1.2.3"

	newRun := testRun()
	newRun.ID = "run-002"
	newRun.Status = types.RunRunning

	updated, _ := m.Update(rerunStartedMsg{run: newRun})
	model := updated.(Model)

	if model.latestVersion != "v1.2.3" {
		t.Fatalf("latestVersion = %q, want %q", model.latestVersion, "v1.2.3")
	}
}

func TestModel_SubscribeCmdReturnsScopedError(t *testing.T) {
	run := testRun()
	m := NewModel(filepath.Join(t.TempDir(), "missing.sock"), nil, run)
	m.subscriptionID = 7

	cmd := m.subscribeCmd()
	if cmd == nil {
		t.Fatal("expected subscribe command")
	}

	msg := cmd()
	subErr, ok := msg.(subscriptionErrMsg)
	if !ok {
		t.Fatalf("expected subscriptionErrMsg, got %T", msg)
	}
	if subErr.subscriptionID != m.subscriptionID {
		t.Fatalf("subscriptionID = %d, want %d", subErr.subscriptionID, m.subscriptionID)
	}
	if subErr.err == nil || !strings.Contains(subErr.err.Error(), "subscribe:") {
		t.Fatalf("expected wrapped subscribe error, got %v", subErr.err)
	}
	if _, ok := msg.(errMsg); ok {
		t.Fatal("expected subscribe failure to avoid errMsg")
	}
}

func TestModel_Update_IgnoresStaleSubscriptionMessagesAfterRerun(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	oldEvents := make(chan ipc.Event)
	oldCancelled := false
	updated, _ := m.Update(connectedMsg{events: oldEvents, cancelSub: func() { oldCancelled = true }, subscriptionID: m.subscriptionID})
	model := updated.(Model)

	newRun := testRun()
	newRun.ID = "run-002"
	newRun.Status = types.RunRunning

	updated, cmd := model.Update(rerunStartedMsg{run: newRun})
	model = updated.(Model)

	if !oldCancelled {
		t.Fatal("expected rerun to cancel the previous subscription")
	}
	if cmd == nil {
		t.Fatal("expected rerun to subscribe to the new run")
	}

	newEvents := make(chan ipc.Event)
	updated, _ = model.Update(connectedMsg{events: newEvents, cancelSub: func() {}, subscriptionID: model.subscriptionID})
	model = updated.(Model)

	staleCancelled := false
	updated, _ = model.Update(connectedMsg{events: make(chan ipc.Event), cancelSub: func() { staleCancelled = true }, subscriptionID: model.subscriptionID - 1})
	model = updated.(Model)
	if !staleCancelled {
		t.Fatal("expected stale connected message to be cancelled")
	}
	if model.events != newEvents {
		t.Fatal("expected stale connected message to be ignored")
	}

	updated, _ = model.Update(subscriptionErrMsg{err: errors.New("event stream closed"), subscriptionID: model.subscriptionID - 1})
	model = updated.(Model)
	if model.err != nil {
		t.Fatalf("expected stale subscription error to be ignored, got %v", model.err)
	}

	staleStatus := string(types.RunFailed)
	staleError := "stale completion"
	updated, _ = model.Update(eventMsg{event: ipc.Event{Type: ipc.EventRunCompleted, Status: &staleStatus, Error: &staleError}, subscriptionID: model.subscriptionID - 1})
	model = updated.(Model)
	if model.done {
		t.Fatal("expected stale event to be ignored")
	}
	if model.run == nil || model.run.ID != newRun.ID {
		t.Fatalf("run = %#v, want rerun %#v", model.run, newRun)
	}
	if model.run.Status != types.RunRunning {
		t.Fatalf("run status = %s, want %s", model.run.Status, types.RunRunning)
	}
}

func TestModel_Update_IgnoresRepeatedRerunKeyWhilePending(t *testing.T) {
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	run := testRun()
	run.Status = types.RunFailed
	m := NewModel(sock, client, run)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model := updated.(Model)
	if cmd == nil {
		t.Fatal("expected first rerun key to start rerun")
	}
	if !model.rerunPending {
		t.Fatal("expected rerun to become pending")
	}
	if model.rerunRequestID != 1 {
		t.Fatalf("rerunRequestID = %d, want 1", model.rerunRequestID)
	}

	updated, secondCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model = updated.(Model)
	if secondCmd != nil {
		t.Fatal("expected repeated rerun key to be ignored while pending")
	}
	if !model.rerunPending {
		t.Fatal("expected rerun to remain pending")
	}
	if model.rerunRequestID != 1 {
		t.Fatalf("rerunRequestID = %d, want 1", model.rerunRequestID)
	}
	_ = srv
}

func TestModel_Update_IgnoresStaleRerunStartedMessage(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.rerunRequestID = 2

	staleRun := testRun()
	staleRun.ID = "run-002"
	staleRun.Status = types.RunRunning

	updated, cmd := m.Update(rerunStartedMsg{run: staleRun, requestID: 1})
	model := updated.(Model)

	if model.runID != run.ID {
		t.Fatalf("runID = %s, want %s", model.runID, run.ID)
	}
	if model.run == nil || model.run.ID != run.ID {
		t.Fatalf("run = %#v, want original run %#v", model.run, run)
	}
	if cmd != nil {
		t.Fatal("expected stale rerun message to be ignored")
	}

	currentRun := testRun()
	currentRun.ID = "run-003"
	currentRun.Status = types.RunRunning

	updated, cmd = model.Update(rerunStartedMsg{run: currentRun, requestID: 2})
	model = updated.(Model)

	if model.runID != currentRun.ID {
		t.Fatalf("runID = %s, want %s", model.runID, currentRun.ID)
	}
	if model.run == nil || model.run.ID != currentRun.ID {
		t.Fatalf("run = %#v, want current run %#v", model.run, currentRun)
	}
	if cmd == nil {
		t.Fatal("expected current rerun message to subscribe")
	}
}
