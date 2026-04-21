// Package wizard provides the pre-pipeline onboarding flow: it detects repo
// state, optionally creates a branch, commits uncommitted changes, and pushes
// to the no-mistakes gate, then hands off to the main TUI. It supports both
// the interactive wizard UI and the non-interactive auto-accept path.
package wizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// Config describes the repo state and dependencies the wizard needs.
// Callers pre-detect git state so the wizard can decide up-front which
// steps to skip.
type Config struct {
	Context       context.Context
	RepoDir       string
	CurrentBranch string
	DefaultBranch string
	// AutoAdvance automatically presses Enter on each active wizard step,
	// preserving the interactive TUI while accepting the default path.
	AutoAdvance  bool
	DisableInput bool
	// NeedsBranch is true when the user has no usable feature branch yet -
	// either they're on the default branch, or HEAD is detached. The branch
	// step is only active when this is true.
	NeedsBranch bool
	IsDirty     bool
	GateRemote  string
	Output      io.Writer

	CreateBranch  func(ctx context.Context, name string) error
	CommitAll     func(ctx context.Context, msg string) error
	Push          func(ctx context.Context, branch string) error
	SuggestBranch func(ctx context.Context) (string, error)
	SuggestCommit func(ctx context.Context) (string, error)
	Track         func(action string, fields map[string]any)
}

// stepID identifies one of the three wizard steps.
type stepID int

const (
	stepBranch stepID = iota
	stepCommit
	stepPush
)

// stepStatus is the visual + logical state of a single step.
type stepStatus int

const (
	statPending stepStatus = iota
	statInput              // awaiting text input from user (branch / commit)
	statAgent              // agent generating a suggestion
	statConfirm            // awaiting y/n (push)
	statRunning            // git action in flight
	statDone
	statSkipped
	statFailed
)

type step struct {
	id         stepID
	status     stepStatus
	result     string // displayed when done
	skipReason string // displayed when skipped
	errMsg     string
	source     string
}

// Model is the bubbletea model for the wizard.
type Model struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	steps  []*step
	active int // index of the currently active step, or len(steps) when finished

	input textinput.Model

	targetBranch string // the branch we intend to end up on / push

	// Tracks side-effects for abort-copy and for the final Result.
	branchCreated bool
	commitMade    bool
	pushed        bool

	width, height int

	spinnerFrame int
	spinnerAlive bool

	confirmQuit bool // true after first q press while side-effects exist

	err      error
	quitting bool
	success  bool // all needed steps completed; pipeline pushed
	aborted  bool // user explicitly aborted
}

// Result reports what the wizard did. Success means a push to the gate
// succeeded and the caller should re-attach to pick up the new run.
type Result struct {
	Success       bool
	Aborted       bool
	BranchCreated bool
	CommitMade    bool
	Pushed        bool
	TargetBranch  string
	Err           error
}

// NewModel constructs a wizard Model. Which steps end up active depends on
// the supplied Config: if the current branch already differs from the default,
// the branch step is skipped; if the working tree is clean, the commit step
// is skipped; the push step always runs.
func NewModel(cfg Config) Model {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.CharLimit = 80
	baseCtx := cfg.Context
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)

	m := Model{
		cfg:          cfg,
		ctx:          ctx,
		cancel:       cancel,
		input:        ti,
		targetBranch: cfg.CurrentBranch,
	}

	branch := &step{id: stepBranch, status: statPending}
	commit := &step{id: stepCommit, status: statPending}
	push := &step{id: stepPush, status: statPending}

	if !cfg.NeedsBranch {
		branch.status = statSkipped
		branch.skipReason = "already on " + cfg.CurrentBranch
	}
	if !cfg.IsDirty {
		commit.status = statSkipped
		commit.skipReason = "no uncommitted changes"
	}
	m.steps = []*step{branch, commit, push}
	m.active = m.firstPending()
	m = m.setupActive()
	return m
}

// Init returns the initial Cmd for the active step. State was already
// prepared in NewModel.
func (m Model) Init() tea.Cmd {
	return m.afterEnterCmd()
}

