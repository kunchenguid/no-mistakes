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
