package wizard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// recording stubs capture calls for assertions.
type recorder struct {
	createdBranch string
	commitMsg     string
	pushedBranch  string

	createBranchErr error
	commitErr       error
	pushErr         error

	suggestBranch    string
	suggestCommit    string
	suggestBranchErr error
	suggestCommitErr error
}

func (r *recorder) deps() Config {
	return Config{
		CreateBranch: func(_ context.Context, name string) error {
			r.createdBranch = name
			return r.createBranchErr
		},
		CommitAll: func(_ context.Context, msg string) error {
			r.commitMsg = msg
			return r.commitErr
		},
		Push: func(_ context.Context, branch string) error {
			r.pushedBranch = branch
			return r.pushErr
		},
		SuggestBranch: func(_ context.Context) (string, error) {
			return r.suggestBranch, r.suggestBranchErr
		},
		SuggestCommit: func(_ context.Context) (string, error) {
			return r.suggestCommit, r.suggestCommitErr
		},
	}
}

func baseConfig(r *recorder) Config {
	cfg := r.deps()
	cfg.RepoDir = "/tmp/repo"
	cfg.CurrentBranch = "main"
	cfg.DefaultBranch = "main"
	cfg.NeedsBranch = true
	cfg.IsDirty = true
	cfg.GateRemote = "no-mistakes"
	return cfg
}

// step the model through a command synchronously by running the returned
// Cmd and feeding the result back into Update.
func drain(m Model, cmd tea.Cmd) Model {
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return m
		}
		// Skip timer ticks - they'd spin forever in tests.
		if _, ok := msg.(spinnerTickMsg); ok {
			return m
		}
		// Skip batched commands from bubbles/textinput (e.g. Blink).
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				m = drain(m, c)
			}
			return m
		}
		next, nextCmd := m.Update(msg)
		m = next.(Model)
		cmd = nextCmd
	}
	return m
}

func advance(m Model, msg tea.Msg) Model {
	next, cmd := m.Update(msg)
	return drain(next.(Model), cmd)
}

func TestNewModel_AllStepsPending(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	if m.steps[0].status != statInput {
		t.Errorf("active branch step should be in input mode, got %v", m.steps[0].status)
	}
	if m.steps[1].status != statPending {
		t.Errorf("commit should be pending when dirty, got %v", m.steps[1].status)
	}
	if m.steps[2].status != statPending {
		t.Errorf("push should be pending initially, got %v", m.steps[2].status)
	}
	if m.active != 0 {
		t.Errorf("first active step should be branch (0), got %d", m.active)
	}
}

func TestNewModel_SkipsBranchOnFeatureBranch(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/existing"
	cfg.NeedsBranch = false
	m := NewModel(cfg)
	if m.steps[0].status != statSkipped {
		t.Fatalf("expected branch skipped, got %v", m.steps[0].status)
	}
	if !strings.Contains(m.steps[0].skipReason, "feat/existing") {
		t.Fatalf("skip reason should mention branch, got %q", m.steps[0].skipReason)
	}
	if m.active != 1 {
		t.Fatalf("first active should be commit, got %d", m.active)
	}
}

func TestNewModel_SkipsCommitWhenClean(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.IsDirty = false
	m := NewModel(cfg)
	if m.steps[1].status != statSkipped {
		t.Fatalf("expected commit skipped, got %v", m.steps[1].status)
	}
}

func TestNewModel_SkipsBothWhenOnFeatureAndClean(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x"
	cfg.NeedsBranch = false
	cfg.IsDirty = false
	m := NewModel(cfg)
	if m.active != 2 {
		t.Fatalf("expected push to be first active, got %d", m.active)
	}
}

func TestNewModel_DetachedHEADForcesBranchStep(t *testing.T) {
	// Simulates detached HEAD: CurrentBranch literally "HEAD" but caller
	// set NeedsBranch=true to force the branch step to run.
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "HEAD"
	cfg.DefaultBranch = "main"
	cfg.NeedsBranch = true
	cfg.IsDirty = false
	m := NewModel(cfg)

	if m.steps[0].status == statSkipped {
		t.Fatal("branch step should NOT be skipped in detached HEAD")
	}
	if m.active != 0 {
		t.Fatalf("expected branch to be first active, got %d", m.active)
	}
}

func TestBranchStep_UserTyped(t *testing.T) {
	r := &recorder{}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())

	// Type a name.
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("feat/wizard")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})

	// After running the create-branch cmd, handleAction advances.
	if r.createdBranch != "feat/wizard" {
		t.Fatalf("expected CreateBranch called with feat/wizard, got %q", r.createdBranch)
	}
	if m.steps[0].status != statDone {
		t.Fatalf("expected branch done, got %v", m.steps[0].status)
	}
	if !m.branchCreated {
		t.Fatal("expected branchCreated = true")
	}
	if m.targetBranch != "feat/wizard" {
		t.Fatalf("expected targetBranch feat/wizard, got %q", m.targetBranch)
	}
	if m.active != 1 {
		t.Fatalf("expected active to be commit, got %d", m.active)
	}
}

