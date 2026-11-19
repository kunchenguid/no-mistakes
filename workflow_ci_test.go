package main

import (
	"regexp"
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
		"macos-latest":   1,
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
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	})

	var raceTests []workflowCommand
	for _, command := range commands {
		if command.name == "go" && len(command.args) > 0 && command.args[0] == "test" {
			raceTests = append(raceTests, command)
		}
	}
	if len(raceTests) != 2 {
		t.Fatalf("Unix test invocations = %d, want two complementary partitions; normalized commands: %#v", len(raceTests), commands)
	}
	// These are complementary Go test selectors over every package: any test
	// name matches the first invocation or the second, exactly once. Keeping
	// the selector top-level also keeps each test's subtests together.
	var patterns [2]string
	for i, flag := range []string{"-run=", "-skip="} {
		args := raceTests[i].args
		if len(args) != 4 || args[0] != "test" || args[1] != "-race" || args[3] != "./..." {
			t.Fatalf("Unix partition must race-test every package: %v", args)
		}
		selector := unquoteWorkflowArg(args[2])
		if !strings.HasPrefix(selector, flag) {
			t.Fatalf("Unix partition selector = %q, want %s", selector, flag)
		}
		patterns[i] = strings.TrimPrefix(selector, flag)
		if patterns[i] == "" || strings.Contains(patterns[i], "/") {
			t.Fatalf("selector must partition top-level test names: %q", patterns[i])
		}
		if _, err := regexp.Compile(patterns[i]); err != nil {
			t.Fatalf("invalid test selector: %v", err)
		}
	}
	if patterns[0] != patterns[1] {
		t.Fatalf("Unix partitions are not complementary: %v", patterns)
	}
}
