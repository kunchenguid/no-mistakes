package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBranchSyncActionRefreshesBeforeConfirmationAndAppliesThroughSharedPath(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunRunning}
	m := NewModel("socket", nil, run)
	cached := branchsync.State{
		State: branchsync.StateBehind, Relation: branchsync.RelationBehind, Safety: "refresh_required",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", PushedHead: strings.Repeat("b", 40)},
		Target:     branchsync.TargetState{Kind: "fork", Remote: "fork", Ref: "refs/heads/feature"},
		NextAction: &branchsync.NextAction{Code: "sync", Command: "no-mistakes axi sync"},
	}
	m.branchSync = &cached
	refreshCalls := 0
	applyCalls := 0
	m.syncRefresh = func() branchsync.State {
		refreshCalls++
		fresh := cached
		fresh.Safety = "safe_fast_forward"
		fresh.Remote.Freshness = "live"
		return fresh
	}
	m.syncApply = func() branchsync.State {
		applyCalls++
		applied := cached
		applied.State = branchsync.StateSynchronized
		applied.Safety = "already_synchronized"
		applied.Relation = branchsync.RelationEqual
		applied.Changed = true
		applied.Local.Head = applied.Pipeline.PushedHead
		return applied
	}

	nextModel, cmd := m.handleKey(keyMsg("u"))
	m = nextModel.(Model)
	if cmd == nil || !m.syncRefreshing || m.syncConfirm || refreshCalls != 0 {
		t.Fatalf("refresh was not scheduled explicitly: %#v", m)
	}
	msg := cmd()
	next, _ := m.Update(msg)
	m = next.(Model)
	if refreshCalls != 1 || !m.syncConfirm || m.branchSync.Remote.Freshness != "live" || applyCalls != 0 {
		t.Fatalf("fresh confirmation state = %#v, refresh=%d apply=%d", m.branchSync, refreshCalls, applyCalls)
	}
	plain := stripANSI(m.View())
	for _, want := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40), "refs/heads/feature", "strict fast-forward", "u/enter apply"} {
		if !strings.Contains(plain, want) {
			t.Errorf("confirmation missing %q:\n%s", want, plain)
		}
	}

	nextModel, cmd = m.handleKey(keyMsg("enter"))
	m = nextModel.(Model)
	if cmd == nil || applyCalls != 0 {
		t.Fatal("apply did not wait for async command")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if applyCalls != 1 || m.syncConfirm || m.branchSync.State != branchsync.StateSynchronized || !m.branchSync.Changed {
		t.Fatalf("apply result = %#v", m.branchSync)
	}
}

func TestLocalBranchStatusIsCompactAndOnlyOffersEligibleAction(t *testing.T) {
	state := branchsync.State{State: branchsync.StateBehind, Safety: "refresh_required"}
	view := stripANSI(renderLocalBranchStatus(&state, false, 80))
	if !strings.Contains(view, "Safe fast-forward available after refresh") || !strings.Contains(view, "u sync branch") {
		t.Fatalf("behind view:\n%s", view)
	}
	state.State = branchsync.StateDiverged
	view = stripANSI(renderLocalBranchStatus(&state, false, 80))
	if !strings.Contains(view, "diverged") || strings.Contains(view, "u sync branch") {
		t.Fatalf("diverged view:\n%s", view)
	}
	state.NextAction = &branchsync.NextAction{Code: "sync", Command: "no-mistakes axi sync"}
	view = stripANSI(renderLocalBranchStatus(&state, false, 80))
	if !strings.Contains(view, "equivalent work") || !strings.Contains(view, "u sync branch") {
		t.Fatalf("equivalent candidate view:\n%s", view)
	}
	state.Safety = branchsync.SafetySafeEquivalentAdvance
	view = stripANSI(renderLocalBranchStatus(&state, false, 80))
	if !strings.Contains(view, "represented in the live pipeline head") || strings.Contains(view, "No automatic reconciliation") {
		t.Fatalf("equivalent live view:\n%s", view)
	}
}

