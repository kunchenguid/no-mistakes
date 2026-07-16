package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/supervision"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

const supervisionHeartbeat = 5 * time.Minute

var supervisionNow = time.Now

// codexHookEvent is the stable subset of the official Codex command-hook
// payload needed to bind an explicitly armed run to the session that ended.
// stop_hook_active is intentionally context only: it is not a blanket veto.
type codexHookEvent struct {
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// claudeHookEvent is the stable subset of the official Claude Code Stop-hook
// payload. Claude does not expose a turn id, so a bounded opaque digest of the
// completed assistant message is used only for local duplicate suppression.
// Neither message nor digest is ever returned through the hook channel.
type claudeHookEvent struct {
	SessionID            string `json:"session_id"`
	CWD                  string `json:"cwd"`
	HookEventName        string `json:"hook_event_name"`
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// supervisorHookEvent is the provider-neutral, privacy-bounded event shape
// needed after provider-specific payload validation has completed.
type supervisorHookEvent struct {
	SessionID string
	HandoffID string
	CWD       string
}

type supervisorOutcome string

const (
	supervisorNone          supervisorOutcome = ""
	supervisorAskUser       supervisorOutcome = "ask_user"
	supervisorTechnicalGate supervisorOutcome = "technical_gate"
	supervisorChecksPassed  supervisorOutcome = "checks_passed"
	supervisorTerminal      supervisorOutcome = "terminal"
	supervisorHeartbeat     supervisorOutcome = "heartbeat"
	supervisorStale         supervisorOutcome = "stale"
	supervisorWatchFault    supervisorOutcome = "watch_fault"
)

func newAxiSuperviseCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "supervise", Short: "Opt-in Codex CLI supervision for one AXI run", SilenceErrors: true, SilenceUsage: true}
	cmd.AddCommand(newAxiSuperviseArmCmd())
	cmd.AddCommand(newAxiSuperviseStatusCmd())
	return cmd
}

func newAxiSuperviseStatusCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{Use: "status", Short: "Show the local supervisor state for one run", Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true}
	cmd.Flags().StringVar(&runID, "run", "", "run id to inspect (required)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runAxiSuperviseStatus(cmd, strings.TrimSpace(runID))
	}
	return cmd
}

func newAxiSuperviseArmCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{Use: "arm", Short: "Arm one active run for an installed Codex Stop hook", Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true}
	cmd.Flags().StringVar(&runID, "run", "", "run id to supervise (required)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runAxiSuperviseArm(cmd, strings.TrimSpace(runID))
	}
	return cmd
}

func newAxiCodexHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "codex-hook",
		Short:         "Codex Stop-hook adapter for armed supervision",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAxiCodexHook(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func newAxiClaudeHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "claude-hook",
		Short:         "Claude Code Stop-hook adapter for armed supervision",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAxiClaudeHook(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func runAxiSuperviseArm(cmd *cobra.Command, runID string) error {
	if runID == "" {
		return emitError(cmd, 2, "--run is required", "Run `no-mistakes axi supervise arm --run <id>` after the AXI run id is known")
	}
	env, err := openAxiEnv(false)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	defer env.close()
	run, err := env.d.GetRun(runID)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get run: %v", err))
	}
	if run == nil || run.RepoID != env.repo.ID || terminalStatus(string(run.Status)) {
		return emitError(cmd, 1, fmt.Sprintf("run %q is not an active run for this repository", runID))
	}
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		return emitError(cmd, 1, "resolve current worktree root")
	}
	branch, err := git.CurrentBranch(cmd.Context(), gitRoot)
	if err != nil || branch == "HEAD" {
		return emitError(cmd, 1, "resolve current worktree branch")
	}
	if run.Branch != branch {
		return emitError(cmd, 1, fmt.Sprintf("run %q belongs to branch %q, not current branch %q", runID, run.Branch, branch))
	}
	cwd, err := canonicalSupervisorCWD(gitRoot)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	reg, err := supervision.NewStore(env.p.SupervisionDir()).Arm(supervision.Registration{RunID: runID, RepoID: env.repo.ID, CWD: cwd, Branch: branch})
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	emitDoc(cmd,
		toonField("supervision", "armed"),
		toonField("run_id", reg.RunID),
		toonField("cwd", reg.CWD),
		toonField("hook_required", true),
		toonField("single_session_per_worktree_required", true),
		toonField("help", []string{"Install the documented Codex Stop hook before ending this turn; it keeps this same turn alive for technical events and pauses for your decisions."}),
	)
	return nil
}

