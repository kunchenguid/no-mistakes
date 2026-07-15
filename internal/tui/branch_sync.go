package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func renderLocalBranchStatus(state *branchsync.State, refreshing bool, width int) string {
	if state == nil {
		return ""
	}
	message := ""
	footer := ""
	if refreshing {
		message = "Refreshing the exact configured push target..."
	} else {
		switch state.State {
		case branchsync.StatePipelineOwned:
			message = "Local branch unchanged; the pipeline fix is not pushed yet. Do not make follow-up commits."
		case branchsync.StatePushInProgress:
			message = "Publishing the pipeline head; synchronization is unavailable."
		case branchsync.StateBehind:
			if state.Safety == "safe_fast_forward" {
				message = "Local branch is strictly behind the exact live pipeline-pushed head."
			} else {
				message = "Local branch is behind the pipeline-pushed head. Safe fast-forward available after refresh."
				footer = "u sync branch"
			}
		case branchsync.StateDirty:
			message = "Local branch is behind, but the worktree has uncommitted or in-progress changes."
		case branchsync.StateDiverged:
			message = "Local branch and pipeline-pushed head have diverged. No automatic reconciliation is allowed."
		case branchsync.StateLocalAhead:
			message = "Local branch contains the pushed head plus new commits. Start a fresh pipeline run."
		case branchsync.StateMergedRemoteRetained:
			message = "PR merged; the feature branch is retired. Local branch was not changed."
		case branchsync.StateMergedRemoteRemoved:
			message = "PR merged and the remote feature branch was removed. Local branch was not changed."
		case branchsync.StateClosed:
			message = "PR closed; the feature branch is retired. Local branch was not changed."
		case branchsync.StateTargetChanged:
			message = "The configured push target changed after the pipeline push. Synchronization is blocked."
		default:
			return ""
		}
	}
	if width < 40 {
		width = 80
	}
	return renderBoxWithFooter("Local branch", message, width, footer)
}

func trackTUISyncAttempt(mode string, state branchsync.State, result string, started time.Time) {
	telemetry.Track("command", telemetry.Fields{
		"command":      "tui-sync",
		"surface":      "tui",
		"mode":         mode,
		"status":       result,
		"result":       result,
		"state_before": boundedTUISyncValue(state.State),
		"relation":     boundedTUISyncValue(state.Relation),
		"target_kind":  boundedTUISyncValue(state.Target.Kind),
		"run_phase":    boundedTUISyncValue(state.Pipeline.Phase),
		"pr_state":     boundedTUISyncValue(state.PRState),
		"reason":       boundedTUISyncValue(state.Safety),
		"dirty":        !state.Local.Clean && state.Local.Head != "",
		"duration_ms":  time.Since(started).Milliseconds(),
	})
}

func boundedTUISyncValue(value string) string {
	if value == "" || len(value) > 64 {
		return "unknown"
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && r != '_' {
			return "unknown"
		}
	}
	return value
}

func renderSyncConfirmation(state branchsync.State, width int) string {
	if width < 40 {
		width = 80
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Only this clean checked-out branch can advance by a strict fast-forward.\n\n")
	fmt.Fprintf(&b, "Local branch: %s\n", state.Local.Branch)
	fmt.Fprintf(&b, "Local HEAD:   %s\n", state.Local.Head)
	fmt.Fprintf(&b, "Target HEAD:  %s\n", state.Pipeline.PushedHead)
	fmt.Fprintf(&b, "Target:       %s %s (%s)\n", state.Target.Remote, state.Target.Ref, state.Target.Kind)
	fmt.Fprintf(&b, "Worktree:     clean\n\n")
	b.WriteString("No reset, stash, merge commit, rebase, force push, branch switch, or remote update can occur.")
	return renderBoxWithFooter("Confirm local branch sync", b.String(), width, "u/enter apply  ·  esc cancel")
}