// Update handles bubbletea events and drives the state machine.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case suggestionMsg:
		return m.handleSuggestion(msg)

	case actionMsg:
		return m.handleAction(msg)

	case spinnerTickMsg:
		m.spinnerAlive = false
		if !m.anySpinnerActive() {
			return m, nil
		}
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		return m, m.scheduleSpinner()

	case autoAdvanceMsg:
		return m.handleAutoAdvance()
	}
	return m, nil
}

// Result returns the terminal state of the wizard. Only meaningful once the
// program has exited.
func (m Model) Result() Result {
	return Result{
		Success:       m.success,
		Aborted:       m.aborted,
		BranchCreated: m.branchCreated,
		CommitMade:    m.commitMade,
		Pushed:        m.pushed,
		TargetBranch:  m.targetBranch,
		Err:           m.err,
	}
}

// firstPending returns the index of the first step that still needs work.
// Returns len(steps) if none are pending.
func (m Model) firstPending() int {
	for i, s := range m.steps {
		if s.status == statPending {
			return i
		}
	}
	return len(m.steps)
}

// activeStep returns the step currently being worked on, or nil if the
// wizard has finished.
func (m *Model) activeStep() *step {
	if m.active < 0 || m.active >= len(m.steps) {
		return nil
	}
	return m.steps[m.active]
}

// setupActive transitions the active step into its initial interactive
// state (input for branch/commit, confirm for push). When no step is
// pending, it marks the wizard as finished.
func (m Model) setupActive() Model {
	s := m.activeStep()
	if s == nil {
		m.success = m.pushed
		if m.success {
			m.track("completed", map[string]any{
				"branch_created": m.branchCreated,
				"commit_made":    m.commitMade,
				"pushed":         m.pushed,
			})
		}
		m.quitting = true
		m.cancel()
		return m
	}
	switch s.id {
	case stepBranch, stepCommit:
		s.status = statInput
		m.input.SetValue("")
		m.input.Focus()
		m.input.Placeholder = "blank = let agent suggest"
	case stepPush:
		s.status = statConfirm
		m.input.Blur()
	}
	return m
}

// afterEnterCmd returns the Cmd to kick off after transitioning to a new step.
func (m Model) afterEnterCmd() tea.Cmd {
	if m.quitting {
		return quitWithTitleReset()
	}
	s := m.activeStep()
	if s != nil && m.cfg.AutoAdvance && (s.status == statInput || s.status == statConfirm) {
		return autoAdvanceCmd()
	}
	if s != nil && s.status == statInput {
		return textinput.Blink
	}
	return nil
}

func (m Model) handleAutoAdvance() (tea.Model, tea.Cmd) {
	s := m.activeStep()
	if s == nil {
		return m, nil
	}
	if s.status != statInput && s.status != statConfirm {
		return m, nil
	}
	if s.status == statConfirm {
		s.source = "auto"
		return m.executeStep(s, "")
	}
	return m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.activeStep()
	if s == nil {
		// Already finished; any key quits.
		m.quitting = true
		m.cancel()
		return m, quitWithTitleReset()
	}

	switch msg.String() {
	case "ctrl+c":
		m.trackAbort("interrupt")
		m.aborted = true
		m.quitting = true
		m.cancel()
		return m, quitWithTitleReset()
	case "q":
		if m.hasSideEffects() && !m.confirmQuit {
			m.confirmQuit = true
			return m, nil
		}
		m.trackAbort("quit")
		m.aborted = true
		m.quitting = true
		m.cancel()
		return m, quitWithTitleReset()
	}

	// Any non-q key clears the abort confirmation.
	m.confirmQuit = false

	switch s.status {
	case statInput:
		return m.handleInputKey(msg)
	case statConfirm:
		return m.handleConfirmKey(msg)
	case statFailed:
		return m.handleFailedKey(msg)
	}
	return m, nil
}