func TestBranchSyncActionRefreshesEquivalentDivergedBeforeConfirmation(t *testing.T) {
	m := NewModel("socket", nil, &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunRunning})
	cached := branchsync.State{
		State: branchsync.StateDiverged, Relation: branchsync.RelationDiverged, Safety: "refresh_required",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", PushedHead: strings.Repeat("b", 40)},
		Target:     branchsync.TargetState{Kind: "fork", Remote: "fork", Ref: "refs/heads/feature"},
		NextAction: &branchsync.NextAction{Code: "sync", Command: "no-mistakes axi sync"},
	}
	m.branchSync = &cached
	refreshCalls := 0
	m.syncRefresh = func() branchsync.State {
		refreshCalls++
		fresh := cached
		fresh.Safety = branchsync.SafetySafeEquivalentAdvance
		fresh.Remote.Freshness = "live"
		return fresh
	}

	nextModel, cmd := m.handleKey(keyMsg("u"))
	m = nextModel.(Model)
	if cmd == nil || !m.syncRefreshing || m.syncConfirm {
		t.Fatalf("refresh was not scheduled: %#v", m)
	}
	next, _ := m.Update(cmd())
	m = next.(Model)
	if refreshCalls != 1 || !m.syncConfirm || m.branchSync.Safety != branchsync.SafetySafeEquivalentAdvance {
		t.Fatalf("fresh equivalent confirmation state = %#v, refresh=%d", m.branchSync, refreshCalls)
	}
	plain := stripANSI(m.View())
	for _, want := range []string{"equivalent live pipeline head", "anchored before the branch moves", "u/enter apply"} {
		if !strings.Contains(plain, want) {
			t.Errorf("confirmation missing %q:\n%s", want, plain)
		}
	}
}

func TestFreshAttachUsesPersistedCIReadyAndCompletedSubscriptionCloseIsNotError(t *testing.T) {
	ciRun := &ipc.RunInfo{ID: "run-ci", Branch: "feature", Status: types.RunRunning, CIReady: true, Steps: []ipc.StepResultInfo{{RunID: "run-ci", StepName: types.StepCI, Status: types.StepStatusRunning}}}
	m := NewModel("socket", nil, ciRun)
	view := stripANSI(renderCIViewWithSelection(ciRun, m.steps, "", nil, 80, 20, 0, nil))
	if !strings.Contains(view, "Checks passed") || strings.Contains(view, "Monitoring CI checks") {
		t.Fatalf("fresh CI view ignored persisted readiness:\n%s", view)
	}

	completed := NewModel("socket", nil, &ipc.RunInfo{ID: "done", Status: types.RunCompleted})
	closed := make(chan ipc.Event)
	close(closed)
	next, cmd := completed.Update(connectedMsg{events: closed, cancelSub: func() {}, subscriptionID: completed.subscriptionID})
	completed = next.(Model)
	if cmd != nil || completed.err != nil {
		t.Fatalf("completed attach scheduled closed-stream error: cmd=%v err=%v", cmd != nil, completed.err)
	}
}

func TestSyncConfirmationEscapeNeverApplies(t *testing.T) {
	m := NewModel("socket", nil, &ipc.RunInfo{ID: "run", Status: types.RunRunning})
	m.syncConfirm = true
	calls := 0
	m.syncApply = func() branchsync.State { calls++; return branchsync.State{} }
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if cmd != nil || m.syncConfirm || calls != 0 {
		t.Fatalf("escape applied sync: confirm=%v calls=%d", m.syncConfirm, calls)
	}
}

