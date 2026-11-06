package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type identityCorrectionStep struct{ commits chan string }

func (*identityCorrectionStep) Name() types.StepName { return types.StepReview }
func (s *identityCorrectionStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if !sctx.Fixing {
		return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"identity-fix","severity":"warning","description":"synthetic correction","action":"auto-fix","file":"test.txt","line":1}],"summary":"correction"}`}, nil
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "commit", "--allow-empty", "-m", "synthetic correction"); err != nil {
		return nil, err
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = head
	got, err := git.Run(sctx.Ctx, sctx.WorkDir, "show", "-s", "--format=%an <%ae> | %cn <%ce>")
	if err != nil {
		return nil, err
	}
	s.commits <- got
	return &pipeline.StepOutcome{}, nil
}

func TestCommitIdentityTwoRunsSurvivePrivateDaemonRestart(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, d, "identity-restart")
	commits := make(chan string, 2)
	start := func() chan error {
		errCh := make(chan error, 1)
		go func() {
			errCh <- RunWithOptions(p, d, func() []pipeline.Step { return []pipeline.Step{&identityCorrectionStep{commits: commits}} })
		}()
		waitForDaemonReady(t, p)
		return errCh
	}
	errCh := start()
	wants := map[string]string{}
	for _, name := range []string{"First", "Second"} {
		identity := git.CommitIdentity{AuthorName: name, AuthorEmail: name + "@example.test", CommitterName: name + " Committer", CommitterEmail: "committer-" + name + "@example.test"}
		branch := "identity-" + name
		gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/"+branch, head)
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			t.Fatal(err)
		}
		var result ipc.PushReceivedResult
		err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/" + branch, Old: head, New: head, CommitIdentity: &identity}, &result)
		client.Close()
		if err != nil {
			t.Fatal(err)
		}
		wants[result.RunID] = name + " <" + name + "@example.test> | " + name + " Committer <committer-" + name + "@example.test>"
	}
	deadline := time.Now().Add(30 * time.Second)
	for id := range wants {
		for {
			run, err := d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if run.AwaitingAgentSince != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s did not park: %+v", id, run)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	stopParkedRunDaemon(t, p, errCh)
	// Both the submitting checkout and the daemon environment change before
	// restart. Neither can replace the values already bound to these runs.
	gitCmd(t, repo.WorkingPath, "config", "user.name", "Changed Checkout")
	gitCmd(t, repo.WorkingPath, "config", "user.email", "changed@example.test")
	t.Setenv("GIT_AUTHOR_NAME", "Daemon")
	t.Setenv("GIT_AUTHOR_EMAIL", "daemon@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Daemon")
	t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
	errCh = start()
	defer stopParkedRunDaemon(t, p, errCh)
	for id, want := range wants {
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			err = client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: types.ActionFix, FindingIDs: []string{"identity-fix"}}, &ipc.RespondResult{})
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("recovered gate: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		client.Close()
		select {
		case got := <-commits:
			if got != want {
				t.Fatalf("recovered correction = %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no correction commit")
		}
		// A fix round deliberately parks for its review, even when the mock
		// step reports no remaining findings. Complete that ordinary gate.
		deadline = time.Now().Add(10 * time.Second)
		for {
			results, err := d.GetStepsByRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) == 1 && results[0].Status == types.StepStatusFixReview {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("correction did not reach fix review: %+v", results)
			}
			time.Sleep(20 * time.Millisecond)
		}
		client, err = ipc.Dial(p.Socket())
		if err != nil {
			t.Fatal(err)
		}
		err = client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: types.ActionApprove}, &ipc.RespondResult{})
		client.Close()
		if err != nil {
			t.Fatal(err)
		}
		run := waitForRunTerminalState(t, d, id)
		if run.Status != types.RunCompleted {
			t.Fatalf("correction run failed: %+v", run)
		}
	}
}

func TestSubmissionIdentityDoesNotTrustDaemonOverrides(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, _ := setupTestGitRepo(t, p, d, "identity-legacy")
	t.Setenv("GIT_AUTHOR_NAME", "Daemon")
	t.Setenv("GIT_AUTHOR_EMAIL", "daemon@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Daemon")
	t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
	identity, err := submissionIdentity(context.Background(), repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	if identity.AuthorName != "Test" || identity.CommitterEmail != "test@test.com" {
		t.Fatalf("daemon identity won: %+v", identity)
	}
}

func TestSubmissionIdentityRefusesUnprovableLegacyGlobalIdentity(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, _ := setupTestGitRepo(t, p, d, "identity-unprovable")
	gitCmd(t, repo.WorkingPath, "config", "--unset", "user.name")
	gitCmd(t, repo.WorkingPath, "config", "--unset", "user.email")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Daemon Global")
	t.Setenv("GIT_CONFIG_KEY_1", "user.email")
	t.Setenv("GIT_CONFIG_VALUE_1", "daemon@example.test")
	if _, err := submissionIdentity(context.Background(), repo.WorkingPath); err == nil {
		t.Fatal("legacy caller silently inherited a valid daemon-global identity")
	}
}
