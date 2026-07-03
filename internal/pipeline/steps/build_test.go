package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func specs(names ...string) []config.StepSpec {
	out := make([]config.StepSpec, 0, len(names))
	for _, n := range names {
		out = append(out, config.StepSpec{Name: n})
	}
	return out
}

func pipelineNames(t *testing.T, s []config.StepSpec) []types.StepName {
	t.Helper()
	built, err := BuildPipeline(s)
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	names := make([]types.StepName, 0, len(built))
	for _, step := range built {
		names = append(names, step.Name())
	}
	return names
}

// The backward-compatibility guarantee: no steps config means the exact
// default pipeline, identical to what AllSteps has always returned.
func TestBuildPipeline_NilMatchesDefaultPipeline(t *testing.T) {
	want := types.AllSteps()
	for _, in := range [][]config.StepSpec{nil, {}} {
		got := pipelineNames(t, in)
		if len(got) != len(want) {
			t.Fatalf("BuildPipeline(%v) = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("BuildPipeline(%v)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}

	all := AllSteps()
	if len(all) != len(want) {
		t.Fatalf("AllSteps() has %d steps, want %d", len(all), len(want))
	}
	for i, step := range all {
		if step.Name() != want[i] {
			t.Fatalf("AllSteps()[%d] = %q, want %q", i, step.Name(), want[i])
		}
	}
}

func TestBuildPipeline_SubsetInOrder(t *testing.T) {
	got := pipelineNames(t, specs("rebase", "test", "push", "pr", "ci"))
	want := []types.StepName{types.StepRebase, types.StepTest, types.StepPush, types.StepPR, types.StepCI}
	if len(got) != len(want) {
		t.Fatalf("pipeline = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pipeline[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildPipeline_ReorderWithoutPushChain(t *testing.T) {
	// Reordering steps that carry no hard dependency must be allowed.
	got := pipelineNames(t, specs("lint", "review", "test"))
	want := []types.StepName{types.StepLint, types.StepReview, types.StepTest}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pipeline[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildPipeline_UnknownName(t *testing.T) {
	_, err := BuildPipeline(specs("rebase", "fuzz", "push"))
	if err == nil {
		t.Fatal("expected error for unknown step name")
	}
	if !strings.Contains(err.Error(), `"fuzz"`) {
		t.Errorf("error should name the unknown step: %v", err)
	}
	if !strings.Contains(err.Error(), "intent, rebase, review, test, document, lint, push, pr, ci") {
		t.Errorf("error should list valid step names: %v", err)
	}
}

func TestBuildPipeline_EmptyName(t *testing.T) {
	if _, err := BuildPipeline(specs("rebase", "", "push")); err == nil {
		t.Fatal("expected error for empty step name")
	}
}

func TestBuildPipeline_DuplicateName(t *testing.T) {
	_, err := BuildPipeline(specs("rebase", "test", "test", "push"))
	if err == nil {
		t.Fatal("expected error for duplicate step name")
	}
	if !strings.Contains(err.Error(), `duplicate step "test"`) {
		t.Errorf("error should call out the duplicate: %v", err)
	}
}

// The dependency chain protects documented data-loss invariants: ci needs the
// PR that pr opened, pr needs the branch push published, and push's
// force-push lease is anchored by rebase's fetch of the remote tips.
func TestBuildPipeline_DependencyValidation(t *testing.T) {
	tests := []struct {
		name    string
		steps   []config.StepSpec
		wantErr string
	}{
		{"ci_without_pr", specs("rebase", "push", "ci"), `"ci" requires "pr"`},
		{"ci_before_pr", specs("rebase", "push", "ci", "pr"), `"ci" must come after "pr"`},
		{"pr_without_push", specs("rebase", "pr"), `"pr" requires "push"`},
		{"pr_before_push", specs("rebase", "pr", "push"), `"pr" must come after "push"`},
		{"push_without_rebase", specs("test", "push"), `"push" requires "rebase"`},
		{"push_before_rebase", specs("push", "rebase"), `"push" must come after "rebase"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildPipeline(tt.steps)
			if err == nil {
				t.Fatalf("expected error for %v", tt.steps)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// All problems are reported in one pass so a maintainer fixes the config once.
func TestBuildPipeline_ReportsAllProblems(t *testing.T) {
	_, err := BuildPipeline(specs("fuzz", "push", "ci"))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{`"fuzz"`, `"push" requires "rebase"`, `"ci" requires "pr"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// A custom command step builds into a CommandStep carrying its command +
// options, and slots into the pipeline at its list position alongside built-ins.
func TestBuildPipeline_CustomCommandStep(t *testing.T) {
	in := []config.StepSpec{
		{Name: "rebase"},
		{Name: "swiftlint", Command: "swiftlint lint", FindingsJSON: "sl.json", AutoFix: true},
		{Name: "push"},
	}
	built, err := BuildPipeline(in)
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	if len(built) != 3 {
		t.Fatalf("got %d steps, want 3", len(built))
	}
	cs, ok := built[1].(*CommandStep)
	if !ok {
		t.Fatalf("step[1] is %T, want *CommandStep", built[1])
	}
	if cs.Name() != types.StepName("swiftlint") || cs.Command != "swiftlint lint" || cs.FindingsPath != "sl.json" || !cs.AutoFix {
		t.Errorf("CommandStep = %+v, want swiftlint config wired through", cs)
	}
}

func TestBuildPipeline_CustomStepNameCollidesWithBuiltin(t *testing.T) {
	_, err := BuildPipeline([]config.StepSpec{{Name: "test", Command: "echo hi"}})
	if err == nil {
		t.Fatal("expected error for custom step colliding with a built-in name")
	}
	if !strings.Contains(err.Error(), "collides with a built-in") {
		t.Errorf("error = %v, want collision message", err)
	}
}

func TestBuildPipeline_CustomStepInvalidName(t *testing.T) {
	_, err := BuildPipeline([]config.StepSpec{{Name: "Swift Lint!", Command: "swiftlint"}})
	if err == nil {
		t.Fatal("expected error for invalid custom step name")
	}
	if !strings.Contains(err.Error(), "invalid custom step name") {
		t.Errorf("error = %v, want invalid-name message", err)
	}
}

func TestBuildPipeline_DuplicateCustomAndBuiltinName(t *testing.T) {
	_, err := BuildPipeline([]config.StepSpec{
		{Name: "swiftlint", Command: "swiftlint lint"},
		{Name: "swiftlint", Command: "swiftlint lint --strict"},
	})
	if err == nil {
		t.Fatal("expected error for duplicate custom step name")
	}
	if !strings.Contains(err.Error(), `duplicate step "swiftlint"`) {
		t.Errorf("error = %v, want duplicate message", err)
	}
}

func TestValidateStepNames_Warnings(t *testing.T) {
	tests := []struct {
		name     string
		steps    []types.StepName
		wantWarn string
	}{
		{
			"intent_after_review",
			[]types.StepName{types.StepReview, types.StepIntent},
			"intent",
		},
		{
			"mutating_after_push",
			[]types.StepName{types.StepRebase, types.StepPush, types.StepLint},
			`"lint"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs, warns := validateStepNames(tt.steps)
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if len(warns) == 0 {
				t.Fatalf("expected a warning for %v", tt.steps)
			}
			found := false
			for _, w := range warns {
				if strings.Contains(w, tt.wantWarn) {
					found = true
				}
			}
			if !found {
				t.Errorf("warnings %v, want one containing %q", warns, tt.wantWarn)
			}
		})
	}
}

func TestValidateStepNames_DefaultPipelineCleanAndWarningFree(t *testing.T) {
	errs, warns := validateStepNames(types.AllSteps())
	if len(errs) > 0 {
		t.Errorf("default pipeline produced errors: %v", errs)
	}
	if len(warns) > 0 {
		t.Errorf("default pipeline produced warnings: %v", warns)
	}
}