// TestRecoverableCustodyActionFlowsThroughConfirmationAndRecoverService covers
// the TUI half of the guarded custody recovery: a terminal pre-push
// pipeline_owned state renders the recovery offer, `u` opens an explicit
// confirmation instead of acting, and `enter` routes through the shared
// branchsync recovery service exactly once.
func TestRecoverableCustodyActionFlowsThroughConfirmationAndRecoverService(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunCancelled}
	m := NewModel("socket", nil, run)
	stranded := branchsync.State{
		State: branchsync.StatePipelineOwned, Relation: branchsync.RelationUnknown, Safety: "blocked_pipeline_owned_recoverable",
		Local:    branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline: branchsync.PipelineState{RunID: "run-1", Status: "cancelled", Phase: "pre_push", CurrentHead: strings.Repeat("c", 40)},
	}
	m.branchSync = &stranded

	view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	for _, want := range []string{"preserved in the local gate", "Recover custody", "u recover custody"} {
		if !strings.Contains(view, want) {
			t.Errorf("recoverable status missing %q:\n%s", want, view)
		}
	}

	recoverCalls := 0
	m.syncRecover = func() branchsync.State {
		recoverCalls++
		recovered := stranded
		recovered.State = branchsync.StateCustodyReturned
		recovered.Safety = "custody_returned"
		recovered.Relation = branchsync.RelationEqual
		recovered.Recovered = true
		recovered.Changed = true
		recovered.Local.Head = recovered.Pipeline.CurrentHead
		return recovered
	}

	nextModel, cmd := m.handleKey(keyMsg("u"))
	m = nextModel.(Model)
	if cmd != nil || !m.recoverConfirm || recoverCalls != 0 {
		t.Fatalf("u must open confirmation without acting: confirm=%v calls=%d", m.recoverConfirm, recoverCalls)
	}
	plain := stripANSI(m.View())
	for _, want := range []string{"custody", strings.Repeat("a", 40), strings.Repeat("c", 40), "u/enter recover", "--keep-local", "rerun"} {
		if !strings.Contains(plain, want) {
			t.Errorf("recover confirmation missing %q:\n%s", want, plain)
		}
	}

	nextModel, cmd = m.handleKey(keyMsg("enter"))
	m = nextModel.(Model)
	if cmd == nil || recoverCalls != 0 {
		t.Fatal("recover did not wait for async command")
	}
	next, _ := m.Update(cmd())
	m = next.(Model)
	if recoverCalls != 1 || m.recoverConfirm || m.branchSync.State != branchsync.StateCustodyReturned || !m.branchSync.Recovered {
		t.Fatalf("recover result = %#v", m.branchSync)
	}
	if m.err != nil {
		t.Fatalf("successful recovery left an error: %v", m.err)
	}

	returned := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	if !strings.Contains(returned, "Custody returned") {
		t.Fatalf("custody_returned status:\n%s", returned)
	}
}

// TestActivePipelineOwnedStateOffersNoRecoveryAction pins that the recovery
// affordance never appears while the owning run is still active.
func TestActivePipelineOwnedStateOffersNoRecoveryAction(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunRunning}
	m := NewModel("socket", nil, run)
	m.branchSync = &branchsync.State{
		State: branchsync.StatePipelineOwned, Safety: "blocked_pipeline_owned",
		Local:    branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline: branchsync.PipelineState{RunID: "run-1", Status: "running", Phase: "pre_push"},
	}
	m.syncRecover = func() branchsync.State {
		t.Fatal("recover service must not be reachable for an active run")
		return branchsync.State{}
	}
	view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	if strings.Contains(view, "recover") || !strings.Contains(view, "Do not make follow-up commits") {
		t.Fatalf("active pipeline_owned view:\n%s", view)
	}
	nextModel, cmd := m.handleKey(keyMsg("u"))
	m = nextModel.(Model)
	if cmd != nil || m.recoverConfirm || m.syncConfirm {
		t.Fatalf("u acted on an active pipeline_owned state: %#v", m)
	}
}

