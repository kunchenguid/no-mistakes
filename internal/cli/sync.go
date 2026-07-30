package cli

import (
	"bufio"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	toON "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/spf13/cobra"
)

var syncInteractive = terminalInteractive

func newSyncCmd() *cobra.Command {
	var check, yes, recover, keepLocal bool
	var runID, adoptForwardHead string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Safely move the current branch to an exact pipeline-pushed head",
		Long: "Refreshes the current branch's persisted pipeline push binding and, after\n" +
			"confirmation, advances only a completely clean checked-out branch using one of\n" +
			"two guarded modes: a strict fast-forward for clean behind branches, or an\n" +
			"equivalent-diverged advance that first anchors the old head and then moves the\n" +
			"branch to the verified pipeline head with reset semantics. It never stashes,\n" +
			"merges genuine divergence, rebases, switches branches, or updates a remote.\n" +
			"--check performs the fresh proof without applying it.\n" +
			"--recover returns custody of a branch whose run went terminal with unpublished\n" +
			"pipeline commits: it anchors the preserved head, fast-forwards a clean behind\n" +
			"worktree to it, and frees the branch for a fresh run. --recover --keep-local keeps\n" +
			"the current local head instead and never touches the worktree.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			exactRecovery := runID != "" || adoptForwardHead != ""
			if check && yes {
				return &exitError{code: 2, err: fmt.Errorf("--check and --yes cannot be used together")}
			}
			if check && recover {
				return &exitError{code: 2, err: fmt.Errorf("--check and --recover cannot be used together")}
			}
			if keepLocal && !recover {
				return &exitError{code: 2, err: fmt.Errorf("--keep-local requires --recover")}
			}
			if exactRecovery {
				if !recover || runID == "" || adoptForwardHead == "" {
					return &exitError{code: 2, err: fmt.Errorf("--run and --adopt-forward-head are required together and only with --recover")}
				}
				if check || keepLocal {
					return &exitError{code: 2, err: fmt.Errorf("operator-authorized forward-head recovery cannot be combined with --check or --keep-local")}
				}
				if !branchsync.IsExactRunID(runID) || !branchsync.IsExactFullObjectID(adoptForwardHead) {
					return &exitError{code: 2, err: fmt.Errorf("--run must be exact and --adopt-forward-head must be a canonical full 40- or 64-hex object ID")}
				}
				return runHumanForwardRecovery(cmd, runID, adoptForwardHead, yes)
			}
			if recover {
				return runHumanRecover(cmd, keepLocal, yes)
			}
			return runHumanSync(cmd, check, yes)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and show the synchronization plan without changing HEAD")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply an eligible guarded synchronization without prompting")
	cmd.Flags().BoolVar(&recover, "recover", false, "return custody of a branch stranded by a terminal run with unpublished pipeline commits")
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "with --recover: keep the current local head; the preserved commits stay anchored and the gate follows the kept head")
	cmd.Flags().StringVar(&runID, "run", "", "with --recover --adopt-forward-head: exact completed run ID")
	cmd.Flags().StringVar(&adoptForwardHead, "adopt-forward-head", "", "with --recover --run: exact full operator-authorized candidate commit")
	return cmd
}

func newAxiSyncCmd() *cobra.Command {
	var check, recover, keepLocal bool
	var runID, adoptForwardHead string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Check or apply guarded current-branch synchronization",
		Long: "Verifies the registered invoking worktree, clean exact branch, persisted\n" +
			"pipeline push binding, configured fork or upstream target, live remote equality,\n" +
			"and either strict ancestry or content-equivalent divergence. The default applies\n" +
			"an eligible plan without a prompt: strict fast-forward for behind branches, or an\n" +
			"equivalent advance that anchors the old head before moving the branch to the\n" +
			"verified pipeline head with reset semantics.\n" +
			"--check performs the same fresh read-only plan. Blocked states change nothing.\n" +
			"--recover performs the guarded custody return offered by\n" +
			"next_action.code: recover_custody; --keep-local keeps the current local head.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			exactRecovery := runID != "" || adoptForwardHead != ""
			if check && recover {
				return emitError(cmd, 2, "--check and --recover cannot be used together")
			}
			if keepLocal && !recover {
				return emitError(cmd, 2, "--keep-local requires --recover")
			}
			if exactRecovery {
				if !recover || runID == "" || adoptForwardHead == "" {
					return emitError(cmd, 2, "--run and --adopt-forward-head are required together and only with --recover")
				}
				if check || keepLocal {
					return emitError(cmd, 2, "operator-authorized forward-head recovery cannot be combined with --check or --keep-local")
				}
				if !branchsync.IsExactRunID(runID) || !branchsync.IsExactFullObjectID(adoptForwardHead) {
					return emitError(cmd, 2, "--run must be exact and --adopt-forward-head must be a canonical full 40- or 64-hex object ID")
				}
				return runAxiForwardRecovery(cmd, runID, adoptForwardHead)
			}
			return runAxiSync(cmd, check, recover, keepLocal)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and return the plan without changing HEAD")
	cmd.Flags().BoolVar(&recover, "recover", false, "return custody of a branch stranded by a terminal run with unpublished pipeline commits")
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "with --recover: keep the current local head; the preserved commits stay anchored and the gate follows the kept head")
	cmd.Flags().StringVar(&runID, "run", "", "with --recover --adopt-forward-head: exact completed run ID")
	cmd.Flags().StringVar(&adoptForwardHead, "adopt-forward-head", "", "with --recover --run: exact full operator-authorized candidate commit")
	return cmd
}