func runAxiSuperviseStatus(cmd *cobra.Command, runID string) error {
	if runID == "" {
		return emitError(cmd, 2, "--run is required", "Run `no-mistakes axi supervise status --run <id>`")
	}
	p, d, err := openResources()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	defer d.Close()
	reg, found, err := supervision.NewStore(p.SupervisionDir()).Get(runID)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	if !found {
		return emitError(cmd, 1, "no local supervision is registered for this run")
	}
	fields := []toon.Field{
		toonField("supervision", string(reg.Phase)),
		toonField("run_id", reg.RunID),
		toonField("session_bound", reg.SessionID != ""),
		toonField("stale_heartbeats", reg.StaleHeartbeats),
		toonField("updated_at", reg.UpdatedAt),
	}
	if reg.Error != "" {
		fields = append(fields, toonField("error", reg.Error))
	}
	emitDoc(cmd, fields...)
	return nil
}

// runAxiCodexHook is intentionally quiet unless it emits the documented Stop
// continuation object. Hook input and any pipeline text are never mirrored to
// stdout; the resumed Codex turn reads the bound AXI status itself.
func runAxiCodexHook(in io.Reader, out io.Writer) error {
	var event codexHookEvent
	if err := json.NewDecoder(io.LimitReader(in, 64<<10)).Decode(&event); err != nil {
		return nil
	}
	if event.HookEventName != "Stop" || strings.TrimSpace(event.SessionID) == "" || strings.TrimSpace(event.TurnID) == "" {
		return nil
	}
	return runAxiSupervisorHook(supervisorHookEvent{SessionID: event.SessionID, HandoffID: event.TurnID, CWD: event.CWD}, out)
}

// runAxiClaudeHook is intentionally quiet unless it emits the documented Stop
// continuation object. Claude's message digest is local-only duplicate
// suppression; no transcript or message content is read or retained.
func runAxiClaudeHook(in io.Reader, out io.Writer) error {
	var event claudeHookEvent
	if err := json.NewDecoder(io.LimitReader(in, 64<<10)).Decode(&event); err != nil {
		return nil
	}
	if event.HookEventName != "Stop" || strings.TrimSpace(event.SessionID) == "" {
		return nil
	}
	handoffID := claudeHookHandoffID(event.SessionID, event.LastAssistantMessage)
	if handoffID == "" {
		return nil
	}
	return runAxiSupervisorHook(supervisorHookEvent{SessionID: event.SessionID, HandoffID: handoffID, CWD: event.CWD}, out)
}

func claudeHookHandoffID(sessionID, lastAssistantMessage string) string {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(lastAssistantMessage) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + lastAssistantMessage))
	return "claude:" + hex.EncodeToString(sum[:])
}

func runAxiSupervisorHook(event supervisorHookEvent, out io.Writer) error {
	cwd, err := canonicalSupervisorCWD(event.CWD)
	if err != nil {
		return nil
	}
	p, d, err := openResources()
	if err != nil {
		return nil
	}
	defer d.Close()
	store := supervision.NewStore(p.SupervisionDir())
	reg, found, err := store.FindByCWD(cwd)
	if err != nil || !found {
		return nil
	}
	if reg.Phase == supervision.PhaseArmed {
		reg, found, err = store.Claim(cwd, event.SessionID)
		if err != nil || !found {
			return nil
		}
	} else if reg.SessionID != event.SessionID {
		return nil
	}
	if reg.Phase == supervision.PhaseAwaitingMerge || reg.Phase == supervision.PhasePaused || reg.Phase == supervision.PhaseCompleted {
		return nil
	}
	if !supervisionRepoMatches(d, cwd, reg.RepoID) {
		_, _, _ = store.UpdateForSession(reg.RunID, event.SessionID, func(reg *supervision.Registration) {
			reg.Phase, reg.Error = supervision.PhasePaused, "repo_binding_mismatch"
		})
		return nil
	}
	if !supervisionBranchMatches(context.Background(), cwd, reg.Branch) {
		_, _, _ = store.UpdateForSession(reg.RunID, event.SessionID, func(reg *supervision.Registration) {
			reg.Phase, reg.Error = supervision.PhasePaused, "branch_binding_mismatch"
		})
		return nil
	}

	outcome, run, err := waitForSupervisorEvent(p, reg)
	if err != nil {
		outcome = supervisorWatchFault
	}
	if run != nil && (run.RepoID != reg.RepoID || run.Branch != reg.Branch) {
		_, _, _ = store.UpdateForSession(reg.RunID, event.SessionID, func(reg *supervision.Registration) {
			reg.Phase, reg.Error = supervision.PhasePaused, "run_binding_mismatch"
		})
		return nil
	}
	applySupervisorOutcome(store, p, reg, event, outcome, run, out)
	return nil
}

