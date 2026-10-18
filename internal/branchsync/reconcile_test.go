package branchsync

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newPostPushReconciliationFixture(t *testing.T) (*recoverFixture, string, string) {
	t.Helper()
	f := newRecoverFixture(t, types.RunCancelled)
	pushed := mustRun(t, f.gate, "rev-parse", f.preserved+"^")
	mustRun(t, f.gate, "push", f.remote, pushed+":refs/heads/feature/recover")
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
	}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.local, "file.txt"), "independent local work\n")
	mustRun(t, f.local, "commit", "-am", "local work")
	local := mustRun(t, f.local, "rev-parse", "HEAD")
	return f, local, pushed
}

func reconciliationToken(t *testing.T, f *recoverFixture) string {
	t.Helper()
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.NextAction == nil || state.NextAction.Code != "reconcile_custody" {
		t.Fatalf("no reconciliation offer: %#v", state)
	}
	words := strings.Fields(state.NextAction.Command)
	for i, word := range words {
		if word == "--observed" && i+1 < len(words) {
			return words[i+1]
		}
	}
	t.Fatalf("no observation in command: %s", state.NextAction.Command)
	return ""
}

func TestReconcilePostPushKeepsChosenHeadAndPreservesEveryHistory(t *testing.T) {
	t.Parallel()
	for _, keep := range []string{"local", "pipeline", "pushed"} {
		t.Run(keep, func(t *testing.T) {
			t.Parallel()
			f, local, pushed := newPostPushReconciliationFixture(t)
			state := f.service.InspectCached(f.ctx)
			if state.Pipeline.Phase != "post_push" || state.Relation != RelationUnknown {
				t.Fatalf("initial phase/relation = %#v", state)
			}
			token := reconciliationToken(t, f)
			if again := reconciliationToken(t, f); again != token {
				t.Fatal("read-only inspection changed the observation")
			}
			chosen := map[string]string{"local": local, "pipeline": f.preserved, "pushed": pushed}[keep]
			result := f.service.Reconcile(f.ctx, keep, token)
			if !result.Recovered || result.Changed != (chosen != local) || !f.custodyReturned() {
				t.Fatalf("reconciliation failed: %#v", result)
			}
			for _, dir := range []string{f.local, f.gate} {
				if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/recover"); got != chosen {
					t.Fatalf("branch = %s, want %s", got, chosen)
				}
				for _, head := range []string{local, f.preserved, pushed, f.submitted} {
					if got := mustRun(t, dir, "rev-parse", reconciliationRef(f.run.ID, head)); got != head {
						t.Fatalf("preserved %s as %s", head, got)
					}
				}
				if got := mustRun(t, dir, "rev-parse", custody.RecoveryRef(f.run.ID)); got != f.preserved {
					t.Fatalf("canonical recovery ref = %s", got)
				}
			}
			if got := mustRun(t, f.local, "status", "--porcelain"); got != "" {
				t.Fatalf("worktree was not updated cleanly: %s", got)
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != pushed {
				t.Fatalf("remote changed to %s", got)
			}
			run, err := f.db.GetRun(f.run.ID)
			if err != nil || run.HeadSHA != f.preserved || ptr(run.LastPushedSHA) != pushed || value(run.PushGeneration) != 1 {
				t.Fatalf("reconciliation changed publication history: %#v, %v", run, err)
			}
		})
	}
}

