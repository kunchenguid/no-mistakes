package main

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// The repository-wide Actions actor policy protects the persistent local runner
// from workflows modified by external contributors. These checks also protect
// routing, token scope, and same-repository PR admission.
func TestCIWorkflowUsesDedicatedLocalRunner(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request", "push", "workflow_dispatch"} {
		if _, ok := settings.On[event]; !ok {
			t.Errorf("missing CI trigger %s", event)
		}
	}
	if settings.Permissions["contents"] != "read" || len(settings.Permissions) != 1 {
		t.Fatalf("CI permissions = %v, want only contents: read", settings.Permissions)
	}
	wf := loadCIWorkflowDoc(t)
	for _, name := range []string{"check", "test", "e2e"} {
		if wf.Jobs[name] == nil {
			t.Fatalf("missing %s job", name)
		}
	}
	for name, job := range wf.Jobs {
		if !slices.Equal(wfList(job.RunsOn), []string{"self-hosted", "Linux", "X64", "king-server", "no-mistakes"}) {
			t.Errorf("%s runner routing = %v", name, job.RunsOn)
		}
		if job.If != "github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository" {
			t.Errorf("%s must refuse external fork PR code before runner admission: %s", name, job.If)
		}
		if job.TimeoutMinutes < 1 || job.TimeoutMinutes > 45 {
			t.Errorf("%s has unbounded runtime", name)
		}
		checkout := 0
		for _, step := range job.Steps {
			if step.Uses != "actions/checkout@v6" {
				continue
			}
			checkout++
			if step.With["persist-credentials"] != "false" || step.With["ref"] != "${{ github.event.pull_request.head.sha || github.sha }}" {
				t.Errorf("%s must test the exact head without persisted credentials: %v", name, step.With)
			}
		}
		if checkout != 1 {
			t.Errorf("%s checkout count = %d", name, checkout)
		}
	}
}

func TestCIWorkflowRunsEndToEndJourneys(t *testing.T) {
	job := loadCIWorkflowDoc(t).Jobs["e2e"]
	if job == nil {
		t.Fatal("CI workflow has no e2e job")
	}
	for _, command := range workflowCommandsMatching(job.Steps, func(wfStep) bool { return true }) {
		if command.name == "make" && slices.Equal(command.args, []string{"e2e"}) {
			return
		}
	}
	t.Fatal("local CI must exercise make e2e")
}
