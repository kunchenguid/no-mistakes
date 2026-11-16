package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestCIWorkflowRunsTestsOnAllSupportedDesktopPlatforms(t *testing.T) {
	job := ciTestJob(t)
	got := make(map[string]int)
	for _, row := range job.Strategy.Matrix.Include {
		got[row["os"]]++
	}

	want := map[string]int{
		"ubuntu-latest":  1,
		"macos-latest":   3,
		"windows-latest": 12,
	}
	if len(got) != len(want) {
		t.Fatalf("test matrix operating systems = %v, want %v", got, want)
	}
	for osName, count := range want {
		if got[osName] != count {
			t.Errorf("test matrix rows for %q = %d, want %d", osName, got[osName], count)
		}
	}
}

// Remote CI owns the complete race-enabled suite; local Test stays targeted.
func TestCIWorkflowUsesRaceTestsOnUnixRunners(t *testing.T) {
	job := ciTestJob(t)
	for _, osName := range []string{"Linux", "macOS"} {
		commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
			return exactRunnerOSCondition(step.If, "==", osName)
		})
		var raceTests []workflowCommand
		for _, command := range commands {
			if command.name == "go" && len(command.args) > 0 && command.args[0] == "test" && command.hasArg("-race") && slices.Equal(goTestPackagePatterns(command), []string{"./..."}) {
				raceTests = append(raceTests, command)
			}
		}
		if len(raceTests) != 1 {
			t.Fatalf("%s go test -race ./... commands = %d, want 1; normalized commands: %#v", osName, len(raceTests), commands)
		}
		if osName == "Linux" && !slices.Equal(raceTests[0].args, []string{"test", "-race", "./..."}) {
			t.Fatalf("Linux must retain the unfiltered race suite, got %v", raceTests[0].args)
		}
	}
}

// The workflow is a declarative execution contract. Its macOS selectors must
// partition Go's runnable names exactly once, including examples and fuzz seeds.
// Use the Go test inventory, rather than implementation-source text, as the oracle.
func TestCIWorkflow_MacOSShardsRunEveryTestExactlyOnce(t *testing.T) {
	job := ciTestJob(t)
	var shards []namedTestShard
	for _, row := range job.Strategy.Matrix.Include {
		if row["os"] == "macos-latest" {
			shards = append(shards, namedTestShard{
				name: row["name"],
				run:  namedShardPattern(t, row, "macos_run"),
				skip: namedShardPattern(t, row, "macos_skip"),
			})
		}
	}
	if len(shards) != 3 {
		t.Fatalf("macOS has %d shards, want three to keep daemon and steps within the package timeout", len(shards))
	}
	commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "==", "macOS")
	})
	command, ok := findWorkflowCommandWithArg(commands, "go", "test")
	if !ok {
		t.Fatal("macOS shards have no go test command")
	}
	step := job.Steps[command.step]
	for flag, field := range map[string]string{"-run": "macos_run", "-skip": "macos_skip"} {
		env := "NM_CI_" + strings.ToUpper(field)
		if step.Env[env] != "${{ matrix."+field+" }}" || !command.hasArg(`"`+flag+`=$`+env+`"`) {
			t.Fatalf("macOS %s selector must reach go test through %s: env=%v args=%v", field, env, step.Env, command.args)
		}
	}

	cmd := exec.Command("go", "test", "-list", ".", "./internal/daemon", "./internal/pipeline/steps/...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list runnable tests: %v: %s", err, out)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if isGoTestName(line) {
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		t.Fatal("Go returned no runnable test names")
	}
	names = append(names, "Test", "Test_Probe", "Example", "ExampleProbe", "FuzzProbe")
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_" {
		names = append(names, "Test"+string(c)+"Probe")
	}
	counts := make(map[string]int)
	for _, name := range names {
		var selectedBy []string
		for _, shard := range shards {
			if shard.selects(name) {
				selectedBy = append(selectedBy, shard.name)
				counts[shard.name]++
			}
		}
		if len(selectedBy) != 1 {
			t.Errorf("%s is selected by macOS shards %v, want exactly one", name, selectedBy)
		}
	}
	if len(counts) != len(shards) {
		t.Fatalf("macOS partition leaves a shard empty: %v", counts)
	}
}
