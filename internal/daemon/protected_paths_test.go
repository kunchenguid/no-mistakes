package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type protectedPathPushRetryStep struct {
	edited bool
}

func (s *protectedPathPushRetryStep) Name() types.StepName { return types.StepPush }
func (s *protectedPathPushRetryStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if !s.edited {
		s.edited = true
		if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, sctx.Run.HeadSHA); err != nil {
			return nil, err
		}
		return (protectedPathCommitStep{step: &steps.PushStep{}}).Execute(sctx)
	}
	return (&steps.PushStep{}).Execute(sctx)
}

func TestProtectedPathPushApprovalCannotSkipPublicationOrDiscardEdits(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&protectedPathPushRetryStep{}}
	})
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-publication")
	publicationDir := filepath.Join(t.TempDir(), "published.git")
	gitCmd(t, "", "init", "--bare", publicationDir)
	if _, err := database.UpdateRepoForkURL(repo.ID, publicationDir); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main",
		Old: strings.Repeat("0", 40), New: headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, result.RunID)
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := database.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if run.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("refusal did not park: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
	}

	approvalErr := client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID: result.RunID, Step: types.StepPush, Action: types.ActionApprove,
	}, nil)
	if approvalErr == nil {
		run := waitForRunTerminalState(t, database, result.RunID)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(workDir); os.IsNotExist(err) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, publicationErr := git.Run(context.Background(), publicationDir, "show-ref", "--verify", "refs/heads/main")
		_, worktreeErr := os.Stat(workDir)
		t.Fatalf("approval bypassed refused Push: run=%s published=%v worktree_exists=%v", run.Status, publicationErr == nil, worktreeErr == nil)
	}
	if !strings.Contains(approvalErr.Error(), "protected") || !strings.Contains(approvalErr.Error(), "fix") {
		t.Fatalf("approval refusal lacks retry guidance: %v", approvalErr)
	}
	if got := gitOutput(t, workDir, "show", ":test.txt"); got != "staged edit" {
		t.Fatalf("approval changed the index: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "test.txt")); err != nil || string(got) != "unstaged edit\n" {
		t.Fatalf("approval changed working files: %q, %v", got, err)
	}
	if _, err := git.Run(context.Background(), publicationDir, "show-ref", "--verify", "refs/heads/main"); err == nil {
		t.Fatal("unresolved protected edit was published")
	}

	// The operator resolves the protected edit, then explicitly retries Push.
	gitCmd(t, workDir, "restore", "--source=HEAD", "--staged", "--worktree", "--", "test.txt")
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID: result.RunID, Step: types.StepPush, Action: types.ActionFix,
	}, nil); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, database, result.RunID)
	if run.Status != types.RunCompleted || run.LastPushedSHA == nil || *run.LastPushedSHA != headSHA {
		t.Fatalf("retry did not complete publication: %+v", run)
	}
	if got := gitOutput(t, publicationDir, "rev-parse", "refs/heads/main"); got != headSHA {
		t.Fatalf("published head = %s, want %s", got, headSHA)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(workDir); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("clean worktree was not removed after successful publication")
}

type protectedPathCommitStep struct {
	step pipeline.Step
}

func (s protectedPathCommitStep) Name() types.StepName { return s.step.Name() }
func (s protectedPathCommitStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	sctx.Config.ProtectedPaths = []string{"*.txt"}
	file := filepath.Join(sctx.WorkDir, "test.txt")
	if err := os.WriteFile(file, []byte("staged edit\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "test.txt"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte("unstaged edit\n"), 0o644); err != nil {
		return nil, err
	}
	if s.Name() == types.StepTest {
		sctx.Fixing = true
		sctx.Agent = recoveredRunTestAgent{}
	}
	return s.step.Execute(sctx)
}

func TestProtectedPathRefusalParksBeforeManagerCleanup(t *testing.T) {
	for _, step := range []pipeline.Step{&steps.PushStep{}, &steps.TestStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
				return []pipeline.Step{protectedPathCommitStep{step: step}}
			})
			_, headSHA := setupTestGitRepo(t, p, database, "protected-paths")
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir("protected-paths"), Ref: "refs/heads/main",
				Old: strings.Repeat("0", 40), New: headSHA,
			}, &result); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				run, err := database.GetRun(result.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != types.RunRunning && run.Status != types.RunPending {
					t.Fatalf("refusal reached terminal cleanup: status=%s error=%v", run.Status, run.Error)
				}
				if run.AwaitingAgentSince != nil {
					workDir := p.WorktreeDir(run.RepoID, run.ID)
					if got := gitOutput(t, workDir, "rev-parse", "HEAD"); got != headSHA {
						t.Errorf("refusal committed: HEAD=%s want %s", got, headSHA)
					}
					if got := gitOutput(t, workDir, "show", ":test.txt"); got != "staged edit" {
						t.Errorf("refusal changed index content: %q", got)
					}
					if got, err := os.ReadFile(filepath.Join(workDir, "test.txt")); err != nil || string(got) != "unstaged edit\n" {
						t.Errorf("refusal changed worktree content: %q err=%v", got, err)
					}
					results, err := database.GetStepsByRun(run.ID)
					if err != nil || len(results) != 1 || results[0].FindingsJSON == nil {
						t.Fatalf("missing persisted gate: %v %v", results, err)
					}
					findings, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
					if err != nil || len(findings.Items) != 1 || findings.Items[0].File != "test.txt" || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, `rule "*.txt"`) {
						t.Fatalf("gate lost path, rule, or operator decision: %+v err=%v", findings, err)
					}
					if err := pipeline.ValidateRecoveredRun(database, run, []pipeline.Step{step}); err != nil {
						t.Fatalf("refusal gate cannot recover: %v", err)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("refusal never parked: %+v", run)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
