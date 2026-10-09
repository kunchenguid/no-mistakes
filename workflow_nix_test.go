package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNixWorkflowBuildsAndChecksTheFlake(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "nix.yml"))
	if err != nil {
		t.Fatalf("read Nix workflow: %v", err)
	}
	var wf wfDoc
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse Nix workflow: %v", err)
	}
	job, ok := wf.Jobs["nix"]
	if !ok {
		t.Fatal("Nix workflow has no nix job")
	}
	if !slices.ContainsFunc(job.Steps, func(step wfStep) bool {
		return strings.HasPrefix(step.Uses, "DeterminateSystems/nix-installer-action@")
	}) {
		t.Fatal("nix job must install Nix")
	}
	var commands []string
	for _, command := range workflowCommandsMatching(job.Steps, func(wfStep) bool { return true }) {
		commands = append(commands, strings.Join(append([]string{command.name}, command.args...), " "))
	}
	for _, want := range []string{
		"nix flake check --all-systems --no-build",
		"set -o pipefail",
		"./result/bin/no-mistakes --version",
		"nix-build default.nix",
	} {
		if !slices.ContainsFunc(commands, func(command string) bool { return strings.HasPrefix(command, want) }) {
			t.Errorf("nix job must run %q; normalized commands: %q", want, commands)
		}
	}
}

func TestNixWorkflowRunsOnEveryVendorHashInput(t *testing.T) {
	pr, ok := loadWorkflowPullRequest(t, filepath.Join(".github", "workflows", "nix.yml"))
	if !ok {
		t.Fatal("Nix workflow has no pull_request trigger")
	}
	paths, _ := pr["paths"].([]any)
	for _, want := range []string{"go.mod", "go.sum", "package.nix", "flake.nix", "flake.lock", "default.nix"} {
		if !slices.Contains(paths, any(want)) {
			t.Errorf("Nix workflow pull_request paths must include %q, got %v", want, paths)
		}
	}
}
