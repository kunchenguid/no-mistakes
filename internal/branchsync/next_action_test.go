package branchsync

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// nextActionCases is the single description of the next-action vocabulary: the
// exact wire string each code ships as, the command it is paired with, and
// whether it counts as taking the pipeline's commits. Every property of a code
// is asserted from this one table, checked for completeness against the
// registry, so a new code cannot land in some of the assertions and miss the
// others.
var nextActionCases = map[NextActionCode]struct {
	wire            string
	action          *NextAction
	command         string
	synchronization bool
}{
	NextActionSync:                        {"sync", syncAction(), "no-mistakes axi sync", true},
	NextActionCheckSync:                   {"check_sync", checkSyncAction(), "no-mistakes axi sync --check", true},
	NextActionRecoverCustody:              {"recover_custody", recoverCustodyAction(), "no-mistakes axi sync --recover", true},
	NextActionRetry:                       {"retry", retryAction(), "no-mistakes axi sync --check", true},
	NextActionRunPipeline:                 {"run_pipeline", runPipelineAction(), `no-mistakes axi run --intent "<what the user set out to accomplish>"`, false},
	NextActionInspectWorktree:             {"inspect_worktree", inspectWorktreeAction(), "git status", false},
	NextActionContinueActiveRun:           {"continue_active_run", continueActiveRunAction(), "no-mistakes axi status", false},
	NextActionInspectAndReconcileManually: {"inspect_and_reconcile_manually", reconcileManuallyAction("refs/no-mistakes/x"), "git log --oneline --left-right HEAD...refs/no-mistakes/x", false},
}

// The codes ship verbatim to agents in skills/no-mistakes/sync-recovery.md and
// in docs/src/content/docs/reference/cli.md, so renaming one breaks every agent
// and document matching on it. The wire strings, the command each code is
// paired with by its constructor, and the synchronization classification are
// all pinned here against the complete vocabulary.
func TestNextActionVocabulary(t *testing.T) {
	codes := allNextActionCodes()
	if len(nextActionCases) != len(codes) {
		t.Fatalf("described %d codes, want all %d", len(nextActionCases), len(codes))
	}
	for _, code := range codes {
		want, ok := nextActionCases[code]
		if !ok {
			t.Errorf("code %q is undescribed; pin its wire value, command, and whether it takes the pipeline's commits", code)
			continue
		}
		t.Run(string(code), func(t *testing.T) {
			if string(code) != want.wire {
				t.Errorf("code = %q, want %q", string(code), want.wire)
			}
			if want.action == nil {
				t.Fatalf("code %q built no action", code)
			}
			if want.action.Code != code {
				t.Errorf("constructed Code = %q, want %q", want.action.Code, code)
			}
			if got := want.action.Command; got != want.command {
				t.Errorf("Command = %q, want %q", got, want.command)
			}
			if got := want.action.IsSynchronization(); got != want.synchronization {
				t.Errorf("IsSynchronization() = %v, want %v", got, want.synchronization)
			}
		})
	}
	if (*NextAction)(nil).IsSynchronization() {
		t.Error("a missing next action must not read as a synchronization action")
	}
}

// A code that was never registered has no command to ship, so it must be
// unable to produce an action at all rather than reaching an agent paired with
// whatever a call site happened to type next to it.
func TestUnregisteredNextActionCodeBuildsNoAction(t *testing.T) {
	if action := actionFor(NextActionCode("not_minted")); action != nil {
		t.Fatalf("unregistered code built %#v", action)
	}
	if action := actionFor(""); action != nil {
		t.Fatalf("empty code built %#v", action)
	}
	if action := reconcileManuallyAction("refs/no-mistakes/x"); action == nil || !strings.HasSuffix(action.Command, "refs/no-mistakes/x") {
		t.Fatalf("parameterized builder did not append its ref: %#v", action)
	}
}

// assertRegisteredNextAction is what makes the registry load-bearing rather
// than decorative: whatever an inspection or recovery decides, the action it
// hands its caller has to be the code that outcome owes an agent, carrying that
// code's registered command. An empty want means the outcome must offer none,
// so a silently dropped action fails here instead of passing as "no action".
func assertRegisteredNextAction(t *testing.T, context string, state State, want NextActionCode) {
	t.Helper()
	action := state.NextAction
	if want == "" {
		if action != nil {
			t.Fatalf("%s: expected no next action, got %#v", context, action)
		}
		return
	}
	if action == nil {
		t.Fatalf("%s: expected next action %q, got none (state %s, safety %s)", context, want, state.State, state.Safety)
	}
	if action.Code != want {
		t.Fatalf("%s: next action code = %q, want %q", context, action.Code, want)
	}
	command, ok := nextActionCommands[action.Code]
	if !ok {
		t.Fatalf("%s: next action code %q is not in the registered vocabulary", context, action.Code)
	}
	if !strings.HasPrefix(action.Command, command) {
		t.Fatalf("%s: code %q shipped command %q, registry pairs it with %q", context, action.Code, action.Command, command)
	}
}

// Every reachable outcome of the real inspection and recovery surfaces must
// carry a registered code. This is the executable half of exhaustiveness: an
// action hand-written at a call site compiles, but fails here.
func TestSyncOutcomesCarryRegisteredNextActionCodes(t *testing.T) {
	t.Parallel()

	t.Run("behind_pipeline_head", func(t *testing.T) {
		t.Parallel()
		f := newSyncFixture(t)
		assertRegisteredNextAction(t, "behind", f.service.Refresh(f.ctx), NextActionSync)
	})

	t.Run("dirty_worktree", func(t *testing.T) {
		t.Parallel()
		f := newSyncFixture(t)
		mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n")
		assertRegisteredNextAction(t, "dirty", f.service.Refresh(f.ctx), NextActionInspectWorktree)
	})

	// An active run that already published an exact binding still leaves the
	// worktree free to take those commits, so the offer is the sync itself.
	t.Run("active_run_with_published_binding", func(t *testing.T) {
		t.Parallel()
		f := newSyncFixture(t)
		if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		assertRegisteredNextAction(t, "active run", f.service.Refresh(f.ctx), NextActionSync)
	})

	// A run that currently owns a push is the case where the only thing an
	// agent can do is wait for the run it already has.
	t.Run("push_in_progress", func(t *testing.T) {
		t.Parallel()
		f := newSyncFixture(t)
		if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		if err := f.db.SetRunPushActive(f.run.ID, true); err != nil {
			t.Fatal(err)
		}
		assertRegisteredNextAction(t, "push in progress", f.service.Refresh(f.ctx), NextActionContinueActiveRun)
	})

	t.Run("terminal_unpublished_custody", func(t *testing.T) {
		t.Parallel()
		f := newRecoverFixture(t, types.RunCancelled)
		assertRegisteredNextAction(t, "custody", f.service.Refresh(f.ctx), NextActionRecoverCustody)
	})

	t.Run("recover_dirty_refusal", func(t *testing.T) {
		t.Parallel()
		f := newRecoverFixture(t, types.RunCancelled)
		mustWrite(t, filepath.Join(f.local, "feature.txt"), "operator edit\n")
		assertRegisteredNextAction(t, "recover refusal", f.service.Recover(f.ctx, false), NextActionInspectWorktree)
	})

	t.Run("apply", func(t *testing.T) {
		t.Parallel()
		f := newSyncFixture(t)
		assertRegisteredNextAction(t, "apply", f.service.Apply(f.ctx), "")
	})
}
