package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublishChannelsWorkflowRefreshesManifestWithoutRESTForEndUsers(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/publish-channels.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var wf struct {
		On          map[string]any `yaml:"on"`
		Permissions struct {
			Contents string `yaml:"contents"`
		} `yaml:"permissions"`
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	if _, ok := wf.On["workflow_call"]; !ok {
		t.Fatal("publish-channels must be a reusable workflow so release.yml can invoke it after finalize (GITHUB_TOKEN release events do not cascade)")
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Fatal("publish-channels workflow must allow workflow_dispatch so a manifest can be published without a new versioned release")
	}
	release, _ := wf.On["release"].(map[string]any)
	types, _ := release["types"].([]any)
	gotTypes := make([]string, 0, len(types))
	for _, item := range types {
		s, _ := item.(string)
		gotTypes = append(gotTypes, s)
	}
	got := strings.Join(gotTypes, ",")
	for _, want := range []string{"released", "prereleased", "edited"} {
		found := false
		for _, have := range gotTypes {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("release types = %q, want %q so promoting a prerelease to latest refreshes stable", got, want)
		}
	}
	if wf.Permissions.Contents != "write" {
		t.Fatalf("contents permission = %q, want write to upload the manifest asset", wf.Permissions.Contents)
	}
	job, ok := wf.Jobs["publish-channels"]
	if !ok {
		t.Fatal("missing publish-channels job")
	}
	for _, tc := range []struct {
		name    string
		context workflowConditionContext
		wantRun bool
	}{
		{name: "channels release", context: workflowConditionContext{EventName: "release", ReleaseTagName: "channels"}, wantRun: false},
		{name: "version release", context: workflowConditionContext{EventName: "release", ReleaseTagName: "v1.72.0"}, wantRun: true},
		{name: "workflow call", context: workflowConditionContext{EventName: "workflow_call"}, wantRun: true},
		{name: "workflow dispatch", context: workflowConditionContext{EventName: "workflow_dispatch"}, wantRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateWorkflowCondition(job.If, tc.context)
			if err != nil {
				t.Fatalf("evaluate publish-channels condition: %v", err)
			}
			if got != tc.wantRun {
				t.Fatalf("publish-channels runs = %t, want %t", got, tc.wantRun)
			}
		})
	}
	var sawPublish bool
	for _, step := range job.Steps {
		if step.Run == "go run ./cmd/publish-channels" {
			sawPublish = true
			if step.Env["GH_TOKEN"] != "${{ github.token }}" {
				t.Fatalf("publisher GH_TOKEN = %q, want the workflow token", step.Env["GH_TOKEN"])
			}
			if step.Env["GH_REPO"] != "${{ github.repository }}" {
				t.Fatalf("publisher GH_REPO = %q, want github.repository", step.Env["GH_REPO"])
			}
		}
	}
	if !sawPublish {
		t.Fatal("publish-channels job must run go run ./cmd/publish-channels")
	}
}