// TestWedgedCustodyRecordReachesTheSameSettlementExitAsTheCLI is the TUI half
// of issue #824. A self-inconsistent custody record - terminal run, recorded
// pipeline head no longer verifiable - carries the settlement next action
// rather than blocked_pipeline_owned_recoverable, so keying the u affordance on
// the recoverable safety code alone left the TUI as the one operator surface
// with no exit at all: the CLI could settle the record and the TUI could only
// describe it. The exit must be the same one, and it must be an explicit
// choice, because settling KEEPS the local head instead of taking the
// preserved one.
func TestWedgedCustodyRecordReachesTheSameSettlementExitAsTheCLI(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunFailed}
	m := NewModel("socket", nil, run)
	wedged := branchsync.State{
		State: branchsync.StatePipelineOwned, Relation: branchsync.RelationUnknown,
		Safety:     "blocked_recover_preserved_head_missing",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", Status: "failed", Phase: "pre_push", CurrentHead: strings.Repeat("c", 40)},
		NextAction: &branchsync.NextAction{Code: "return_custody_keep_local", Command: "no-mistakes axi sync --recover --keep-local"},
	}
	m.branchSync = &wedged

	// The dead end was visible here first: the status line described the block
	// and offered no key at all.
	view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	for _, want := range []string{"can no longer be verified", "u settle custody at local head"} {
		if !strings.Contains(view, want) {
			t.Errorf("wedged status missing %q:\n%s", want, view)
		}
	}

	settleCalls := 0
	recoverCalls := 0
	m.syncRecover = func() branchsync.State {
		recoverCalls++
		t.Error("a wedged record must not take the plain recovery path")
		return branchsync.State{}
	}
	m.syncSettle = func() branchsync.State {
		settleCalls++
		settled := wedged
		settled.State = branchsync.StateCustodyReturned
		settled.Safety = "custody_returned"
		settled.Relation = branchsync.RelationEqual
		settled.Recovered = true
		settled.NextAction = nil
		return settled
	}

	nextModel, cmd := m.handleKey(keyMsg("u"))
	m = nextModel.(Model)
	if cmd != nil || !m.settleConfirm || settleCalls != 0 {
		t.Fatalf("u must open the settlement confirmation without acting: confirm=%v calls=%d", m.settleConfirm, settleCalls)
	}
	if m.recoverConfirm {
		t.Fatal("wedged record opened the recovery confirmation instead of the settlement one")
	}
	// Settling keeps the local head and abandons an unverifiable recorded one,
	// so the confirmation has to say which head survives before asking.
	plain := stripANSI(m.View())
	for _, want := range []string{"custody", strings.Repeat("a", 40), strings.Repeat("c", 40), "u/enter settle", "--keep-local"} {
		if !strings.Contains(plain, want) {
			t.Errorf("settlement confirmation missing %q:\n%s", want, plain)
		}
	}

	// esc must back out without touching anything.
	escModel, escCmd := m.handleKey(keyMsg("esc"))
	escaped := escModel.(Model)
	if escCmd != nil || escaped.settleConfirm || settleCalls != 0 {
		t.Fatalf("esc did not cancel the settlement cleanly: confirm=%v calls=%d", escaped.settleConfirm, settleCalls)
	}

	nextModel, cmd = m.handleKey(keyMsg("enter"))
	m = nextModel.(Model)
	if cmd == nil || settleCalls != 0 {
		t.Fatal("settlement did not wait for its async command")
	}
	next, _ := m.Update(cmd())
	m = next.(Model)
	if settleCalls != 1 || recoverCalls != 0 {
		t.Fatalf("settlement calls=%d recover calls=%d", settleCalls, recoverCalls)
	}
	if m.settleConfirm || m.branchSync.State != branchsync.StateCustodyReturned || !m.branchSync.Recovered {
		t.Fatalf("settlement result = %#v", m.branchSync)
	}
	if m.err != nil {
		t.Fatalf("successful settlement left an error: %v", m.err)
	}
}

