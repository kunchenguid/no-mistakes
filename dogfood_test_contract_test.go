package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// Local Test is targeted validation of the requested intent. This repository
// must not dogfood a complete-suite commands.test override (historically
// go test -race ./...), which burned multi-hour local walks while remote CI
// already owns the race-enabled full suite.
func TestDogfoodConfig_NoBroadLocalTestCommand(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRepo(root)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if got := strings.TrimSpace(cfg.Commands.Test); got != "" {
		t.Fatalf("dogfood commands.test = %q, want empty so local Test stays agent-targeted; put broad regression in remote CI", got)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatalf("read .no-mistakes.yaml: %v", err)
	}
	content := string(raw)
	for _, forbid := range []string{
		`test: "go test -race ./..."`,
		`test: 'go test -race ./...'`,
		`test: go test -race ./...`,
	} {
		if strings.Contains(content, forbid) {
			t.Fatalf(".no-mistakes.yaml still configures broad local Test %q", forbid)
		}
	}
}

// commands.test contract ownership lives in repo-config.md: targeted local
// validation, not CI-parity complete-suite configuration, and no brittle
// shell-heuristics claiming to detect "broad" commands.
func TestRepoConfigDocs_CommandsTestIsTargetedContract(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("docs", "src", "content", "docs", "reference", "repo-config.md"))
	if err != nil {
		t.Fatalf("read repo-config.md: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"### commands.test",
		"targeted validation",
		"not a CI-parity repository-wide regression command",
		"Broad regression belongs in remote CI",
		"does not guess whether an arbitrary shell string is \"too broad\"",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("repo-config.md commands.test contract missing %q", want)
		}
	}
}

// Remote CI must keep the complete race-enabled Go suite as the broad
// regression owner after local Test stops duplicating it.
func TestCIWorkflow_RetainsFullRaceSuiteAsBroadRegressionOwner(t *testing.T) {
	job := ciTestJob(t)
	commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	})
	for _, command := range commands {
		if command.name != "go" || len(command.args) == 0 || command.args[0] != "test" {
			continue
		}
		if slices.Contains(command.args, "-race") && slices.Contains(command.args, "-timeout=15m") &&
			command.args[len(command.args)-1] == "./..." {
			return
		}
	}
	t.Fatalf("CI workflow must run go test -race -timeout=15m ./... on a non-Windows step as the broad regression owner; normalized commands: %#v", commands)
}
