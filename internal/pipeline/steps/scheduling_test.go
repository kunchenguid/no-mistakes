package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type scheduledCISpy struct{}

func (*scheduledCISpy) Name() types.StepName { return types.StepCI }
func (*scheduledCISpy) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	panic("CI monitor must not execute in a scheduling test")
}

func TestAllStepsForConfig_TrustedNoCIOmitsCIWithoutConstructingIt(t *testing.T) {
	constructed := 0
	got := allStepsForConfig(&config.Config{NoCI: true}, func() pipeline.Step {
		constructed++ // Models forge-client/monitor initialization at construction time.
		return &scheduledCISpy{}
	})

	if constructed != 0 {
		t.Fatalf("CI was constructed %d time(s); trusted no_ci must suppress construction before any forge initialization", constructed)
	}
	assertStepAbsent(t, got, types.StepCI)
}

func TestAllStepsForConfig_FalseOrAbsentNoCISchedulesCI(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{name: "absent", cfg: nil},
		{name: "false", cfg: &config.Config{NoCI: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constructed := 0
			got := allStepsForConfig(tc.cfg, func() pipeline.Step {
				constructed++
				return &scheduledCISpy{}
			})
			if constructed != 1 {
				t.Fatalf("CI constructions = %d, want 1", constructed)
			}
			assertStepPresent(t, got, types.StepCI)
		})
	}
}

func TestAllStepsForConfig_BranchOnlyNoCIStillSchedulesCI(t *testing.T) {
	effective := config.EffectiveRepoConfig(
		&config.RepoConfig{NoCI: true},  // untrusted pushed branch
		&config.RepoConfig{NoCI: false}, // trusted default branch
		false,
	)
	cfg := config.Merge(&config.GlobalConfig{}, effective)

	constructed := 0
	got := allStepsForConfig(cfg, func() pipeline.Step {
		constructed++
		return &scheduledCISpy{}
	})
	if constructed != 1 {
		t.Fatalf("CI constructions = %d, want 1; branch-only no_ci must not suppress monitoring", constructed)
	}
	assertStepPresent(t, got, types.StepCI)
}

func assertStepPresent(t *testing.T, steps []pipeline.Step, name types.StepName) {
	t.Helper()
	for _, step := range steps {
		if step.Name() == name {
			return
		}
	}
	t.Fatalf("step %s is absent", name)
}

func assertStepAbsent(t *testing.T, steps []pipeline.Step, name types.StepName) {
	t.Helper()
	for _, step := range steps {
		if step.Name() == name {
			t.Fatalf("step %s is present", name)
		}
	}
}
