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
		Local:    branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline: branchsync.PipelineState{RunID: "run-1", PushedHead: strings.Repeat("b", 40)},
		Target:   branchsync.TargetState{Kind: "fork", Remote: "fork", Ref: "refs/heads/feature"},
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
