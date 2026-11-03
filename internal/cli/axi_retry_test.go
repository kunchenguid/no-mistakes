package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunObjectLinksARetryToTheFailureItContinues(t *testing.T) {
	continuation := runView{
		ID: "run-2", Branch: "feature/x", Status: string(types.RunRunning), HeadSHA: "abcdef1234567890",
		RetryOf: &ipc.RunRetryInfo{RunID: "run-1", Step: types.StepReview, Round: 4, Error: "agent exited", CheckpointHeadSHA: "abcdef1234567890"},
	}
	out := axiDoc(runObjectField(continuation))
	for _, want := range []string{"  retry_of:\n", "    run: run-1\n", "    step: review\n", "    failed_round: 4\n", "    checkpoint_head: abcdef12\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("continuation run object missing %q:\n%s", want, out)
		}
	}

	failed := runView{ID: "run-1", Branch: "feature/x", Status: string(types.RunFailed), HeadSHA: "abcdef1234567890", RetriedBy: "run-2"}
	if out := axiDoc(runObjectField(failed)); !strings.Contains(out, "  retried_by: run-2\n") {
		t.Errorf("failed run object does not name its continuation:\n%s", out)
	}
	if out := axiDoc(runObjectField(runView{ID: "run-3", Status: string(types.RunFailed)})); strings.Contains(out, "retry_of") || strings.Contains(out, "retried_by") {
		t.Errorf("a run with no retry lineage reports one:\n%s", out)
	}
}

func TestAxiRetryRequiresTheFailedRunsID(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())
	out, err := executeCmd("axi", "retry")
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != 2 || !strings.Contains(out, "--run is required") {
		t.Fatalf("axi retry without --run = %v:\n%s", err, out)
	}
}