func supervisionBranchMatches(ctx context.Context, cwd, branch string) bool {
	if strings.TrimSpace(branch) == "" {
		return false
	}
	current, err := git.CurrentBranch(ctx, cwd)
	return err == nil && current == branch
}

func supervisionRepoMatches(d interface {
	GetRepo(string) (*db.Repo, error)
	GetRepoByPath(string) (*db.Repo, error)
}, cwd, repoID string) bool {
	root, err := git.FindGitRoot(cwd)
	if err != nil {
		return false
	}
	root, err = canonicalSupervisorCWD(root)
	if err != nil {
		return false
	}
	repo, err := d.GetRepoByPath(root)
	if err == nil && repo != nil {
		return repo.ID == repoID
	}
	mainRoot, err := git.FindMainRepoRoot(cwd)
	if err != nil {
		return false
	}
	mainRoot, err = canonicalSupervisorCWD(mainRoot)
	if err != nil {
		return false
	}
	repo, err = d.GetRepoByPath(mainRoot)
	return err == nil && repo != nil && repo.ID == repoID
}

func waitForSupervisorEvent(p *paths.Paths, reg supervision.Registration) (supervisorOutcome, *ipc.RunInfo, error) {
	if alive, _ := daemon.IsRunning(p); !alive {
		return supervisorWatchFault, nil, fmt.Errorf("daemon unavailable")
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return supervisorWatchFault, nil, err
	}
	defer client.Close()
	read := func() (*ipc.RunInfo, error) { return getRunInfo(client, reg.RunID) }
	run, err := read()
	if err != nil || run == nil {
		return supervisorWatchFault, nil, err
	}
	if outcome := classifySupervisorRun(run, ciLogReader(p)); outcome != supervisorNone {
		return outcome, run, nil
	}
	deadline := supervisorHeartbeatDeadline(reg)
	handshakeCtx, cancelHandshake := context.WithDeadline(context.Background(), deadline)
	events, unsubscribe, err := ipc.SubscribeWithHandshakeContext(handshakeCtx, context.Background(), p.Socket(), &ipc.SubscribeParams{RunID: reg.RunID})
	cancelHandshake()
	if err != nil {
		return supervisorWatchFault, run, err
	}
	defer unsubscribe()
	run, err = read()
	if err != nil || run == nil {
		return supervisorWatchFault, run, err
	}
	if outcome := classifySupervisorRun(run, ciLogReader(p)); outcome != supervisorNone {
		return outcome, run, nil
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			run, err := read()
			if err != nil || run == nil {
				return supervisorWatchFault, run, err
			}
			if outcome := classifySupervisorRun(run, ciLogReader(p)); outcome != supervisorNone {
				return outcome, run, nil
			}
			return supervisorHeartbeat, run, nil
		case _, ok := <-events:
			run, err := read()
			if err != nil || run == nil {
				return supervisorWatchFault, run, err
			}
			if outcome := classifySupervisorRun(run, ciLogReader(p)); outcome != supervisorNone {
				return outcome, run, nil
			}
			if !ok {
				return supervisorWatchFault, run, fmt.Errorf("event stream closed")
			}
		}
	}
}

func supervisorHeartbeatDeadline(reg supervision.Registration) time.Time {
	deadline := time.Unix(reg.NextHeartbeatAt, 0)
	if reg.NextHeartbeatAt == 0 || !deadline.After(supervisionNow()) {
		return supervisionNow().Add(supervisionHeartbeat)
	}
	return deadline
}