func TestBranchStep_BlankUsesAgent(t *testing.T) {
	r := &recorder{suggestBranch: "fix/agent-name"}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())

	// Press enter with blank input.
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})

	if r.createdBranch != "fix/agent-name" {
		t.Fatalf("expected agent suggestion used, got %q", r.createdBranch)
	}
	if m.targetBranch != "fix/agent-name" {
		t.Fatalf("targetBranch should be agent suggestion, got %q", m.targetBranch)
	}
}

func TestBranchStep_SuggestionFailureFallsBackToInput(t *testing.T) {
	r := &recorder{suggestBranchErr: errors.New("agent down")}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())

	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.steps[0].status != statInput {
		t.Fatalf("expected fallback to input, got %v", m.steps[0].status)
	}
	if !strings.Contains(m.input.Placeholder, "agent unavailable") {
		t.Fatalf("expected placeholder to mention agent failure, got %q", m.input.Placeholder)
	}
	if r.createdBranch != "" {
		t.Fatalf("should not have called CreateBranch, got %q", r.createdBranch)
	}
}

func TestCommitStep_UserTyped(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x" // skip branch
	cfg.NeedsBranch = false
	r := &recorder{}
	cfg2 := cfg
	cfg2.CreateBranch = r.deps().CreateBranch
	cfg2.CommitAll = r.deps().CommitAll
	cfg2.Push = r.deps().Push
	cfg2.SuggestBranch = r.deps().SuggestBranch
	cfg2.SuggestCommit = r.deps().SuggestCommit
	m := NewModel(cfg2)
	m = drain(m, m.Init())

	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("feat: add x")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})

	if r.commitMsg != "feat: add x" {
		t.Fatalf("expected commit message, got %q", r.commitMsg)
	}
	if !m.commitMade {
		t.Fatal("expected commitMade = true")
	}
	if m.steps[1].status != statDone {
		t.Fatalf("expected commit done, got %v", m.steps[1].status)
	}
	if m.active != 2 {
		t.Fatalf("expected push active, got %d", m.active)
	}
}

func TestPushStep_Confirm(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x" // skip branch
	cfg.NeedsBranch = false
	cfg.IsDirty = false // skip commit
	r := &recorder{}
	cfg.CreateBranch = r.deps().CreateBranch
	cfg.CommitAll = r.deps().CommitAll
	cfg.Push = r.deps().Push
	cfg.SuggestBranch = r.deps().SuggestBranch
	cfg.SuggestCommit = r.deps().SuggestCommit
	m := NewModel(cfg)
	m = drain(m, m.Init())

	if m.active != 2 {
		t.Fatalf("active should be push, got %d", m.active)
	}
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})

	if r.pushedBranch != "feat/x" {
		t.Fatalf("expected Push for feat/x, got %q", r.pushedBranch)
	}
	if !m.pushed {
		t.Fatal("expected pushed = true")
	}
	if !m.quitting {
		t.Fatal("wizard should quit after final step")
	}
	if !m.success {
		t.Fatal("wizard should report success")
	}
}

func TestPushStep_DeclineAborts(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x"
	cfg.NeedsBranch = false
	cfg.IsDirty = false
	r := &recorder{}
	cfg.CreateBranch = r.deps().CreateBranch
	cfg.CommitAll = r.deps().CommitAll
	cfg.Push = r.deps().Push
	cfg.SuggestBranch = r.deps().SuggestBranch
	cfg.SuggestCommit = r.deps().SuggestCommit
	m := NewModel(cfg)
	m = drain(m, m.Init())

	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})

	if r.pushedBranch != "" {
		t.Fatalf("Push should not have run, got %q", r.pushedBranch)
	}
	if !m.aborted {
		t.Fatal("expected aborted = true")
	}
	if m.success {
		t.Fatal("declining push should not be success")
	}
}

func TestActionFailure(t *testing.T) {
	r := &recorder{createBranchErr: errors.New("branch exists")}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dup")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.steps[0].status != statFailed {
		t.Fatalf("expected failed, got %v", m.steps[0].status)
	}
	if m.steps[0].errMsg == "" {
		t.Fatal("expected error message")
	}
}

func TestRetryAfterFailure(t *testing.T) {
	r := &recorder{createBranchErr: errors.New("branch exists")}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dup")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.steps[0].status != statFailed {
		t.Fatalf("precondition: expected failed, got %v", m.steps[0].status)
	}

	// Clear the error and press r to retry.
	r.createBranchErr = nil
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.steps[0].status != statInput {
		t.Fatalf("expected retry to re-enter input, got %v", m.steps[0].status)
	}

	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("fresh")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.steps[0].status != statDone {
		t.Fatalf("expected retry to succeed, got %v", m.steps[0].status)
	}
}

func TestQuit_NoSideEffects_SinglePress(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m = drain(m, m.Init())
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !m.aborted || !m.quitting {
		t.Fatal("single q with no side-effects should quit immediately")
	}
}