func callForwardRecovery(cmd *cobra.Command, runID, candidate string) (ipc.RecoverForwardHeadResult, error) {
	var result ipc.RecoverForwardHeadResult
	p, err := paths.New()
	if err != nil {
		return result, fmt.Errorf("resolve paths: %w", err)
	}
	if err := daemon.EnsureDaemon(p); err != nil {
		return result, err
	}
	workDir, err := filepath.Abs(".")
	if err != nil {
		return result, fmt.Errorf("resolve invoking worktree: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return result, err
	}
	defer client.Close()
	if err := client.Call(ipc.MethodRecoverForwardHead, &ipc.RecoverForwardHeadParams{
		RunID: runID, Candidate: candidate, WorkDir: workDir,
	}, &result); err != nil {
		return result, fmt.Errorf("daemon-owned forward-head recovery: %w", err)
	}
	return result, nil
}

func runHumanForwardRecovery(cmd *cobra.Command, runID, candidate string, yes bool) error {
	if !yes {
		if !syncInteractive() {
			fmt.Fprintln(cmd.OutOrStdout(), "  Non-interactive input cannot authorize this exact historical repair. Re-run with --yes after reviewing the run and full candidate.")
			return &exitError{code: 1}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\n  Candidate %s is explicit operator authorization.\n", candidate)
		fmt.Fprintln(cmd.OutOrStdout(), "  Ancestry and gate equality preserve history but do not prove this historical run produced it.")
		fmt.Fprintf(cmd.OutOrStdout(), "  Authorize exact run %s and strict-forward local recovery? [y/N] ", runID)
		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "  Cancelled; no recovery mutation was requested.")
			return nil
		}
	}
	result, err := callForwardRecovery(cmd, runID, candidate)
	if err != nil {
		return err
	}
	printHumanForwardRecovery(cmd, result)
	if !result.Recovered {
		return &exitError{code: 1}
	}
	return nil
}

func runAxiForwardRecovery(cmd *cobra.Command, runID, candidate string) error {
	result, err := callForwardRecovery(cmd, runID, candidate)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	fields := []toON.Field{
		{Key: "recovery_mode", Value: result.Mode}, {Key: "recovered", Value: result.Recovered},
		{Key: "changed", Value: result.Changed}, {Key: "state", Value: result.State},
		{Key: "safety", Value: result.Safety}, {Key: "phase", Value: result.Phase},
		{Key: "run_id", Value: result.RunID}, {Key: "repo_id", Value: result.RepoID},
		{Key: "branch", Value: result.Branch}, {Key: "local_head", Value: result.LocalHead},
		{Key: "recorded_head", Value: result.RecordedHead}, {Key: "candidate", Value: result.Candidate},
		{Key: "anchor_ref", Value: result.AnchorRef},
	}
	if result.Error != "" {
		fields = append(fields, toON.Field{Key: "error", Value: result.Error})
	}
	emitDoc(cmd, fields...)
	if !result.Recovered {
		return &exitError{code: 1}
	}
	return nil
}

func printHumanForwardRecovery(cmd *cobra.Command, result ipc.RecoverForwardHeadResult) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "\n  Recovery phase: %s\n", result.Phase)
	fmt.Fprintf(w, "  run:       %s\n  candidate: %s\n", result.RunID, result.Candidate)
	if result.AnchorRef != "" {
		fmt.Fprintf(w, "  anchor:    %s\n", result.AnchorRef)
	}
	if result.LocalHead != "" {
		fmt.Fprintf(w, "  local:     %s\n", result.LocalHead)
	}
	if result.Error != "" {
		fmt.Fprintf(w, "  blocked:   %s\n", result.Error)
	} else if result.Recovered {
		fmt.Fprintln(w, "  Custody returned under the exact audited predicate.")
	}
}