func (m Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.activeStep()
	switch msg.String() {
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if value == "" {
			// Agent suggestion path.
			s.status = statAgent
			s.source = "agent"
			return m, tea.Batch(m.suggestCmd(s.id), m.scheduleSpinner())
		}
		s.source = "user"
		return m.executeStep(s, value)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.activeStep()
	switch msg.String() {
	case "y", "Y", "enter":
		s.source = "user"
		return m.executeStep(s, "")
	case "n", "N":
		m.trackAbort("decline_push")
		m.aborted = true
		m.quitting = true
		m.cancel()
		return m, quitWithTitleReset()
	}
	return m, nil
}

func (m Model) handleFailedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "r":
		s := m.activeStep()
		s.status = statPending
		s.errMsg = ""
		m = m.setupActive()
		return m, m.afterEnterCmd()
	}
	return m, nil
}

// executeStep runs the git action for the step. For branch/commit, value is
// the user-supplied or agent-generated string. For push, value is ignored.
func (m *Model) executeStep(s *step, value string) (tea.Model, tea.Cmd) {
	s.status = statRunning
	s.result = value
	m.input.Blur()
	switch s.id {
	case stepBranch:
		return *m, tea.Batch(m.runCreateBranch(value), m.scheduleSpinner())
	case stepCommit:
		return *m, tea.Batch(m.runCommit(value), m.scheduleSpinner())
	case stepPush:
		return *m, tea.Batch(m.runPush(), m.scheduleSpinner())
	}
	return *m, nil
}

func (m Model) handleSuggestion(msg suggestionMsg) (tea.Model, tea.Cmd) {
	if m.active >= len(m.steps) || m.steps[m.active].id != msg.id {
		return m, nil
	}
	s := m.steps[m.active]
	if msg.err != nil {
		wrappedErr := fmt.Errorf("suggest %s: %w", stepName(msg.id), msg.err)
		if m.cfg.AutoAdvance {
			s.status = statFailed
			s.errMsg = wrappedErr.Error()
			m.err = wrappedErr
			m.quitting = true
			m.cancel()
			return m, quitWithTitleReset()
		}
		// Fall back to asking the user to type.
		s.status = statInput
		s.source = ""
		m.input.SetValue("")
		m.input.Placeholder = "agent unavailable: " + truncate(msg.err.Error(), 40)
		m.input.Focus()
		return m, textinput.Blink
	}
	return m.executeStep(s, msg.value)
}

func (m Model) handleAction(msg actionMsg) (tea.Model, tea.Cmd) {
	if m.active >= len(m.steps) || m.steps[m.active].id != msg.id {
		return m, nil
	}
	s := m.steps[m.active]
	if msg.err != nil {
		wrappedErr := fmt.Errorf("%s: %w", stepActionLabel(msg.id), msg.err)
		s.status = statFailed
		s.errMsg = wrappedErr.Error()
		if m.cfg.AutoAdvance {
			m.err = wrappedErr
			m.quitting = true
			m.cancel()
			return m, quitWithTitleReset()
		}
		return m, nil
	}
	switch s.id {
	case stepBranch:
		m.branchCreated = true
		m.targetBranch = s.result
		m.track("branch_created", map[string]any{"step": stepName(s.id), "source": stepSource(s.source)})
	case stepCommit:
		m.commitMade = true
		m.track("committed", map[string]any{"step": stepName(s.id), "source": stepSource(s.source)})
	case stepPush:
		m.pushed = true
		m.track("pushed", map[string]any{"step": stepName(s.id), "source": stepSource(s.source)})
	}
	s.status = statDone
	m.active = m.firstPending()
	m = m.setupActive()
	return m, m.afterEnterCmd()
}

func (m Model) hasSideEffects() bool {
	return m.branchCreated || m.commitMade
}

// terminalTitle returns the terminal title string reflecting wizard state.
// Format mirrors internal/tui: "<icon> Setup <step> - <branch>".
func (m Model) terminalTitle() string {
	branch := m.targetBranch
	if branch == "" {
		branch = m.cfg.CurrentBranch
	}
	suffix := ""
	if branch != "" {
		suffix = " - " + branch
	}

	switch {
	case m.success:
		return "✓ Setup complete" + suffix
	case m.aborted:
		return "✗ Setup aborted" + suffix
	}

	for _, s := range m.steps {
		if s.status == statFailed {
			return "✗ Setup " + stepLabel(s.id) + suffix
		}
	}

	s := m.activeStep()
	if s == nil {
		return "○ Setup" + suffix
	}
	icon := stepTitleIcon(s.status, m.spinnerFrame)
	return icon + " Setup " + stepLabel(s.id) + suffix
}

