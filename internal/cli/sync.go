package cli

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	toON "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/spf13/cobra"
)

var syncInteractive = terminalInteractive

func newSyncCmd() *cobra.Command {
	var check, yes, recover, keepLocal bool
	var bindArchiveRef string
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
			"pipeline commits: it anchors an available preserved head, then either\n" +
			"fast-forwards a clean behind worktree or adopts a diverged preserved head only\n" +
			"when proven to carry every local change. Unproven divergence refuses. A run\n" +
			"cancelled before the pipeline changed anything releases the branch by itself\n" +
			"(user_owned) and makes --recover a no-op. --recover --keep-local keeps the\n" +
			"current local head and never touches the worktree; available preserved commits\n" +
			"stay anchored, while genuinely missing preserved commits are discarded.\n" +
			"--bind-archive-ref records one exact existing refs/heads/archive/* commit as\n" +
			"evidence for the narrow keep-local recovery that stays at a required head while\n" +
			"a divergent later head remains archived; it never creates or moves a Git ref.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check && yes {
				return &exitError{code: 2, err: fmt.Errorf("--check and --yes cannot be used together")}
			}
			if check && recover {
				return &exitError{code: 2, err: fmt.Errorf("--check and --recover cannot be used together")}
			}
			if keepLocal && !recover {
				return &exitError{code: 2, err: fmt.Errorf("--keep-local requires --recover")}
			}
			if bindArchiveRef != "" && (check || yes || recover || keepLocal) {
				return &exitError{code: 2, err: fmt.Errorf("--bind-archive-ref cannot be combined with synchronization or recovery flags")}
			}
			if bindArchiveRef != "" {
				return runHumanBindRecoveryArchive(cmd, bindArchiveRef)
			}
			if recover {
				return runHumanRecover(cmd, keepLocal, yes)
			}
			return runHumanSync(cmd, check, yes)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and show the synchronization plan without changing HEAD")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply an eligible guarded synchronization without prompting")
	cmd.Flags().BoolVar(&recover, "recover", false, "return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch)")
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "with --recover: keep the current local head; anchor available preserved commits, discard genuinely missing ones, and make the gate follow the kept head")
	cmd.Flags().StringVar(&bindArchiveRef, "bind-archive-ref", "", "bind one existing refs/heads/archive/* commit as exact keep-local recovery evidence without changing Git refs")
	return cmd
}