func openSyncService() (*branchsync.Service, func(), error) {
	p, d, err := openResources()
	if err != nil {
		return nil, nil, err
	}
	repo, err := findRepo(d)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	return &branchsync.Service{DB: d, Repo: repo, WorkDir: ".", GateDir: p.RepoDir(repo.ID), Paths: p}, func() { _ = d.Close() }, nil
}

func runHumanSync(cmd *cobra.Command, check, yes bool) error {
	started := time.Now()
	mode := "apply"
	if check {
		mode = "check"
	}
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", mode, observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	state := service.Refresh(cmd.Context())
	observed = state
	printHumanSyncState(cmd, state)
	if check {
		if syncStateSuccessful(state, true) {
			result = "noop"
			return nil
		}
		result = "refused"
		return &exitError{code: 1}
	}
	if state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved {
		result = "noop"
		return nil
	}
	if !branchsync.CanApply(state) {
		result = "refused"
		return &exitError{code: 1}
	}
	if !yes {
		if !syncInteractive() {
			fmt.Fprintln(cmd.OutOrStdout(), "  Non-interactive input cannot confirm this plan. Re-run with `no-mistakes sync --yes`.")
			result = "refused"
			return &exitError{code: 1}
		}
		if state.Safety == branchsync.SafetySafeEquivalentAdvance {
			fmt.Fprint(cmd.OutOrStdout(), "  Apply this guarded synchronization? [y/N] ")
		} else {
			fmt.Fprint(cmd.OutOrStdout(), "  Apply this exact strict fast-forward? [y/N] ")
		}
		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "  Cancelled; no files or refs were changed.")
			result = "cancelled"
			return nil
		}
	}

	applyResult := service.Apply(cmd.Context())
	observed = applyResult
	printHumanSyncState(cmd, applyResult)
	if syncStateSuccessful(applyResult, false) {
		if applyResult.Changed {
			result = "applied"
		} else {
			result = "noop"
		}
		return nil
	}
	result = "refused"
	return &exitError{code: 1}
}

func runHumanRecover(cmd *cobra.Command, keepLocal, yes bool) error {
	started := time.Now()
	mode := "recover"
	if keepLocal {
		mode = "recover_keep_local"
	}
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", mode, observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	state := service.InspectCached(cmd.Context())
	observed = state
	if !yes {
		printHumanSyncState(cmd, state)
		if !syncInteractive() {
			fmt.Fprintln(cmd.OutOrStdout(), "  Non-interactive input cannot confirm this recovery. Re-run with `no-mistakes sync --recover --yes`.")
			result = "refused"
			return &exitError{code: 1}
		}
		fmt.Fprintln(cmd.OutOrStdout(), "  Recovery returns custody of this branch from its terminal run. The only")
		if keepLocal {
			fmt.Fprintln(cmd.OutOrStdout(), "  possible changes are anchoring the preserved pipeline commits and moving the")
			fmt.Fprintln(cmd.OutOrStdout(), "  local gate branch to your current head; the worktree is never touched.")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "  possible worktree change is a strict fast-forward of this clean branch to the")
			fmt.Fprintln(cmd.OutOrStdout(), "  preserved pipeline head; anything else refuses without changes.")
		}
		fmt.Fprint(cmd.OutOrStdout(), "  Return custody of this branch? [y/N] ")
		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "  Cancelled; no files or refs were changed.")
			result = "cancelled"
			return nil
		}
	}

	recovered := service.Recover(cmd.Context(), keepLocal)
	observed = recovered
	printHumanSyncState(cmd, recovered)
	if recovered.Recovered {
		fmt.Fprintln(cmd.OutOrStdout(), "  Custody returned; start a fresh run when ready.")
		if recovered.Changed {
			result = "applied"
		} else {
			result = "noop"
		}
		return nil
	}
	result = "refused"
	return &exitError{code: 1}
}