// stepTitleIcon returns a plain-text icon suitable for use in the terminal
// title bar. It mirrors the visual icons in stepIconAndStyle but without
// lipgloss styling, since terminal titles can't render ANSI.
func stepTitleIcon(status stepStatus, spinnerFrame int) string {
	switch status {
	case statDone:
		return "✓"
	case statSkipped:
		return "–"
	case statFailed:
		return "✗"
	case statAgent, statRunning:
		if len(spinnerFrames) == 0 {
			return "◉"
		}
		return spinnerFrames[spinnerFrame%len(spinnerFrames)]
	case statInput, statConfirm:
		return "⏸"
	}
	return "○"
}

// setTerminalTitle returns the OSC escape sequence to set the terminal title.
func setTerminalTitle(title string) string {
	return "\x1b]2;" + title + "\x07"
}

// quitWithTitleReset clears the terminal title via bubbletea, then quits.
func quitWithTitleReset() tea.Cmd {
	return tea.Sequence(tea.SetWindowTitle(""), tea.Quit)
}

func resetTerminalTitle(output io.Writer) {
	if output == nil {
		output = os.Stdout
	}
	_, _ = io.WriteString(output, setTerminalTitle(""))
}

func (m Model) track(action string, fields map[string]any) {
	if m.cfg.Track == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	m.cfg.Track(action, fields)
}

func (m Model) trackAbort(reason string) {
	fields := map[string]any{"reason": reason}
	if s := m.activeStep(); s != nil {
		fields["step"] = stepName(s.id)
	}
	m.track("aborted", fields)
}

func (m Model) anySpinnerActive() bool {
	s := m.activeStep()
	if s == nil {
		return false
	}
	return s.status == statAgent || s.status == statRunning
}

// Commands.

type suggestionMsg struct {
	id    stepID
	value string
	err   error
}

type actionMsg struct {
	id  stepID
	err error
}

type spinnerTickMsg struct{}

type autoAdvanceMsg struct{}

const spinnerInterval = 120 * time.Millisecond

func autoAdvanceCmd() tea.Cmd {
	return func() tea.Msg { return autoAdvanceMsg{} }
}

func (m *Model) scheduleSpinner() tea.Cmd {
	if m.spinnerAlive {
		return nil
	}
	m.spinnerAlive = true
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

func (m Model) suggestCmd(id stepID) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		switch id {
		case stepBranch:
			if m.cfg.SuggestBranch == nil {
				return suggestionMsg{id: id, err: errors.New("no branch suggester configured")}
			}
			v, err := m.cfg.SuggestBranch(ctx)
			return suggestionMsg{id: id, value: v, err: err}
		case stepCommit:
			if m.cfg.SuggestCommit == nil {
				return suggestionMsg{id: id, err: errors.New("no commit suggester configured")}
			}
			v, err := m.cfg.SuggestCommit(ctx)
			return suggestionMsg{id: id, value: v, err: err}
		}
		return suggestionMsg{id: id, err: errors.New("unknown step")}
	}
}

func (m Model) runCreateBranch(name string) tea.Cmd {
	return func() tea.Msg {
		err := m.cfg.CreateBranch(m.ctx, name)
		return actionMsg{id: stepBranch, err: err}
	}
}

func (m Model) runCommit(msg string) tea.Cmd {
	return func() tea.Msg {
		err := m.cfg.CommitAll(m.ctx, msg)
		return actionMsg{id: stepCommit, err: err}
	}
}

func (m Model) runPush() tea.Cmd {
	branch := m.targetBranch
	return func() tea.Msg {
		err := m.cfg.Push(m.ctx, branch)
		return actionMsg{id: stepPush, err: err}
	}
}

