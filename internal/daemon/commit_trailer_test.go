package daemon

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/committrailer"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushReceivedPersistsCommitTrailers(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "trailer-run-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	trailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
		"Reviewed-by: Reviewer <reviewer@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:           p.RepoDir("trailer-run-repo"),
		Ref:            "refs/heads/main",
		Old:            "0000000000000000000000000000000000000000",
		New:            headSHA,
		CommitTrailers: trailers,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if !reflect.DeepEqual(run.CommitTrailers, trailers) {
		t.Fatalf("run commit trailers = %#v, want %#v", run.CommitTrailers, trailers)
	}
}

func TestRerunInheritsCommitTrailersFromSelectedRun(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "selected-trailer-rerun-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	trailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}

	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:           p.RepoDir("selected-trailer-rerun-repo"),
		Ref:            "refs/heads/main",
		Old:            "0000000000000000000000000000000000000000",
		New:            headSHA,
		CommitTrailers: trailers,
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)

	newerTrailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Other Agent <other@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := d.InsertRunWithOptions("selected-trailer-rerun-repo", "main", headSHA, headSHA, db.InsertRunOptions{
		CommitTrailers: newerTrailers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(newer.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	var rerun ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:        "selected-trailer-rerun-repo",
		Branch:        "main",
		PreviousRunID: first.RunID,
	}, &rerun)
	if err != nil {
		t.Fatal(err)
	}
	got := waitForRunTerminalState(t, d, rerun.RunID)
	if !reflect.DeepEqual(got.CommitTrailers, trailers) {
		t.Fatalf("rerun trailers = %#v, want selected run trailers %#v", got.CommitTrailers, trailers)
	}
}
