package daemon

import (
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
