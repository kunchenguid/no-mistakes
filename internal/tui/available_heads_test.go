package tui

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAvailableHeadsRecoveryKeepsLocalAfterConfirmation(t *testing.T) {
	m := NewModel("socket", nil, &ipc.RunInfo{ID: "failed-run", Branch: "feature", Status: types.RunFailed})
	state := branchsync.State{
		State: branchsync.StatePipelineOwned, Safety: "blocked_pipeline_owned_recoverable", Relation: branchsync.RelationDiverged,
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "failed-run", Status: "failed", CurrentHead: strings.Repeat("b", 40)},
		Recovery:   &branchsync.RecoveryEvidence{Source: "available_heads", RequiredHead: strings.Repeat("a", 40), PreservedHead: strings.Repeat("b", 40), KeepLocal: true, Proof: "verified"},
		NextAction: &branchsync.NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover --keep-local"},
	}
	m.branchSync = &state
	called := false
	m.syncRecover = func(keepLocal bool) branchsync.State {
		if !keepLocal {
			t.Fatal("available-head custody recovery did not keep local")
		}
		called = true
		result := state
		result.State = branchsync.StateCustodyReturned
		result.Recovered = true
		return result
	}
	view := stripANSI(renderLocalBranchStatus(&state, false, 100))
	if !strings.Contains(view, "no commits are discarded or merged") || strings.Contains(view, "archive") {
		t.Fatalf("available-head status is misleading:\n%s", view)
	}
	next, cmd := m.handleKey(keyMsg("u"))
	m = next.(Model)
	if cmd != nil || !m.recoverConfirm || called {
		t.Fatal("recovery bypassed confirmation")
	}
	confirmation := stripANSI(m.View())
	for _, want := range []string{"No commits are discarded or merged", state.Local.Head, state.Pipeline.CurrentHead, "fresh validation run"} {
		if !strings.Contains(confirmation, want) {
			t.Errorf("confirmation missing %q:\n%s", want, confirmation)
		}
	}
	next, cmd = m.handleKey(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil || called {
		t.Fatal("recovery did not wait for command")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !called || !m.branchSync.Recovered || m.branchSync.Changed {
		t.Fatalf("keep-local recovery result = %#v", m.branchSync)
	}
}
