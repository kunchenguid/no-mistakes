//go:build e2e

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	pipelinesteps "github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type failAfterProductionStep struct {
	step pipeline.Step
	live *atomic.Bool
}

type cancelOnceProductionStep struct {
	step pipeline.Step
	live *atomic.Bool
}

func (s *cancelOnceProductionStep) Name() types.StepName { return s.step.Name() }

func (s *cancelOnceProductionStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if !s.live.CompareAndSwap(true, false) {
		return s.step.Execute(ctx)
	}
	cancelled := *ctx
	cancelledCtx, cancelledCancel := context.WithCancel(ctx.Ctx)
	cancelled.Ctx = cancelledCtx
	cancelledCancel()
	return s.step.Execute(&cancelled)
}

func (s *failAfterProductionStep) Name() types.StepName { return s.step.Name() }

func (s *failAfterProductionStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	outcome, err := s.step.Execute(ctx)
	if err != nil {
		return nil, err
	}
	if s.live.CompareAndSwap(true, false) {
		return nil, errors.New("injected recoverable post-delivery failure")
	}
	return outcome, nil
}

func TestDeliveryResumeUsesProductionDeliverySteps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider script is POSIX-only")
	}
	for _, failAt := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		t.Run(string(failAt), func(t *testing.T) {
			providerDir := t.TempDir()
			installDeliveryResumeGH(t, providerDir)
			t.Setenv("PATH", providerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			probe := &deliveryResumeProbe{}
			var failureLive atomic.Bool
			failureLive.Store(true)
			p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
				production := map[types.StepName]pipeline.Step{
					types.StepPush: &pipelinesteps.PushStep{},
					types.StepPR:   &pipelinesteps.PRStep{},
					types.StepCI:   &pipelinesteps.CIStep{},
				}
				result := make([]pipeline.Step, 0, len(types.AllSteps()))
				for _, name := range types.AllSteps() {
					if step := production[name]; step != nil {
						if name == failAt {
							if name == types.StepCI {
								step = &cancelOnceProductionStep{step: step, live: &failureLive}
							} else {
								step = &failAfterProductionStep{step: step, live: &failureLive}
							}
						}
						result = append(result, step)
						continue
					}
					result = append(result, &deliveryResumeProbeStep{name: name, probe: probe})
				}
				return result
			})

			repoID := "production-delivery-resume-" + string(failAt)
			_, headSHA := setupTestGitRepo(t, p, database, repoID)
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			var first ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir(repoID), Ref: "refs/heads/feature", Old: strings.Repeat("0", 40), New: headSHA,
			}, &first); err != nil {
				t.Fatal(err)
			}
			if run := waitForRunTerminalState(t, database, first.RunID); run.Status != types.RunFailed {
				t.Fatalf("source status = %s, want failed", run.Status)
			}
			var retry ipc.RerunResult
			if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repoID, Branch: "feature"}, &retry); err != nil {
				t.Fatal(err)
			}
			if run := waitForRunTerminalState(t, database, retry.RunID); run.Status != types.RunCompleted {
				t.Fatalf("retry status = %s, want completed: %v", run.Status, run.Error)
			}
			for _, name := range validationStepNamesForTest() {
				if got := probe.calls[name.Order()-1].Load(); got != 1 {
					t.Errorf("validation step %s executed %d times, want 1", name, got)
				}
			}
		})
	}
}

func installDeliveryResumeGH(t *testing.T, dir string) {
	t.Helper()
	state := filepath.Join(dir, "pr-created")
	ciState := filepath.Join(dir, "ci-state")
	if err := os.WriteFile(ciState, []byte("MERGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
  "auth status"*) exit 0 ;;
  "pr list"*)
    if [ -f ` + state + ` ]; then printf '%s\n' '[{"number":42,"url":"https://github.com/test/repo/pull/42","headRefName":"feature","headRepositoryOwner":{"login":"test"}}]'; else printf '%s\n' '[]'; fi
    exit 0 ;;
  "pr create"*) touch ` + state + `; printf '%s\n' 'https://github.com/test/repo/pull/42'; exit 0 ;;
  "pr edit"*) exit 0 ;;
  "pr view"*"--json state"*)
    state=$(cat ` + ciState + `)
    if [ "$state" = ERROR ]; then printf '%s\n' 'provider unavailable' >&2; exit 1; fi
    printf '%s\n' "$state"; exit 0 ;;
  "pr view"*"--json mergeable"*) printf '%s\n' 'MERGEABLE'; exit 0 ;;
  "pr checks"*) printf '%s\n' '[{"name":"build","state":"SUCCESS","bucket":"pass"}]'; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