// TestPipelineOwnedStateWithoutASettlementActionOffersNoSettlement keeps the
// TUI from inventing an exit the service did not advertise: the branchsync
// predicate decides where the settlement can actually complete, and a record it
// sent to manual reconciliation must not be offered a key that would only
// refuse - which is the #824 shape in reverse.
func TestPipelineOwnedStateWithoutASettlementActionOffersNoSettlement(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunFailed}
	m := NewModel("socket", nil, run)
	m.branchSync = &branchsync.State{
		State: branchsync.StatePipelineOwned, Safety: "blocked_recover_preserved_head_missing",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", Status: "failed", Phase: "pre_push"},
		NextAction: &branchsync.NextAction{Code: "inspect_and_reconcile_manually", Command: "no-mistakes axi status"},
	}
	m.syncSettle = func() branchsync.State {
		t.Fatal("settlement must not be reachable for a record sent to manual reconciliation")
		return branchsync.State{}
	}
	view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	if strings.Contains(view, "u settle") {
		t.Fatalf("unadvertised settlement offered a key:\n%s", view)
	}
	nextModel, cmd := m.handleKey(keyMsg("u"))
	m = nextModel.(Model)
	if cmd != nil || m.settleConfirm || m.recoverConfirm || m.syncConfirm {
		t.Fatalf("u acted on a record with no advertised settlement: %#v", m)
	}
}

