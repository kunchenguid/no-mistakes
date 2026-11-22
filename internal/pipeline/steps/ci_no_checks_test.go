package steps

import "testing"

func TestWorkflowReportsOnPR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		workflow  string
		reports   bool
		wantEvent string
	}{
		{"push scalar", "on: push\n", true, "push"},
		{"pull_request in a list", "on: [issues, pull_request]\n", true, "pull_request"},
		{"pull_request_target map", "on:\n  pull_request_target:\n    types: [opened]\n", true, "pull_request_target"},
		{"merge queue", "on:\n  merge_group:\n", true, "merge_group"},
		{"workflow_run counts as reporting", "on:\n  workflow_run:\n    workflows: [CI]\n", true, "workflow_run"},
		{"an event this version does not know", "on: some_future_event\n", true, "some_future_event"},
		{"issues only", "on:\n  issues:\n    types: [opened]\n", false, ""},
		{"bookkeeping triggers only", "on:\n  schedule:\n    - cron: '0 0 * * *'\n  workflow_dispatch:\n  issue_comment:\n", false, ""},
		{"reusable workflow only", "on:\n  workflow_call:\n", false, ""},
		{"quoted on key", "\"on\": [release, label]\n", false, ""},
		{"no trigger", "name: x\njobs: {}\n", true, "no declared trigger"},
		{"empty trigger list", "on: []\n", true, "an empty trigger list"},
		{"unparseable", "on: [push\n", true, "an unparseable workflow file"},
		{"not a mapping", "- on: push\n", true, "an unrecognized workflow shape"},
		{"nested trigger list", "on: [[push]]\n", true, "an unrecognized trigger list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			event, reports := workflowReportsOnPR([]byte(tc.workflow))
			if reports != tc.reports || event != tc.wantEvent {
				t.Fatalf("workflowReportsOnPR(%q) = (%q, %v), want (%q, %v)", tc.workflow, event, reports, tc.wantEvent, tc.reports)
			}
		})
	}
}
