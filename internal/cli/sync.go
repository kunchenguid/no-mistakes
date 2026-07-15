package cli

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	toON "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/spf13/cobra"
)

var syncInteractive = terminalInteractive

func newSyncCmd() *cobra.Command {
	var check, yes bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Safely fast-forward the current branch to an exact pipeline-pushed head",
		Long: "Refreshes the current branch's persisted pipeline push binding and, after\n" +
			"confirmation, advances only a completely clean checked-out branch by a strict\n" +
			"fast-forward. It never resets, stashes, merges divergent work, rebases, switches\n" +
			"branches, or updates a remote. --check performs the fresh proof without applying it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check && yes {
				return &exitError{code: 2, err: fmt.Errorf("--check and --yes cannot be used together")}
			}
			return runHumanSync(cmd, check, yes)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and show the synchronization plan without changing HEAD")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply an eligible strict fast-forward without prompting")
	return cmd
}

func newAxiSyncCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Check or apply the guarded current-branch fast-forward",
		Long: "Verifies the registered invoking worktree, clean exact branch, persisted\n" +
			"pipeline push binding, configured fork or upstream target, live remote equality,\n" +
			"and strict ancestry. The default applies an eligible fast-forward without a prompt.\n" +
			"--check performs the same fresh read-only plan. Blocked states change nothing.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAxiSync(cmd, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and return the plan without changing HEAD")
	return cmd
}

func openSyncService() (*branchsync.Service, func(), error) {
	_, d, err := openResources()
	if err != nil {
		return nil, nil, err
	}
	repo, err := findRepo(d)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	return &branchsync.Service{DB: d, Repo: repo, WorkDir: "."}, func() { _ = d.Close() }, nil
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
	if state.Safety != "safe_fast_forward" {
		result = "refused"
		return &exitError{code: 1}
	}
	if !yes {
		if !syncInteractive() {
			fmt.Fprintln(cmd.OutOrStdout(), "  Non-interactive input cannot confirm this plan. Re-run with `no-mistakes sync --yes`.")
			result = "refused"
			return &exitError{code: 1}
		}
		fmt.Fprint(cmd.OutOrStdout(), "  Apply this exact strict fast-forward? [y/N] ")
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

func printHumanSyncState(cmd *cobra.Command, state branchsync.State) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "\n  Local branch: %s\n", humanSyncSummary(state))
	if state.Local.Head != "" {
		fmt.Fprintf(w, "  local:    %s %s\n", state.Local.Branch, state.Local.Head)
	}
	if state.Pipeline.PushedHead != "" {
		fmt.Fprintf(w, "  pipeline: %s\n", state.Pipeline.PushedHead)
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
		return "pipeline fix is not pushed yet; do not make local follow-up commits"
	case branchsync.StatePushInProgress:
		return "pipeline branch update is in progress; synchronization is unavailable"
	case branchsync.StateBehind:
		if state.Safety == "safe_fast_forward" {
			return "clean and strictly behind; exact safe fast-forward verified"
		}
		return "behind the pipeline-pushed head; refresh required"
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

func runAxiSync(cmd *cobra.Command, check bool) error {
	started := time.Now()
	mode := "apply"
	if check {
		mode = "check"
	}
	var state branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("axi-sync", "axi", mode, state, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer closeFn()

	if check {
		state = service.Refresh(cmd.Context())
	} else {
		state = service.Apply(cmd.Context())
	}
	fields := []toON.Field{branchSyncField(state)}
	if state.Error != "" {
		fields = append(fields, toON.Field{Key: "error", Value: state.Error})
	}
	if state.NextAction != nil {
		fields = append(fields, toON.Field{Key: "help", Value: []string{"Run `" + state.NextAction.Command + "`"}})
	}
	emitDoc(cmd, fields...)
	if syncStateSuccessful(state, check) {
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
	return check && state.Safety == "safe_fast_forward"
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
		{Key: "local", Value: toON.NewObject(local...)},
		{Key: "pipeline", Value: toON.NewObject(pipeline...)},
		{Key: "target", Value: target},
		{Key: "remote", Value: remote},
		{Key: "relation", Value: state.Relation},
		{Key: "safety", Value: state.Safety},
		{Key: "pr_state", Value: state.PRState},
	}
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
		branchsync.StateTargetChanged:
		return true
	default:
		return false
	}
}