// TestStampFailureOffersTheCompletionTheServiceAdvertised closes the TUI half
// of the complete_custody_return gap. That code is emitted when a custody
// return's Git side already applied and only the database write failed - refs
// changed, custody unrecorded. settleableBranchSync correctly stopped matching
// it (its confirmation claims the recorded head is unverifiable, which is false
// here), but nothing else matched it either, so the TUI rendered the state with
// no exit at all: the #824 dead end one layer down on the operator surface.
func TestStampFailureOffersTheCompletionTheServiceAdvertised(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunFailed}
	m := NewModel("socket", nil, run)
	stamped := branchsync.State{
		State:      branchsync.StatePipelineOwned,
		Safety:     "blocked_recover_stamp_failed",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", Status: "failed", Phase: "pre_push", CurrentHead: strings.Repeat("c", 40)},
		NextAction: &branchsync.NextAction{Code: "complete_custody_return", Command: "no-mistakes axi sync --recover --keep-local"},
	}
	m.branchSync = &stamped

	view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	if !strings.Contains(view, "u complete custody return") {
		t.Errorf("stamp-failure status offered no key at all:\n%s", view)
	}
	if strings.Contains(view, "can no longer be verified") {
		t.Errorf("stamp-failure status reused the settlement's unverifiable-head claim:\n%s", view)
	}

	settleCalls, recoverCalls := 0, 0
	m.syncRecover = func() branchsync.State {
		recoverCalls++
		t.Error("a keep-local completion must not run the plain recovery")
		return branchsync.State{}
	}
	m.syncSettle = func() branchsync.State {
		settleCalls++
		done := stamped
		done.State = branchsync.StateCustodyReturned
		done.Safety = "custody_returned"
		done.Recovered = true
		done.NextAction = nil
		return done
	}

	next, cmd := m.handleKey(keyMsg("u"))
	m = next.(Model)
	if cmd != nil || !m.completeConfirm || settleCalls != 0 {
		t.Fatalf("u must open the completion confirmation without acting: confirm=%v calls=%d", m.completeConfirm, settleCalls)
	}
	if m.settleConfirm || m.recoverConfirm {
		t.Fatal("stamp failure opened the settlement or recovery confirmation instead of the completion one")
	}
	// The box must describe a retry, never the settlement's abandonment of an
	// unverifiable recorded head.
	plain := stripANSI(m.View())
	for _, want := range []string{"could not record the custody return", "u/enter complete", "no-mistakes axi sync --recover --keep-local"} {
		if !strings.Contains(plain, want) {
			t.Errorf("completion confirmation missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "can no longer be verified") {
		t.Errorf("completion confirmation told the operator their recorded head is unverifiable:\n%s", plain)
	}

	escModel, escCmd := m.handleKey(keyMsg("esc"))
	escaped := escModel.(Model)
	if escCmd != nil || escaped.completeConfirm || settleCalls != 0 {
		t.Fatalf("esc did not cancel the completion cleanly: confirm=%v calls=%d", escaped.completeConfirm, settleCalls)
	}

	next, cmd = m.handleKey(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil || settleCalls != 0 {
		t.Fatal("completion did not wait for its async command")
	}
	applied, _ := m.Update(cmd())
	m = applied.(Model)
	if settleCalls != 1 || recoverCalls != 0 {
		t.Fatalf("completion settle calls=%d recover calls=%d", settleCalls, recoverCalls)
	}
	if m.completeConfirm || m.branchSync.State != branchsync.StateCustodyReturned || !m.branchSync.Recovered {
		t.Fatalf("completion result = %#v", m.branchSync)
	}
}

// TestDefaultRecoveryStampFailureCompletesWithThePlainRecovery pins the other
// half of the same contract: the completion runs the recovery the service
// NAMED, so a stamp failure after a default `--recover` must not silently
// become the keep-local settlement, which keeps the opposite head.
func TestDefaultRecoveryStampFailureCompletesWithThePlainRecovery(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunFailed}
	m := NewModel("socket", nil, run)
	m.branchSync = &branchsync.State{
		State:      branchsync.StatePipelineOwned,
		Safety:     "blocked_recover_stamp_failed",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", Status: "failed", Phase: "pre_push"},
		NextAction: &branchsync.NextAction{Code: "complete_custody_return", Command: "no-mistakes axi sync --recover"},
	}
	recoverCalls := 0
	m.syncRecover = func() branchsync.State {
		recoverCalls++
		return branchsync.State{State: branchsync.StateCustodyReturned, Recovered: true}
	}
	m.syncSettle = func() branchsync.State {
		t.Fatal("a default-recovery completion must not run the keep-local settlement")
		return branchsync.State{}
	}

	next, _ := m.handleKey(keyMsg("u"))
	m = next.(Model)
	if !m.completeConfirm {
		t.Fatal("default-recovery stamp failure offered no completion")
	}
	next, cmd := m.handleKey(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("completion produced no command")
	}
	m.Update(cmd())
	if recoverCalls != 1 {
		t.Fatalf("plain recovery calls = %d, want 1", recoverCalls)
	}
}

// TestUnrecognizedCompletionCommandOffersNoKey keeps the completion fail-closed.
// The TUI resolves which recovery to re-run from the advertised command, so a
// command shape it does not recognize must yield no affordance rather than
// guessing - guessing wrong would run the recovery that takes the OTHER head.
func TestUnrecognizedCompletionCommandOffersNoKey(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-1", Branch: "feature", Status: types.RunFailed}
	m := NewModel("socket", nil, run)
	m.branchSync = &branchsync.State{
		State:      branchsync.StatePipelineOwned,
		Safety:     "blocked_recover_stamp_failed",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", Status: "failed", Phase: "pre_push"},
		NextAction: &branchsync.NextAction{Code: "complete_custody_return", Command: "no-mistakes axi sync --recover --some-future-mode"},
	}
	m.syncRecover = func() branchsync.State {
		t.Fatal("an unrecognized completion command must not run a recovery")
		return branchsync.State{}
	}
	m.syncSettle = func() branchsync.State {
		t.Fatal("an unrecognized completion command must not run a settlement")
		return branchsync.State{}
	}
	if view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80)); strings.Contains(view, "u complete") {
		t.Fatalf("unrecognized completion command offered a key:\n%s", view)
	}
	next, cmd := m.handleKey(keyMsg("u"))
	m = next.(Model)
	if cmd != nil || m.completeConfirm || m.settleConfirm || m.recoverConfirm {
		t.Fatalf("u acted on an unrecognized completion command: %#v", m)
	}
}
