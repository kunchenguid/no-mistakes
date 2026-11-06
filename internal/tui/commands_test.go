package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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
	dir := tuiIdentityCheckout(t)
	t.Chdir(dir)
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)
	srv.Handle(ipc.MethodProbeCommitIdentity, func(context.Context, json.RawMessage) (interface{}, error) {
		return map[string]bool{"ok": true}, nil
	})

	newRun := testRun()
	newRun.ID = "run-002"
	newRun.Status = types.RunRunning
	newRun.Error = nil

	homes := t.TempDir()
	claudeHome, codexHome := filepath.Join(homes, "claude account"), filepath.Join(homes, "codex account")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	srv.Handle(ipc.MethodProbeTranscriptHomes, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.ProbeTranscriptHomesResult{OK: true}, nil
	})
	srv.Handle(ipc.MethodRerun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RerunParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.RepoID != "repo-001" || params.Branch != "feature/foo" || params.PreviousRunID != "run-001" {
			return nil, fmt.Errorf("unexpected rerun params: %#v", params)
		}
		if params.CommitIdentity == nil || params.CommitIdentity.AuthorName != "TUI Submitter" || params.CommitIdentity.CommitterEmail != "tui@example.test" {
			return nil, fmt.Errorf("rerun lost submitting identity: %+v", params.CommitIdentity)
		}
		if params.TranscriptHomes == nil || params.TranscriptHomes.ClaudeConfigDir != claudeHome || params.TranscriptHomes.CodexHome != codexHome {
			return nil, fmt.Errorf("rerun lost the selected transcript homes: %#v", params.TranscriptHomes)
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
	srv.HandleStream(ipc.MethodSubscribe, func(_ context.Context, raw json.RawMessage) (ipc.StreamFunc, error) {
		var params ipc.SubscribeParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.RunID != newRun.ID {
			return nil, fmt.Errorf("unexpected subscribe id: %s", params.RunID)
		}
		return func(func(interface{}) error) error { return nil }, nil
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

// A legacy rerun handler would ignore the homes and discover another account.
// Neither failed nor cancelled runs may reach it without capability support.
func TestModel_RerunRefusesOlderDaemonTranscriptDiscovery(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunFailed, types.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			sock := testSocketPath(t)
			srv := startTestIPCServer(t, sock)
			requests := make(chan struct{}, 1)
			srv.Handle(ipc.MethodRerun, func(context.Context, json.RawMessage) (interface{}, error) {
				requests <- struct{}{}
				return &ipc.RerunResult{RunID: "wrong-account-discovery"}, nil
			})
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "selected claude"))
			t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "selected codex"))
			client, err := ipc.Dial(sock)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			run := testRun()
			run.Status = status
			m := NewModel(sock, client, run)
			cmd := m.rerunCmd(42)
			if cmd == nil {
				t.Fatal("expected rerun command")
			}
			msg := cmd()
			failure, ok := msg.(rerunErrMsg)
			if !ok || failure.requestID != 42 || !strings.Contains(failure.err.Error(), "no-mistakes daemon restart") {
				t.Fatalf("expected restart guidance for the rerun request, got %#v", msg)
			}
			select {
			case <-requests:
				t.Fatal("older daemon received a request that could read another account")
			default:
			}
		})
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

func TestModel_Update_RerunStartedBackfillsMissingPipelineSteps(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	m := NewModel("/tmp/sock", nil, run)

	newRun := &ipc.RunInfo{
		ID:      "run-002",
		RepoID:  run.RepoID,
		Branch:  run.Branch,
		HeadSHA: run.HeadSHA,
		BaseSHA: run.BaseSHA,
		Status:  types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{
				ID:        "s0",
				RunID:     "run-002",
				StepName:  types.StepIntent,
				StepOrder: types.StepIntent.Order(),
				Status:    types.StepStatusSkipped,
			},
			{
				ID:        "s1",
				RunID:     "run-002",
				StepName:  types.StepRebase,
				StepOrder: types.StepRebase.Order(),
				Status:    types.StepStatusRunning,
			},
		},
	}

	updated, _ := m.Update(rerunStartedMsg{run: newRun})
	model := updated.(Model)

	if len(model.steps) != len(types.AllSteps()) {
		t.Fatalf("step count = %d, want %d", len(model.steps), len(types.AllSteps()))
	}
	for i, stepName := range types.AllSteps() {
		if model.steps[i].StepName != stepName {
			t.Fatalf("step %d = %s, want %s", i, model.steps[i].StepName, stepName)
		}
	}
	rebaseIdx := types.StepRebase.Order() - 1
	reviewIdx := types.StepReview.Order() - 1
	if model.steps[rebaseIdx].Status != types.StepStatusRunning {
		t.Fatalf("rebase status = %s, want %s", model.steps[rebaseIdx].Status, types.StepStatusRunning)
	}
	if model.steps[reviewIdx].Status != types.StepStatusPending {
		t.Fatalf("review status = %s, want %s", model.steps[reviewIdx].Status, types.StepStatusPending)
	}

	plain := stripANSI(renderPipelineView(model.run, model.steps, 80, 0, 40))
	for _, label := range []string{"Intent", "Rebase", "Review", "Test", "Document", "Lint", "Push", "PR", "CI"} {
		if !strings.Contains(plain, label) {
			t.Fatalf("expected pipeline view to contain %q, got:\n%s", label, plain)
		}
	}

	review := types.StepReview
	model.applyEvent(ipc.Event{Type: ipc.EventStepStarted, StepName: &review})
	if model.steps[reviewIdx].Status != types.StepStatusRunning {
		t.Fatalf("review status after event = %s, want %s", model.steps[reviewIdx].Status, types.StepStatusRunning)
	}
}

