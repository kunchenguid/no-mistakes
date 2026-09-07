package main

import (
	"slices"
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
		"macos-latest":   1,
		"windows-latest": 3,
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
		return runnerOSCondition(step.If, "!=", "Windows")
	})

	var raceTests []workflowCommand
	for _, command := range commands {
		if command.name == "go" && slices.Equal(command.args, []string{"test", "-race", "./..."}) {
			raceTests = append(raceTests, command)
		}
	}
	if len(raceTests) != 1 {
		t.Fatalf("Unix-only go test -race ./... commands = %d, want 1; normalized commands: %#v", len(raceTests), commands)
	}
}