func TestReconcilePostPushRefusesChangedObservationWithoutMutation(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*testing.T, *recoverFixture){
		"local": func(t *testing.T, f *recoverFixture) {
			mustRun(t, f.local, "commit", "--allow-empty", "-m", "later local work")
		},
		"gate": func(t *testing.T, f *recoverFixture) {
			mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
		},
		"pipeline": func(t *testing.T, f *recoverFixture) {
			if err := f.db.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
				t.Fatal(err)
			}
		},
		"remote": func(t *testing.T, f *recoverFixture) {
			mustRun(t, f.remote, "update-ref", "refs/heads/feature/recover", f.submitted)
		},
		"generation": func(t *testing.T, f *recoverFixture) {
			run, _ := f.db.GetRun(f.run.ID)
			if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: ptr(run.LastPushedSHA), TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
				t.Fatal(err)
			}
		},
		"target": func(t *testing.T, f *recoverFixture) {
			if _, err := f.db.UpdateRepoForkURL(f.repo.ID, f.remote+"-changed"); err != nil {
				t.Fatal(err)
			}
		},
		"anchor": func(t *testing.T, f *recoverFixture) {
			mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.submitted)
		},
		"symbolic-gate": func(t *testing.T, f *recoverFixture) {
			mustRun(t, f.gate, "update-ref", "refs/heads/other", f.preserved)
			mustRun(t, f.gate, "symbolic-ref", "refs/heads/feature/recover", "refs/heads/other")
		},
		"dirty": func(t *testing.T, f *recoverFixture) {
			mustWrite(t, filepath.Join(f.local, "untracked.txt"), "keep this\n")
		},
		"new-run": func(t *testing.T, f *recoverFixture) {
			if _, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, _, _ := newPostPushReconciliationFixture(t)
			token := reconciliationToken(t, f)
			mutate(t, f)
			beforeLocal := mustRun(t, f.local, "show-ref")
			beforeGate := mustRun(t, f.gate, "show-ref")
			beforeStatus := mustRun(t, f.local, "status", "--porcelain")
			got := f.service.Reconcile(f.ctx, "pipeline", token)
			if got.Recovered || f.custodyReturned() {
				t.Fatalf("changed observation accepted: %#v", got)
			}
			if after := mustRun(t, f.local, "show-ref"); after != beforeLocal {
				t.Fatal("local refs changed on refusal")
			}
			if after := mustRun(t, f.gate, "show-ref"); after != beforeGate {
				t.Fatal("gate refs changed on refusal")
			}
			if after := mustRun(t, f.local, "status", "--porcelain"); after != beforeStatus {
				t.Fatal("worktree changed on refusal")
			}
		})
	}
}

func TestReconcilePostPushRechecksAfterImport(t *testing.T) {
	t.Parallel()
	f, _, _ := newPostPushReconciliationFixture(t)
	token := reconciliationToken(t, f)
	f.service.beforeReconcile = func() {
		mustRun(t, f.local, "commit", "--allow-empty", "-m", "concurrent local work")
	}
	got := f.service.Reconcile(f.ctx, "pushed", token)
	if got.Recovered || f.custodyReturned() {
		t.Fatalf("raced reconciliation accepted: %#v", got)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatal("gate moved after local race")
	}
}

func TestReconcilePostPushRefusesMovementAfterPreservation(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"local", "gate", "anchor", "dirty"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			f, local, _ := newPostPushReconciliationFixture(t)
			token := reconciliationToken(t, f)
			f.service.beforeReconcileMove = func() {
				switch target {
				case "local":
					mustRun(t, f.local, "commit", "--allow-empty", "-m", "concurrent commit")
				case "gate":
					mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
				case "anchor":
					mustRun(t, f.gate, "update-ref", reconciliationRef(f.run.ID, local), f.submitted)
				case "dirty":
					mustWrite(t, filepath.Join(f.local, "file.txt"), "concurrent edit\n")
				}
			}
			got := f.service.Reconcile(f.ctx, "pipeline", token)
			if got.Recovered || f.custodyReturned() {
				t.Fatalf("raced reconciliation accepted: %#v", got)
			}
			if head := mustRun(t, f.local, "rev-parse", "HEAD"); head == f.preserved {
				t.Fatal("local branch adopted pipeline after race")
			}
			if head := mustRun(t, f.local, "rev-parse", reconciliationRef(f.run.ID, local)); head != local {
				t.Fatal("original local head was not preserved")
			}
		})
	}
}