func printHumanSyncState(cmd *cobra.Command, state branchsync.State) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "\n  Local branch: %s\n", humanSyncSummary(state))
	if state.Local.Head != "" {
		fmt.Fprintf(w, "  local:    %s %s\n", state.Local.Branch, state.Local.Head)
	}
	if state.Pipeline.PushedHead != "" {
		fmt.Fprintf(w, "  pipeline: %s\n", state.Pipeline.PushedHead)
	} else if state.Pipeline.CurrentHead != "" && state.Pipeline.CurrentHead != state.Local.Head {
		fmt.Fprintf(w, "  preserved: %s (run %s, %s)\n", state.Pipeline.CurrentHead, state.Pipeline.RunID, state.Pipeline.Status)
	}
	if state.Target.Ref != "" {
		fmt.Fprintf(w, "  target:   %s %s (%s)\n", state.Target.Remote, state.Target.Ref, state.Target.Kind)
	}
	if state.Error != "" {
		fmt.Fprintf(w, "  blocked:  %s\n", state.Error)
	}
}

func humanSyncSummary(state branchsync.State) string {
	switch state.State {
	case branchsync.StatePipelineOwned:
		if state.Safety == "blocked_pipeline_owned_recoverable" {
			return "run ended without publishing its pipeline commits; recover custody with `no-mistakes sync --recover` (or `no-mistakes rerun` to resume validation)"
		}
		return "pipeline fix is not pushed yet; do not make local follow-up commits"
	case branchsync.StateCustodyReturned:
		return "custody returned; the branch is yours - start a fresh run when ready"
	case branchsync.StatePushInProgress:
		return "pipeline branch update is in progress; synchronization is unavailable"
	case branchsync.StateBehind:
		if state.Safety == branchsync.SafetySafeFastForward {
			return "clean and strictly behind; exact safe fast-forward verified"
		}
		return "behind the pipeline-pushed head; refresh required"
	case branchsync.StateDiverged:
		if state.Safety == branchsync.SafetySafeEquivalentAdvance {
			return "diverged, but local changes are represented in the pipeline head; guarded advance verified"
		}
		if state.NextAction != nil && state.NextAction.Code == "sync" {
			return "diverged; refresh required to verify equivalent pipeline content"
		}
		return "diverged from the pipeline-pushed head; manual reconciliation required"
	case branchsync.StateSynchronized:
		return "already synchronized with the pipeline-pushed head"
	case branchsync.StateMergedRemoteRemoved:
		return "PR merged and remote feature branch removed; nothing to synchronize"
	case branchsync.StateMergedRemoteRetained:
		return "PR merged; feature branch is retired and local branch was not changed"
	case branchsync.StateClosed:
		return "PR closed; feature branch is retired and local branch was not changed"
	default:
		return strings.ReplaceAll(state.State, "_", " ")
	}
}

func runAxiSync(cmd *cobra.Command, check, recover, keepLocal bool) error {
	started := time.Now()
	mode := "apply"
	switch {
	case check:
		mode = "check"
	case recover && keepLocal:
		mode = "recover_keep_local"
	case recover:
		mode = "recover"
	}
	var state branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("axi-sync", "axi", mode, state, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer closeFn()

	switch {
	case check:
		state = service.Refresh(cmd.Context())
	case recover:
		state = service.Recover(cmd.Context(), keepLocal)
	default:
		state = service.Apply(cmd.Context())
	}
	fields := []toON.Field{branchSyncField(state)}
	if state.Error != "" {
		fields = append(fields, toON.Field{Key: "error", Value: state.Error})
	}
	var help []string
	if state.NextAction != nil {
		help = append(help, "Run `"+state.NextAction.Command+"`")
	}
	if state.Safety == "blocked_pipeline_owned_recoverable" {
		help = append(help, "Run `no-mistakes rerun` instead to resume validating the preserved pipeline head")
	}
	if len(help) > 0 {
		fields = append(fields, toON.Field{Key: "help", Value: help})
	}
	emitDoc(cmd, fields...)
	successful := syncStateSuccessful(state, check)
	if recover {
		successful = state.Recovered
	}
	if successful {
		if state.Changed {
			result = "applied"
		} else {
			result = "noop"
		}
		return nil
	}
	result = "refused"
	return &exitError{code: 1}
}

func trackSyncAttempt(command, surface, mode string, state branchsync.State, result string, started time.Time) {
	telemetry.Track("command", telemetry.Fields{
		"command":      command,
		"surface":      surface,
		"mode":         mode,
		"status":       result,
		"result":       result,
		"state_before": boundedSyncValue(state.State),
		"relation":     boundedSyncValue(state.Relation),
		"target_kind":  boundedSyncValue(state.Target.Kind),
		"run_phase":    boundedSyncValue(state.Pipeline.Phase),
		"pr_state":     boundedSyncValue(state.PRState),
		"reason":       boundedSyncValue(state.Safety),
		"dirty":        !state.Local.Clean && state.Local.Head != "",
		"duration_ms":  time.Since(started).Milliseconds(),
	})
}

func boundedSyncValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	if len(value) > 64 {
		return "unknown"
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && r != '_' {
			return "unknown"
		}
	}
	return value
}