// Run invokes the wizard as an interactive bubbletea program. Returns the
// terminal Result describing what happened.
func Run(cfg Config) (Result, error) {
	baseCtx := cfg.Context
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if err := baseCtx.Err(); err != nil {
		return Result{Err: err}, err
	}
	m := NewModel(cfg)
	options := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(baseCtx)}
	if cfg.DisableInput {
		options = append(options, tea.WithInput(nil))
	}
	if cfg.Output != nil {
		options = append(options, tea.WithOutput(cfg.Output))
	}
	defer resetTerminalTitle(cfg.Output)
	p := tea.NewProgram(m, options...)
	final, err := p.Run()
	if err != nil {
		return Result{Err: err}, err
	}
	fm, ok := final.(Model)
	if !ok {
		return Result{}, errors.New("wizard: unexpected terminal model type")
	}
	return fm.Result(), nil
}

// RunAuto executes the wizard steps non-interactively. It accepts the default
// automated path for each step: use agent suggestions for branch and commit,
// then push to the gate. Suggestion failures are returned immediately.
func RunAuto(cfg Config) (Result, error) {
	res := Result{TargetBranch: cfg.CurrentBranch}
	baseCtx := cfg.Context
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	track := func(action string, fields map[string]any) {
		if cfg.Track == nil {
			return
		}
		if fields == nil {
			fields = map[string]any{}
		}
		cfg.Track(action, fields)
	}

	if cfg.NeedsBranch {
		if cfg.SuggestBranch == nil {
			err := errors.New("no branch suggester configured")
			res.Err = err
			return res, err
		}
		suggestCtx, suggestCancel := context.WithTimeout(ctx, 60*time.Second)
		branch, err := cfg.SuggestBranch(suggestCtx)
		suggestCancel()
		if err != nil {
			err = fmt.Errorf("suggest branch: %w", err)
			res.Err = err
			return res, err
		}
		if cfg.CreateBranch == nil {
			err = errors.New("no branch creator configured")
			res.Err = err
			return res, err
		}
		if err := cfg.CreateBranch(ctx, branch); err != nil {
			err = fmt.Errorf("create branch: %w", err)
			res.Err = err
			return res, err
		}
		res.BranchCreated = true
		res.TargetBranch = branch
		track("branch_created", map[string]any{"step": stepName(stepBranch), "source": "agent"})
	}

	if cfg.IsDirty {
		if cfg.SuggestCommit == nil {
			err := errors.New("no commit suggester configured")
			res.Err = err
			return res, err
		}
		suggestCtx, suggestCancel := context.WithTimeout(ctx, 60*time.Second)
		commitMsg, err := cfg.SuggestCommit(suggestCtx)
		suggestCancel()
		if err != nil {
			err = fmt.Errorf("suggest commit: %w", err)
			res.Err = err
			return res, err
		}
		if cfg.CommitAll == nil {
			err = errors.New("no commit action configured")
			res.Err = err
			return res, err
		}
		if err := cfg.CommitAll(ctx, commitMsg); err != nil {
			err = fmt.Errorf("commit changes: %w", err)
			res.Err = err
			return res, err
		}
		res.CommitMade = true
		track("committed", map[string]any{"step": stepName(stepCommit), "source": "agent"})
	}

	if cfg.Push == nil {
		err := errors.New("no push action configured")
		res.Err = err
		return res, err
	}
	if err := cfg.Push(ctx, res.TargetBranch); err != nil {
		err = fmt.Errorf("push branch: %w", err)
		res.Err = err
		return res, err
	}
	res.Pushed = true
	res.Success = true
	track("pushed", map[string]any{"step": stepName(stepPush), "source": "auto"})
	track("completed", map[string]any{
		"branch_created": res.BranchCreated,
		"commit_made":    res.CommitMade,
		"pushed":         res.Pushed,
	})
	return res, nil
}

func stepName(id stepID) string {
	switch id {
	case stepBranch:
		return "branch"
	case stepCommit:
		return "commit"
	case stepPush:
		return "push"
	default:
		return "unknown"
	}
}

func stepActionLabel(id stepID) string {
	switch id {
	case stepBranch:
		return "create branch"
	case stepCommit:
		return "commit changes"
	case stepPush:
		return "push branch"
	default:
		return stepName(id)
	}
}

func stepSource(source string) string {
	if source == "" {
		return "user"
	}
	return source
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}