func classifySupervisorRun(run *ipc.RunInfo, logs func(string) []string) supervisorOutcome {
	rv := runViewFromIPC(run)
	if terminalStatus(rv.Status) {
		return supervisorTerminal
	}
	if gate, ok := rv.awaitingStep(); ok {
		findings, err := types.ParseFindingsJSON(gate.FindingsJSON)
		if err != nil || types.HasAskUserFindings(findings) {
			return supervisorAskUser
		}
		return supervisorTechnicalGate
	}
	if ciReadyToMerge(rv, logs(run.ID)) {
		return supervisorChecksPassed
	}
	return supervisorNone
}

func applySupervisorOutcome(store *supervision.Store, p *paths.Paths, reg supervision.Registration, event supervisorHookEvent, outcome supervisorOutcome, run *ipc.RunInfo, out io.Writer) {
	if outcome == supervisorNone {
		return
	}
	fingerprint := supervisorFingerprint(outcome, run, reg.NextHeartbeatAt)
	nextHeartbeat := supervisionNow().Add(supervisionHeartbeat).Unix()
	stale := reg.StaleHeartbeats
	phase := supervision.PhaseHandoffInProgress
	reason := ""
	switch outcome {
	case supervisorAskUser:
		_, _, _ = store.UpdateForSession(reg.RunID, event.SessionID, func(reg *supervision.Registration) {
			reg.Phase, reg.Fingerprint, reg.Error = supervision.PhaseAwaitingUser, supervisorProgressFingerprint(run), ""
		})
		return
	case supervisorTechnicalGate:
		reason = "nm_event=technical_gate"
	case supervisorChecksPassed:
		phase, reason = supervision.PhaseAwaitingMerge, "nm_event=checks_passed"
	case supervisorTerminal:
		phase, reason = supervision.PhaseCompleted, "nm_event=terminal"
	case supervisorWatchFault:
		phase, reason = supervision.PhasePaused, "nm_event=watch_fault"
	case supervisorHeartbeat:
		if reg.Fingerprint == supervisorProgressFingerprint(run) {
			if stale >= configStaleHeartbeatLimit(p) {
				phase, reason = supervision.PhasePaused, "nm_event=stale"
			} else {
				stale++
				reason = "nm_event=heartbeat"
			}
		} else {
			stale = 0
			reason = "nm_event=heartbeat"
		}
	}
	if reason == "" {
		return
	}
	prepared, emit, err := store.PrepareHandoff(reg.RunID, event.SessionID, event.HandoffID, fingerprint, supervisorProgressFingerprint(run), phase, nextHeartbeat, stale)
	if err != nil || !emit || prepared.SessionID != event.SessionID {
		return
	}
	_, _ = io.WriteString(out, `{"decision":"block","reason":"`+reason+`"}`+"\n")
}

func supervisorFingerprint(outcome supervisorOutcome, run *ipc.RunInfo, deadline int64) string {
	return string(outcome) + "|" + supervisorProgressFingerprint(run) + fmt.Sprintf("|%d", deadline)
}

func supervisorProgressFingerprint(run *ipc.RunInfo) string {
	if run == nil {
		return "unavailable"
	}
	parts := []string{string(run.Status), fmt.Sprintf("%d", run.UpdatedAt), fmt.Sprintf("%t", run.AwaitingAgent)}
	for _, step := range run.Steps {
		activity := int64(0)
		if step.LastActivityAt != nil {
			activity = *step.LastActivityAt
		}
		parts = append(parts, string(step.StepName)+":"+string(step.Status)+":"+fmt.Sprintf("%d:%d:%d", step.RoundCount, step.FixRoundCount, activity))
	}
	return strings.Join(parts, "|")
}

func configStaleHeartbeatLimit(p *paths.Paths) int {
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return config.DefaultSupervisionMaxStaleHeartbeats
	}
	return cfg.SupervisionMaxStaleHeartbeats
}

func canonicalSupervisorCWD(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		var err error
		value, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get cwd: %w", err)
		}
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func toonField(key string, value any) toon.Field { return toon.Field{Key: key, Value: value} }
