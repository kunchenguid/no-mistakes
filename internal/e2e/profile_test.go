//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// profileV1 and profileV2 are two revisions of one shared gate profile. Each is
// a plain built-in step list that differs from the default pipeline (no
// intent/review/document), so a run driven by the profile is unmistakably
// distinguishable from the default. V2 inserts a lint step to prove an edit to
// the single on-disk profile propagates to every repo that selects it.
const profileV1 = "version: 1\nsteps:\n  - rebase\n  - test\n  - push\n  - pr\n  - ci\n"
const profileV2 = "version: 2\nsteps:\n  - rebase\n  - lint\n  - test\n  - push\n  - pr\n  - ci\n"

// TestSharedProfileAcrossTwoRepos is the headline acceptance test for shared
// gate profiles: ONE profile directory under <NM_HOME>/profiles/shared/ is
// applied to TWO independent repos via a trusted `profile: shared` field. Both
// gates run the profile's steps (not the default pipeline). Editing that single
// profile file and re-pushing makes BOTH repos pick up the change on their next
// run — the whole point of a shared profile.
func TestSharedProfileAcrossTwoRepos(t *testing.T) {
	optOut := false // secure default: profile is trusted-only regardless
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		AllowRepoCommands: &optOut,
		RepoConfigExtra:   "profile: shared\n",
		Profiles: map[string]map[string]string{
			"shared": {"profile.yaml": profileV1},
		},
	})

	// Repo A is the primary harness repo; it selects the shared profile via its
	// trusted default-branch .no-mistakes.yaml (RepoConfigExtra above).
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init (repo A): %v\n%s", err, out)
	}
	// Repo B is a second, independent gated repo under the same NM_HOME/daemon,
	// selecting the same shared profile.
	repoBConfig := "ignore_patterns:\n  - 'vendor/**'\nallow_repo_commands: false\nprofile: shared\n"
	repoB := h.NewRepo("repo-b", repoBConfig, nil)

	wantV1 := []types.StepName{types.StepRebase, types.StepTest, types.StepPush, types.StepPR, types.StepCI}

	// First run on both repos: the profile's V1 steps drive each gate.
	branch1 := "feature-1"
	h.CommitChange(branch1, "a1.txt", "change a1\n", "repo A change 1")
	h.PushToGate(branch1)
	repoB.CommitChange(branch1, "b1.txt", "change b1\n", "repo B change 1")
	repoB.PushToGate(branch1)

	runA := h.WaitForRun(branch1, 120*time.Second)
	runB := repoB.WaitForRun(branch1, 120*time.Second)
	assertRunCompleted(t, "repo A / v1", runA)
	assertRunCompleted(t, "repo B / v1", runB)
	assertStepNames(t, "repo A / v1", runA, wantV1)
	assertStepNames(t, "repo B / v1", runB, wantV1)

	// The run record is stamped with the profile that gated it, so a consumer
	// can confirm which profile enforced the gate.
	assertProfileStamp(t, "repo A / v1", runA)
	assertProfileStamp(t, "repo B / v1", runB)

	// Edit the ONE shared profile file on disk (insert a lint step).
	h.WriteProfileFile("shared", "profile.yaml", profileV2)

	wantV2 := []types.StepName{types.StepRebase, types.StepLint, types.StepTest, types.StepPush, types.StepPR, types.StepCI}

	// A fresh push on each repo starts a new run, which reads the profile fresh
	// from disk — both repos must now run the V2 step list.
	branch2 := "feature-2"
	h.CommitChange(branch2, "a2.txt", "change a2\n", "repo A change 2")
	h.PushToGate(branch2)
	repoB.CommitChange(branch2, "b2.txt", "change b2\n", "repo B change 2")
	repoB.PushToGate(branch2)

	runA2 := h.WaitForRun(branch2, 120*time.Second)
	runB2 := repoB.WaitForRun(branch2, 120*time.Second)
	assertRunCompleted(t, "repo A / v2", runA2)
	assertRunCompleted(t, "repo B / v2", runB2)
	assertStepNames(t, "repo A / v2", runA2, wantV2)
	assertStepNames(t, "repo B / v2", runB2, wantV2)
}

// TestProfileSpliceComposition proves the `- use: profile` splice sentinel:
// a repo that keeps its own steps: list positions the profile's steps in place
// with the sentinel, and the merged list is what runs.
func TestProfileSpliceComposition(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		AllowRepoCommands: &optOut,
		// The repo brings its own intent step and splices the profile after it.
		RepoConfigExtra: "profile: shared\nsteps:\n  - intent\n  - use: profile\n",
		Profiles: map[string]map[string]string{
			"shared": {"profile.yaml": profileV1},
		},
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	branch := "feature-splice"
	h.CommitChange(branch, "s.txt", "change\n", "add change")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	assertRunCompleted(t, "splice", run)
	// intent (repo) then the profile's V1 steps spliced in place.
	want := []types.StepName{types.StepIntent, types.StepRebase, types.StepTest, types.StepPush, types.StepPR, types.StepCI}
	assertStepNames(t, "splice", run, want)
}

// TestProfileMissingFailsRun proves the fail-closed posture: a repo selects a
// profile the host has not provisioned, so the run fails at start rather than
// silently dropping to the default pipeline.
func TestProfileMissingFailsRun(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		AllowRepoCommands: &optOut,
		RepoConfigExtra:   "profile: not-provisioned\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	branch := "feature"
	h.CommitChange(branch, "x.txt", "change\n", "add change")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed (missing profile must fail closed); error=%v", run.Status, deref(run.Error))
	}
	if run.Error == nil || !strings.Contains(*run.Error, "profile") {
		t.Errorf("run error = %v, want a profile-related failure", deref(run.Error))
	}
}

func assertRunCompleted(t *testing.T, label string, run *ipc.RunInfo) {
	t.Helper()
	if run.Status != types.RunCompleted {
		t.Fatalf("%s: run status = %s, want completed; error=%v", label, run.Status, deref(run.Error))
	}
}

func assertStepNames(t *testing.T, label string, run *ipc.RunInfo, want []types.StepName) {
	t.Helper()
	got := stepNamesOf(run.Steps)
	if len(got) != len(want) {
		t.Fatalf("%s: ran %d steps %v, want exactly the profile's %d %v", label, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: steps[%d] = %q, want %q (full: %v)", label, i, got[i], want[i], got)
		}
	}
}

func assertProfileStamp(t *testing.T, label string, run *ipc.RunInfo) {
	t.Helper()
	if run.Profile == nil {
		t.Errorf("%s: run has no profile stamp; a profile-gated run must record which profile enforced it", label)
		return
	}
	if !strings.HasPrefix(*run.Profile, "shared@") {
		t.Errorf("%s: profile stamp = %q, want a \"shared@<ref>\" stamp", label, *run.Profile)
	}
}
