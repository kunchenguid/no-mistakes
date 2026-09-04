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

func TestMissingPreservedHeadStatusShowsKeepLocalRecoveryCommand(t *testing.T) {
	state := branchsync.State{
		State:  branchsync.StatePipelineOwned,
		Safety: "blocked_recover_preserved_head_missing",
		NextAction: &branchsync.NextAction{
			Code:    "recover_custody",
			Command: "no-mistakes axi sync --recover --keep-local",
		},
	}

	view := stripANSI(renderLocalBranchStatus(&state, false, 80))
	if !strings.Contains(view, "no-mistakes axi sync --recover --keep-local") {
		t.Fatalf("missing-head status did not show its recovery command:\n%s", view)
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
	m.syncRecover = func(keepLocal bool) branchsync.State {
		if keepLocal {
			t.Fatal("ordinary recovery unexpectedly selected keep-local")
		}
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

func TestArchiveRecoveryConfirmationUsesOnlyGuardedKeepLocalAction(t *testing.T) {
	run := &ipc.RunInfo{ID: "run-archive", Branch: "feature", Status: types.RunCancelled}
	m := NewModel("socket", nil, run)
	stranded := branchsync.State{
		State: branchsync.StatePipelineOwned, Relation: branchsync.RelationDiverged, Safety: "blocked_pipeline_owned_recoverable",
		Local:    branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline: branchsync.PipelineState{RunID: run.ID, Status: "cancelled", Phase: "pre_push", CurrentHead: strings.Repeat("c", 40)},
		Recovery: &branchsync.RecoveryEvidence{
			Source: "bound_archive", RepositoryID: "repo-1", RunID: run.ID, Branch: "feature",
			RequiredHead: strings.Repeat("a", 40), PreservedHead: strings.Repeat("c", 40),
			ArchiveRef: "refs/heads/archive/run-archive", KeepLocal: true, Proof: "verified",
		},
		NextAction: &branchsync.NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover --keep-local"},
	}
	m.branchSync = &stranded
	called := false
	m.syncRecover = func(keepLocal bool) branchsync.State {
		called = true
		if !keepLocal {
			t.Fatal("archive recovery did not use keep-local")
		}
		recovered := stranded
		recovered.State = branchsync.StateSynchronized
		recovered.Safety = "already_synchronized"
		recovered.Recovered = true
		recovered.Changed = false
		return recovered
	}

	view := stripANSI(renderLocalBranchStatus(m.branchSync, false, 80))
	for _, want := range []string{"verified archive", "exact required local head", "u recover custody"} {
		if !strings.Contains(view, want) {
			t.Errorf("archive status missing %q:\n%s", want, view)
		}
	}
	next, cmd := m.handleKey(keyMsg("u"))
	m = next.(Model)
	if cmd != nil || !m.recoverConfirm {
		t.Fatal("archive recovery did not open confirmation")
	}
	confirmation := stripANSI(m.View())
	for _, want := range []string{"never selects or replays the archive", stranded.Recovery.ArchiveRef, stranded.Recovery.RequiredHead} {
		if !strings.Contains(confirmation, want) {
			t.Errorf("archive confirmation missing %q:\n%s", want, confirmation)
		}
	}
	next, cmd = m.handleKey(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil || called {
		t.Fatal("archive recovery did not wait for its command")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !called || !m.branchSync.Recovered || m.branchSync.Changed {
		t.Fatalf("archive recovery result = %#v", m.branchSync)
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
	m.syncRecover = func(bool) branchsync.State {
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
