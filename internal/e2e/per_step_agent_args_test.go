//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPerStepAgentArgsProfile is the end-user proof for
// agent_args_override_per_step: a real daemon run launches each step's agent
// process with that step's flags, and a step with no profile still gets the
// global agent_args_override flags. The assertion reads the fake agent's
// recorded argv, which is the actual command line the daemon executed.
func TestPerStepAgentArgsProfile(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	const (
		globalModel   = "global-model"
		reviewModel   = "review-model"
		documentModel = "document-model"
	)

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	source := string(data) + fmt.Sprintf(`agent_args_override:
  claude:
    - --model
    - %s
agent_args_override_per_step:
  review:
    claude:
      - --model
      - %s
  document:
    claude:
      - --model
      - %s
`, globalModel, reviewModel, documentModel)
	if err := os.WriteFile(globalConfig, []byte(source), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/per-step-agent-args"
	h.CommitChange(branch, "feature.txt", "per-step profile\n", "add feature")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	// Every invocation carries its phase boundary in the prompt, so the
	// recorded argv can be attributed back to the step that made the call.
	byStep := map[types.StepName][][]string{}
	for _, inv := range h.AgentInvocations() {
		for _, step := range types.AllSteps() {
			if strings.Contains(inv.Prompt, fmt.Sprintf("You are the %s phase", step)) {
				byStep[step] = append(byStep[step], inv.Args)
				break
			}
		}
	}

	assertModel := func(step types.StepName, want string) {
		t.Helper()
		invocations := byStep[step]
		if len(invocations) == 0 {
			t.Fatalf("no agent invocation recorded for the %s step", step)
		}
		for i, args := range invocations {
			if !argvHasPair(args, "--model", want) {
				t.Errorf("%s invocation %d argv = %v, want --model %s", step, i, args, want)
			}
			for _, other := range []string{globalModel, reviewModel, documentModel} {
				if other != want && argvHasPair(args, "--model", other) {
					t.Errorf("%s invocation %d argv = %v, must not carry --model %s", step, i, args, other)
				}
			}
		}
		t.Logf("%s step launched with --model %s (argv: %v)", step, want, invocations[0])
	}

	assertModel(types.StepReview, reviewModel)
	assertModel(types.StepDocument, documentModel)
	// Test has no profile, so it falls back to the global override.
	assertModel(types.StepTest, globalModel)
}

func argvHasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