func newAxiSyncCmd() *cobra.Command {
	var check, recover, keepLocal bool
	var bindArchiveRef string
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
			"next_action.code: recover_custody; --keep-local keeps the current local head.\n" +
			"--bind-archive-ref binds one exact existing refs/heads/archive/* commit to\n" +
			"the selected terminal run; it never creates or moves a Git ref.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check && recover {
				return emitError(cmd, 2, "--check and --recover cannot be used together")
			}
			if keepLocal && !recover {
				return emitError(cmd, 2, "--keep-local requires --recover")
			}
			if bindArchiveRef != "" && (check || recover || keepLocal) {
				return emitError(cmd, 2, "--bind-archive-ref cannot be combined with synchronization or recovery flags")
			}
			return runAxiSync(cmd, check, recover, keepLocal, bindArchiveRef)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and return the plan without changing HEAD")
	cmd.Flags().BoolVar(&recover, "recover", false, "return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch)")
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "with --recover: keep the current local head; anchor available preserved commits, discard genuinely missing ones, and make the gate follow the kept head")
	cmd.Flags().StringVar(&bindArchiveRef, "bind-archive-ref", "", "bind one existing refs/heads/archive/* commit as exact keep-local recovery evidence without changing Git refs")
	return cmd
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
	globalCfg, cfgErr := config.LoadGlobal(p.ConfigFile())
	if cfgErr != nil {
		d.Close()
		return nil, nil, cfgErr
	}
	return &branchsync.Service{DB: d, Repo: repo, WorkDir: ".", GateDir: p.RepoDir(repo.ID), Paths: p, RemoteTimeout: globalCfg.BranchSyncRemoteTimeout}, func() { _ = d.Close() }, nil
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
	if state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved || state.State == branchsync.StateUserOwned {
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

func runHumanBindRecoveryArchive(cmd *cobra.Command, archiveRef string) error {
	started := time.Now()
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", "bind_archive", observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	state := service.BindRecoveryArchive(cmd.Context(), archiveRef)
	observed = state
	printHumanSyncState(cmd, state)
	if verifiedArchiveRecovery(state) {
		fmt.Fprintln(cmd.OutOrStdout(), "  Archive evidence bound; follow the exact guarded recovery action shown above.")
		result = "applied"
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
	// A branch released by cancellation needs no confirmation: the recovery is
	// an idempotent no-op that cannot mutate anything.
	if !yes && state.State != branchsync.StateUserOwned {
		printHumanSyncState(cmd, state)
		if !syncInteractive() {
			retry := "no-mistakes sync --recover --yes"
			if keepLocal {
				retry = "no-mistakes sync --recover --keep-local --yes"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  Non-interactive input cannot confirm this recovery. Re-run with `%s`.\n", retry)
			result = "refused"
			return &exitError{code: 1}
		}
		fmt.Fprintln(cmd.OutOrStdout(), "  Recovery returns custody of this branch from its terminal run. The only")
		if keepLocal {
			if state.Recovery != nil && state.Recovery.KeepLocal {
				fmt.Fprintln(cmd.OutOrStdout(), "  possible Git change is moving the local gate branch to the exact required")
				fmt.Fprintln(cmd.OutOrStdout(), "  head; the worktree and verified divergent archive are never touched.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "  possible changes are anchoring available preserved pipeline commits, discarding")
				fmt.Fprintln(cmd.OutOrStdout(), "  genuinely missing ones, and moving the local gate branch to your current head;")
				fmt.Fprintln(cmd.OutOrStdout(), "  the worktree is never touched.")
			}
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "  possible worktree change is a fast-forward of this clean behind branch, or")
			fmt.Fprintln(cmd.OutOrStdout(), "  adoption of a diverged preserved head proven to carry every local change;")
			fmt.Fprintln(cmd.OutOrStdout(), "  unproven divergence refuses, and --keep-local keeps the current head.")
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
		if recovered.State == branchsync.StateUserOwned {
			fmt.Fprintln(cmd.OutOrStdout(), "  Nothing to recover; cancellation already released this branch to you.")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "  Custody returned; start a fresh run when ready.")
		}
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
	if state.Recovery != nil && state.Recovery.ArchiveRef != "" {
		fmt.Fprintf(w, "  archive:  %s -> %s (%s)\n", state.Recovery.ArchiveRef, state.Recovery.PreservedHead, state.Recovery.Proof)
		fmt.Fprintf(w, "  required: %s\n", state.Recovery.RequiredHead)
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
			if state.Recovery != nil && state.Recovery.KeepLocal {
				return "later pipeline work is preserved by a verified archive; recover custody at the exact required head with `no-mistakes sync --recover --keep-local`"
			}
			return "run ended without publishing its pipeline commits; recover custody with `no-mistakes sync --recover` (or `no-mistakes rerun` to resume validation)"
		}
		if state.Safety == "blocked_recover_preserved_head_missing" {
			return "run ended without a recoverable preserved head; recover custody with `no-mistakes sync --recover --keep-local` to keep the current local head"
		}
		return "pipeline fix is not pushed yet; do not make local follow-up commits"
	case branchsync.StateCustodyReturned:
		return "custody returned; the branch is yours - start a fresh run when ready"
	case branchsync.StateUserOwned:
		return "run ended before the pipeline changed anything; the branch and head are yours and immediately usable"
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

func runAxiSync(cmd *cobra.Command, check, recover, keepLocal bool, bindArchiveRef string) error {
	started := time.Now()
	mode := "apply"
	switch {
	case bindArchiveRef != "":
		mode = "bind_archive"
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
	case bindArchiveRef != "":
		state = service.BindRecoveryArchive(cmd.Context(), bindArchiveRef)
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
	if state.Safety == "blocked_pipeline_owned_recoverable" && (state.Recovery == nil || !state.Recovery.KeepLocal) {
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
	if bindArchiveRef != "" {
		successful = verifiedArchiveRecovery(state)
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

func verifiedArchiveRecovery(state branchsync.State) bool {
	return state.Recovery != nil && state.Recovery.Source == "bound_archive" && state.Recovery.Proof == "verified" &&
		state.Recovery.KeepLocal && state.NextAction != nil && state.NextAction.Code == "recover_custody" &&
		state.NextAction.Command == "no-mistakes axi sync --recover --keep-local"
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
	// A branch released by cancellation is the operator's with nothing to
	// synchronize or recover; it must never surface as a blocked exit.
	if state.State == branchsync.StateUserOwned {
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
	if state.Recovery != nil {
		fields = append(fields, toON.Field{Key: "recovery", Value: toON.NewObject(
			toON.Field{Key: "source", Value: state.Recovery.Source},
			toON.Field{Key: "repository", Value: state.Recovery.RepositoryID},
			toON.Field{Key: "run", Value: state.Recovery.RunID},
			toON.Field{Key: "branch", Value: state.Recovery.Branch},
			toON.Field{Key: "required_head", Value: state.Recovery.RequiredHead},
			toON.Field{Key: "preserved_head", Value: state.Recovery.PreservedHead},
			toON.Field{Key: "archive_ref", Value: state.Recovery.ArchiveRef},
			toON.Field{Key: "keep_local", Value: state.Recovery.KeepLocal},
			toON.Field{Key: "proof", Value: state.Recovery.Proof},
		)})
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
		branchsync.StateTargetChanged, branchsync.StateCustodyReturned, branchsync.StateUserOwned:
		return true
	default:
		return false
	}
}
