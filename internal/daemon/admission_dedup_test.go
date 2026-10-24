package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReusableActiveRunRequiresFreshMatchingRow(t *testing.T) {
	head := "head"
	digest := "digest"
	observed := &db.Run{ID: "run-1"}
	current := &db.Run{ID: "run-1", SubmittedHeadSHA: &head, AdmissionDigest: &digest}
	m := &RunManager{runContexts: make(map[string]context.Context)}
	runCtx, cancel := context.WithCancelCause(context.Background())
	m.runContexts[current.ID] = runCtx
	if !m.reusableActiveRun(observed, current, head, digest) {
		t.Fatal("fresh equivalent active row was not reusable")
	}
	if m.reusableActiveRun(observed, nil, head, digest) {
		t.Fatal("stopped run remained reusable")
	}
	replacement := *current
	replacement.ID = "run-2"
	if m.reusableActiveRun(observed, &replacement, head, digest) {
		t.Fatal("replacement active row reused stale run")
	}
	parked := *current
	parkedAt := time.Now().Unix()
	parked.AwaitingAgentSince = &parkedAt
	if m.reusableActiveRun(observed, &parked, head, digest) {
		t.Fatal("parked run was reusable")
	}
	cancel(context.Canceled)
	if m.reusableActiveRun(observed, current, head, digest) {
		t.Fatal("run with requested cancellation was reusable")
	}
}

func TestPushReceivedReusesEquivalentActiveRunAndSupersedesChangedIntent(t *testing.T) {
	started := make(chan string, 3)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "admission-dedup-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	call := func(intent string) ipc.PushReceivedResult {
		t.Helper()
		var result ipc.PushReceivedResult
		if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir("admission-dedup-repo"), Ref: "refs/heads/main",
			Old: "0000000000000000000000000000000000000000", New: headSHA, Intent: intent,
		}, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := call("review the delivery")
	waitForStartedBranch(t, started, "main")
	duplicate := call("review the delivery")
	if duplicate.RunID != first.RunID {
		t.Fatalf("equivalent delivery run = %q, want reused %q", duplicate.RunID, first.RunID)
	}
	stored, err := d.GetRun(first.RunID)
	if err != nil || stored == nil || stored.AdmissionDigest == nil || *stored.AdmissionDigest == "" {
		t.Fatalf("stored admission identity = %#v, err=%v", stored, err)
	}
	select {
	case branch := <-started:
		t.Fatalf("equivalent delivery started a second executor for %q", branch)
	case <-time.After(150 * time.Millisecond):
	}

	replacement := call("review the changed task")
	if replacement.RunID == first.RunID {
		t.Fatal("changed intent reused the superseded run")
	}
	waitForStartedBranch(t, started, "main")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		old, err := d.GetRun(first.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if old != nil && old.Status == types.RunCancelled {
			if old.Error == nil || *old.Error != types.RunCancelReasonSuperseded {
				t.Fatalf("superseded error = %#v", old.Error)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s was not superseded", first.RunID)
}