func TestModel_Update_RerunStartedBackfillsEmptyRunningPipelineSteps(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	m := NewModel("/tmp/sock", nil, run)

	newRun := &ipc.RunInfo{
		ID:      "run-002",
		RepoID:  run.RepoID,
		Branch:  run.Branch,
		HeadSHA: run.HeadSHA,
		BaseSHA: run.BaseSHA,
		Status:  types.RunRunning,
	}

	updated, _ := m.Update(rerunStartedMsg{run: newRun})
	model := updated.(Model)

	if len(model.steps) != len(types.AllSteps()) {
		t.Fatalf("step count = %d, want %d", len(model.steps), len(types.AllSteps()))
	}
	for i, stepName := range types.AllSteps() {
		if model.steps[i].StepName != stepName {
			t.Fatalf("step %d = %s, want %s", i, model.steps[i].StepName, stepName)
		}
		if model.steps[i].Status != types.StepStatusPending {
			t.Fatalf("step %s status = %s, want %s", stepName, model.steps[i].Status, types.StepStatusPending)
		}
	}
}

func TestNewModel_DoesNotBackfillEmptyTerminalPipelineSteps(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	run.Steps = nil

	m := NewModel("/tmp/sock", nil, run)

	if len(m.steps) != 0 {
		t.Fatalf("step count = %d, want 0", len(m.steps))
	}
	if len(m.run.Steps) != 0 {
		t.Fatalf("run step count = %d, want 0", len(m.run.Steps))
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

func tuiIdentityCheckout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	global := filepath.Join(root, "global")
	if err := os.WriteFile(global, []byte("[user]\nname = TUI Submitter\nemail = tui@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	appRoot := t.TempDir()
	t.Setenv("NM_HOME", appRoot)
	database, err := db.Open(paths.WithRoot(appRoot).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	checkout, err := git.FindGitRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepoWithID("repo-001", checkout, "https://example.test/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestModel_RerunBindsLinkedCheckoutIdentity(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunFailed, types.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			main := tuiIdentityCheckout(t)
			gitCmd := func(dir string, args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			gitCmd(main, "commit", "--allow-empty", "-m", "base")
			linked := filepath.Join(t.TempDir(), "linked")
			gitCmd(main, "worktree", "add", "-b", "linked", linked)
			gitCmd(main, "config", "extensions.worktreeConfig", "true")
			gitCmd(linked, "config", "--worktree", "user.name", "Linked Submitter")
			gitCmd(linked, "config", "--worktree", "user.email", "linked@example.test")
			t.Chdir(linked)
			sock := testSocketPath(t)
			srv := startTestIPCServer(t, sock)
			for _, method := range []string{ipc.MethodProbeTranscriptHomes, ipc.MethodProbeCommitIdentity} {
				srv.Handle(method, func(context.Context, json.RawMessage) (interface{}, error) {
					return map[string]bool{"ok": true}, nil
				})
			}
			requests := make(chan ipc.RerunParams, 1)
			srv.Handle(ipc.MethodRerun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.RerunParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				requests <- params
				return &ipc.RerunResult{RunID: "rerun"}, nil
			})
			srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetRunResult{Run: testRun()}, nil
			})
			client, err := ipc.Dial(sock)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			run := testRun()
			run.Status = status
			model := NewModel(sock, client, run)
			msg := model.rerunCmd(1)()
			if _, ok := msg.(rerunStartedMsg); !ok {
				t.Fatalf("rerun failed: %#v", msg)
			}
			got := <-requests
			if got.CommitIdentity == nil || got.CommitIdentity.AuthorName != "Linked Submitter" || got.CommitIdentity.CommitterEmail != "linked@example.test" {
				t.Fatalf("linked identity lost: %+v", got.CommitIdentity)
			}
		})
	}
}

func TestModel_RerunRefusesMissingIdentityOrOlderDaemon(t *testing.T) {
	for _, reason := range []string{"older daemon", "missing identity", "unrelated checkout"} {
		t.Run(reason, func(t *testing.T) {
			dir := tuiIdentityCheckout(t)
			t.Chdir(dir)
			if reason == "missing identity" {
				if err := os.WriteFile(filepath.Join(dir, "global"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sock := testSocketPath(t)
			srv := startTestIPCServer(t, sock)
			srv.Handle(ipc.MethodProbeTranscriptHomes, func(context.Context, json.RawMessage) (interface{}, error) {
				return map[string]bool{"ok": true}, nil
			})
			if reason != "older daemon" {
				srv.Handle(ipc.MethodProbeCommitIdentity, func(context.Context, json.RawMessage) (interface{}, error) {
					return map[string]bool{"ok": true}, nil
				})
			}
			requests := make(chan struct{}, 1)
			srv.Handle(ipc.MethodRerun, func(context.Context, json.RawMessage) (interface{}, error) {
				requests <- struct{}{}
				return &ipc.RerunResult{}, nil
			})
			client, err := ipc.Dial(sock)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			run := testRun()
			run.Status = types.RunFailed
			if reason == "unrelated checkout" {
				run.RepoID = "another-repository"
			}
			model := NewModel(sock, client, run)
			msg := model.rerunCmd(9)()
			if _, ok := msg.(rerunErrMsg); !ok {
				t.Fatalf("invalid identity reached rerun: %#v", msg)
			}
			select {
			case <-requests:
				t.Fatal("refused identity was submitted")
			default:
			}
		})
	}
}