func syncStateSuccessful(state branchsync.State, check bool) bool {
	if state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved {
		return true
	}
	// A recovered branch has no pending synchronization: custody is with the
	// operator and the next step is a fresh run, not a blocked exit code.
	if state.State == branchsync.StateCustodyReturned {
		return true
	}
	return check && branchsync.CanApply(state)
}

func branchSyncField(state branchsync.State) toON.Field {
	local := []toON.Field{
		{Key: "branch", Value: state.Local.Branch},
		{Key: "head", Value: state.Local.Head},
		{Key: "clean", Value: state.Local.Clean},
	}
	if state.Local.Reason != "" {
		local = append(local, toON.Field{Key: "reason", Value: state.Local.Reason})
	}
	pipeline := []toON.Field{
		{Key: "run", Value: state.Pipeline.RunID},
		{Key: "status", Value: state.Pipeline.Status},
		{Key: "phase", Value: state.Pipeline.Phase},
		{Key: "submitted_head", Value: state.Pipeline.SubmittedHead},
		{Key: "current_head", Value: state.Pipeline.CurrentHead},
		{Key: "pushed_head", Value: state.Pipeline.PushedHead},
		{Key: "pushed_at", Value: state.Pipeline.PushedAt},
		{Key: "push_generation", Value: state.Pipeline.PushGeneration},
	}
	target := toON.NewObject(
		toON.Field{Key: "kind", Value: state.Target.Kind},
		toON.Field{Key: "remote", Value: state.Target.Remote},
		toON.Field{Key: "url", Value: state.Target.URL},
		toON.Field{Key: "ref", Value: state.Target.Ref},
	)
	remote := toON.NewObject(
		toON.Field{Key: "observed_head", Value: state.Remote.ObservedHead},
		toON.Field{Key: "freshness", Value: state.Remote.Freshness},
		toON.Field{Key: "observed_at", Value: state.Remote.ObservedAt},
	)
	fields := []toON.Field{
		{Key: "state", Value: state.State},
		{Key: "changed", Value: state.Changed},
	}
	if state.Recovered {
		fields = append(fields, toON.Field{Key: "recovered", Value: true})
	}
	fields = append(fields,
		toON.Field{Key: "local", Value: toON.NewObject(local...)},
		toON.Field{Key: "pipeline", Value: toON.NewObject(pipeline...)},
		toON.Field{Key: "target", Value: target},
		toON.Field{Key: "remote", Value: remote},
		toON.Field{Key: "relation", Value: state.Relation},
		toON.Field{Key: "safety", Value: state.Safety},
		toON.Field{Key: "pr_state", Value: state.PRState},
	)
	if state.Error != "" {
		fields = append(fields, toON.Field{Key: "note", Value: state.Error})
	}
	if state.NextAction != nil {
		fields = append(fields, toON.Field{Key: "next_action", Value: toON.NewObject(
			toON.Field{Key: "code", Value: state.NextAction.Code},
			toON.Field{Key: "command", Value: state.NextAction.Command},
		)})
	}
	return toON.Field{Key: "branch_sync", Value: toON.NewObject(fields...)}
}

func cachedBranchSyncField(ctxCmd *cobra.Command, runID string) *toON.Field {
	service, closeFn, err := openSyncService()
	if err != nil {
		return nil
	}
	defer closeFn()
	state := service.InspectCached(ctxCmd.Context())
	if runID != "" && state.Pipeline.RunID != runID {
		return nil
	}
	if !relevantCachedSyncState(state) {
		return nil
	}
	field := branchSyncField(state)
	return &field
}

func relevantCachedSyncState(state branchsync.State) bool {
	switch state.State {
	case branchsync.StatePipelineOwned, branchsync.StatePushInProgress, branchsync.StateBehind,
		branchsync.StateLocalAhead, branchsync.StateDiverged, branchsync.StateDirty,
		branchsync.StateRemoteAdvanced, branchsync.StateRemoteRewritten, branchsync.StateRemoteMissing,
		branchsync.StateMergedRemoteRetained, branchsync.StateMergedRemoteRemoved, branchsync.StateClosed,
		branchsync.StateTargetChanged, branchsync.StateCustodyReturned:
		return true
	default:
		return false
	}
}
