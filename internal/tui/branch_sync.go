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
			if recoverableBranchSync(state) {
				message = "Run ended without publishing its pipeline commits; they are preserved in the local gate. Recover custody to take the branch back, or rerun to resume validation."
				footer = "u recover custody"
			} else if settleableBranchSync(state) {
				message = "Run ended terminally and its recorded pipeline head can no longer be verified, so there is nothing to recover. Settle custody at your current head to take the branch back."
				footer = "u settle custody at local head"
			} else {
				message = "Local branch unchanged; the pipeline fix is not pushed yet. Do not make follow-up commits."
			}
		case branchsync.StatePushInProgress:
			message = "Publishing the pipeline head; synchronization is unavailable."
		case branchsync.StateBehind:
			if state.Safety == branchsync.SafetySafeFastForward {
				message = "Local branch is strictly behind the exact live pipeline-pushed head."
			} else {
				message = "Local branch is behind the pipeline-pushed head. Safe fast-forward available after refresh."
				footer = "u sync branch"
			}
		case branchsync.StateDirty:
			message = "Local branch is behind, but the worktree has uncommitted or in-progress changes."
		case branchsync.StateDiverged:
			if state.Safety == branchsync.SafetySafeEquivalentAdvance {
				message = "Local branch diverged, but its changes are represented in the live pipeline head. Sync will preserve the old head before advancing."
			} else if state.NextAction != nil && state.NextAction.Code == "sync" {
				message = "Local branch diverged, but the pipeline head may contain equivalent work. Refresh to verify before syncing."
				footer = "u sync branch"
			} else {
				message = "Local branch and pipeline-pushed head have diverged. No automatic reconciliation is allowed."
			}
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
		case branchsync.StateCustodyReturned:
			message = "Custody returned; the branch is yours. Start a fresh run when ready."
		case branchsync.StateUserOwned:
			message = "Run ended before the pipeline changed anything; the branch and head are yours and immediately usable."
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

// recoverableBranchSync reports whether the state is the stranded terminal
// pipeline_owned custody state that the guarded recovery action can end.
func recoverableBranchSync(state *branchsync.State) bool {
	return state != nil && state.State == branchsync.StatePipelineOwned && state.Safety == "blocked_pipeline_owned_recoverable"
}

// settleableBranchSync reports whether the state is the SELF-INCONSISTENT
// terminal custody record whose only exit is the keep-local settlement (#824).
//
// It keys on the advertised next action rather than on a safety code, and that
// is deliberate. A self-inconsistent record reaches the TUI under whichever
// safety code described how it failed to verify - blocked_recover_preserved_
// head_missing is only the commonest - so a safety-code list here would drift
// out of agreement with the branchsync predicate that decides whether the
// settlement can actually complete. Keying on the action the state machine
// itself advertises is what makes this exactly the road the CLI offers: the
// service never names return_custody_keep_local for a record the settlement
// would only refuse, and the TUI must never offer one it does not.
func settleableBranchSync(state *branchsync.State) bool {
	return state != nil && state.State == branchsync.StatePipelineOwned &&
		state.NextAction != nil && state.NextAction.Code == "return_custody_keep_local"
}

func renderRecoverConfirmation(state branchsync.State, width int) string {
	if width < 40 {
		width = 80
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The run ended %s without publishing its pipeline commits. Recovery returns\n", state.Pipeline.Status)
	fmt.Fprintf(&b, "custody by fast-forwarding a clean behind worktree, or by adopting a diverged\n")
	fmt.Fprintf(&b, "preserved head only when it is proven to carry every local change.\n\n")
	fmt.Fprintf(&b, "Local branch:   %s\n", state.Local.Branch)
	fmt.Fprintf(&b, "Local HEAD:     %s\n", state.Local.Head)
	fmt.Fprintf(&b, "Preserved HEAD: %s\n\n", state.Pipeline.CurrentHead)
	b.WriteString("Dirty worktrees and divergence that cannot be proven contained refuse without changes; `no-mistakes sync --recover --keep-local` keeps the current head instead. `no-mistakes rerun` resumes validation.")
	return renderBoxWithFooter("Confirm custody recovery", b.String(), width, "u/enter recover  ·  esc cancel")
}

// renderSettleConfirmation is deliberately NOT renderRecoverConfirmation with
// different words. Recovery takes the preserved pipeline head; settlement
// keeps the local head and moves the gate to it, abandoning a recorded head
// that can no longer be verified. The CLI makes that an explicit --keep-local
// choice, so the TUI has to state the same consequence before asking for it.
func renderSettleConfirmation(state branchsync.State, width int) string {
	if width < 40 {
		width = 80
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The run ended %s and its recorded pipeline head can no longer be verified,\n", state.Pipeline.Status)
	fmt.Fprintf(&b, "so there is nothing to recover. Settling returns custody at the head you\n")
	fmt.Fprintf(&b, "already have and points the gate branch at it.\n\n")
	fmt.Fprintf(&b, "Local branch:   %s\n", state.Local.Branch)
	fmt.Fprintf(&b, "Kept HEAD:      %s\n", state.Local.Head)
	fmt.Fprintf(&b, "Recorded HEAD:  %s (unverifiable)\n\n", state.Pipeline.CurrentHead)
	b.WriteString("Your worktree is never touched. Any still-reachable copy of the recorded head is anchored first, and the settlement refuses rather than proceeding if one exists and cannot be anchored. The gate moves only by compare-and-swap, so a concurrent gate push wins. This is `no-mistakes sync --recover --keep-local`.")
	return renderBoxWithFooter("Confirm custody settlement at local head", b.String(), width, "u/enter settle  ·  esc cancel")
}

func renderSyncConfirmation(state branchsync.State, width int) string {
	if width < 40 {
		width = 80
	}
	var b strings.Builder
	if state.Safety == branchsync.SafetySafeEquivalentAdvance {
		fmt.Fprintf(&b, "Only this clean checked-out branch can advance to an equivalent live pipeline head.\n")
		fmt.Fprintf(&b, "The current local head is anchored before the branch moves.\n\n")
	} else {
		fmt.Fprintf(&b, "Only this clean checked-out branch can advance by a strict fast-forward.\n\n")
	}
	fmt.Fprintf(&b, "Local branch: %s\n", state.Local.Branch)
	fmt.Fprintf(&b, "Local HEAD:   %s\n", state.Local.Head)
	fmt.Fprintf(&b, "Target HEAD:  %s\n", state.Pipeline.PushedHead)
	fmt.Fprintf(&b, "Target:       %s %s (%s)\n", state.Target.Remote, state.Target.Ref, state.Target.Kind)
	fmt.Fprintf(&b, "Worktree:     clean\n\n")
	if state.Safety == branchsync.SafetySafeEquivalentAdvance {
		b.WriteString("No stash, merge commit, rebase, force push, branch switch, or remote update can occur.")
	} else {
		b.WriteString("No reset, stash, merge commit, rebase, force push, branch switch, or remote update can occur.")
	}
	return renderBoxWithFooter("Confirm local branch sync", b.String(), width, "u/enter apply  ·  esc cancel")
}
