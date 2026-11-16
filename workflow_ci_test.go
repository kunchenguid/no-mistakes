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

func TestCIWorkflowUsesRaceTestsOnUnixRunners(t *testing.T) {
	job := ciTestJob(t)
	commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "==", "Linux")
	})

	var raceTests []workflowCommand
	for _, command := range commands {
		if command.name == "go" && slices.Equal(command.args, []string{"test", "-race", "./..."}) {
			raceTests = append(raceTests, command)
		}
	}
	if len(raceTests) != 1 {
		t.Fatalf("Linux go test -race ./... commands = %d, want 1; normalized commands: %#v", len(raceTests), commands)
	}

	commands = workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "==", "macOS")
	})
	if len(commands) != 1 || commands[0].name != "go" {
		t.Fatalf("want one macOS go test command, got %#v", commands)
	}
	command := commands[0]
	args := make([]string, len(command.args))
	for i, arg := range command.args {
		args[i] = unquoteWorkflowArg(arg)
	}
	want := []string{"test", "-race", "-run=$NM_CI_TEST_RUN", "-skip=$NM_CI_TEST_SKIP", "./..."}
	if !slices.Equal(args, want) {
		t.Fatalf("macOS test arguments = %v, want %v", args, want)
	}
	step := job.Steps[command.step]
	for env, key := range map[string]string{"NM_CI_TEST_RUN": "test_run", "NM_CI_TEST_SKIP": "test_skip"} {
		if got, want := step.Env[env], "${{ matrix."+key+" }}"; got != want {
			t.Errorf("macOS %s = %q, want %q", env, got, want)
		}
	}
}

func TestCIWorkflow_MacOSShardsRunEveryTestExactlyOnce(t *testing.T) {
	job := ciTestJob(t)
	var shards []namedTestShard
	for _, row := range job.Strategy.Matrix.Include {
		if row["os"] == "macos-latest" {
			shards = append(shards, namedTestShard{
				name: row["name"],
				run:  namedShardPattern(t, row, "test_run"),
				skip: namedShardPattern(t, row, "test_skip"),
			})
		}
	}
	if len(shards) < 2 {
		t.Fatalf("macOS has %d shards, want multiple package invocations within the default Go timeout", len(shards))
	}
	names := []string{"Test", "Test_Probe", "TestÉProbe", "Fuzz", "FuzzParse", "Example", "ExampleNew", "Example_type"}
	for _, initial := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" {
		names = append(names, "Test"+string(initial)+"Probe")
	}
	cmd := exec.Command("go", "test", "-list=.", "./internal/pipeline/steps")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list pipeline step tests: %v\n%s", err, out)
	}
	listed := 0
	for _, line := range strings.Split(string(out), "\n") {
		if isGoTestName(line) {
			names = append(names, line)
			listed++
		}
	}
	if listed == 0 {
		t.Fatal("go test listed no pipeline step tests")
	}
	counts := make(map[string]int)
	for _, name := range names {
		var selectedBy []string
		for _, shard := range shards {
			if shard.selects(name) {
				selectedBy = append(selectedBy, shard.name)
				if strings.HasPrefix(name, "Test") {
					counts[shard.name]++
				}
			}
		}
		if len(selectedBy) != 1 {
			t.Errorf("%s selected by macOS shards %v, want exactly one", name, selectedBy)
		}
	}
	for _, shard := range shards {
		if counts[shard.name] == 0 {
			t.Errorf("macOS shard %q selects no test-name partition", shard.name)
		}
	}
}