func TestQuit_WithSideEffects_RequiresConfirm(t *testing.T) {
	r := &recorder{}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())
	// Complete branch step (side-effect).
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("feat/x")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.branchCreated {
		t.Fatal("precondition: branch should be created")
	}

	// First q sets confirm, does not quit.
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.quitting {
		t.Fatal("first q should not quit when there are side-effects")
	}
	if !m.confirmQuit {
		t.Fatal("first q should set confirmQuit")
	}

	// Second q actually quits.
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !m.aborted || !m.quitting {
		t.Fatal("second q should abort")
	}
}

func TestConfirmQuit_ResetsOnOtherKey(t *testing.T) {
	r := &recorder{}
	m := NewModel(baseConfig(r))
	m = drain(m, m.Init())
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("feat/x")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})

	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !m.confirmQuit {
		t.Fatal("expected confirmQuit set")
	}
	// Any other key resets.
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.confirmQuit {
		t.Fatal("non-q key should clear confirmQuit")
	}
}

func TestQuit_CancelsInFlightSuggestion(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	done := make(chan tea.Msg, 1)

	cfg := baseConfig(&recorder{})
	cfg.SuggestBranch = func(ctx context.Context) (string, error) {
		ctxCh <- ctx
		<-ctx.Done()
		return "", ctx.Err()
	}

	m := NewModel(cfg)
	cmd := m.suggestCmd(stepBranch)
	go func() {
		done <- cmd()
	}()

	var suggestCtx context.Context
	select {
	case suggestCtx = <-ctxCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for suggestion to start")
	}

	if err := suggestCtx.Err(); err != nil {
		t.Fatalf("suggestion context should start active, got %v", err)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = next.(Model)

	if !m.aborted || !m.quitting {
		t.Fatal("quit should abort the wizard")
	}

	select {
	case msg := <-done:
		suggestion, ok := msg.(suggestionMsg)
		if !ok {
			t.Fatalf("expected suggestionMsg, got %T", msg)
		}
		if !errors.Is(suggestion.err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", suggestion.err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for suggestion cancellation")
	}
}

func TestQuit_CancelsInFlightGitActions(t *testing.T) {
	tests := []struct {
		name    string
		run     func(Model, string) tea.Cmd
		wantMsg stepID
	}{
		{name: "create branch", run: func(m Model, value string) tea.Cmd { return m.runCreateBranch(value) }, wantMsg: stepBranch},
		{name: "commit", run: func(m Model, value string) tea.Cmd { return m.runCommit(value) }, wantMsg: stepCommit},
		{name: "push", run: func(m Model, value string) tea.Cmd { return m.runPush() }, wantMsg: stepPush},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctxCh := make(chan context.Context, 1)
			done := make(chan tea.Msg, 1)

			cfg := baseConfig(&recorder{})
			cfg.CurrentBranch = "feat/x"
			cfg.NeedsBranch = false
			cfg.IsDirty = false
			cfg.CreateBranch = func(ctx context.Context, _ string) error {
				ctxCh <- ctx
				<-ctx.Done()
				return ctx.Err()
			}
			cfg.CommitAll = func(ctx context.Context, _ string) error {
				ctxCh <- ctx
				<-ctx.Done()
				return ctx.Err()
			}
			cfg.Push = func(ctx context.Context, _ string) error {
				ctxCh <- ctx
				<-ctx.Done()
				return ctx.Err()
			}

			m := NewModel(cfg)
			cmd := tc.run(m, "value")
			go func() {
				done <- cmd()
			}()

			var actionCtx context.Context
			select {
			case actionCtx = <-ctxCh:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for action to start")
			}

			if err := actionCtx.Err(); err != nil {
				t.Fatalf("action context should start active, got %v", err)
			}

			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
			m = next.(Model)

			if !m.aborted || !m.quitting {
				t.Fatal("quit should abort the wizard")
			}

			select {
			case msg := <-done:
				action, ok := msg.(actionMsg)
				if !ok {
					t.Fatalf("expected actionMsg, got %T", msg)
				}
				if action.id != tc.wantMsg {
					t.Fatalf("expected action id %v, got %v", tc.wantMsg, action.id)
				}
				if !errors.Is(action.err, context.Canceled) {
					t.Fatalf("expected context canceled, got %v", action.err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for action cancellation")
			}
		})
	}
}

func TestCtrlCAborts(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m = drain(m, m.Init())
	m = advance(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.aborted || !m.quitting {
		t.Fatal("ctrl+c should abort regardless of side-effects")
	}
}

func TestResult(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x"
	cfg.NeedsBranch = false
	cfg.IsDirty = false
	r := &recorder{}
	cfg.CreateBranch = r.deps().CreateBranch
	cfg.CommitAll = r.deps().CommitAll
	cfg.Push = r.deps().Push
	cfg.SuggestBranch = r.deps().SuggestBranch
	cfg.SuggestCommit = r.deps().SuggestCommit
	m := NewModel(cfg)
	m = drain(m, m.Init())
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})

	res := m.Result()
	if !res.Success {
		t.Fatal("expected Success")
	}
	if !res.Pushed {
		t.Fatal("expected Pushed")
	}
	if res.TargetBranch != "feat/x" {
		t.Fatalf("expected TargetBranch feat/x, got %q", res.TargetBranch)
	}
}
