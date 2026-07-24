package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGateControlPolicyCoversEveryMutationEntrypoint(t *testing.T) {
	root := newRootCmd()
	cases := []struct {
		args    []string
		mutates bool
	}{
		{args: nil, mutates: true},
		{args: []string{"init"}, mutates: true},
		{args: []string{"eject"}, mutates: true},
		{args: []string{"rerun"}, mutates: true},
		{args: []string{"sync"}, mutates: true},
		{args: []string{"sync", "--recover"}, mutates: true},
		{args: []string{"sync", "--check"}, mutates: false},
		{args: []string{"axi", "run"}, mutates: true},
		{args: []string{"axi", "respond"}, mutates: true},
		{args: []string{"axi", "sync"}, mutates: true},
		{args: []string{"axi", "sync", "--recover"}, mutates: true},
		{args: []string{"axi", "sync", "--check"}, mutates: false},
		{args: []string{"axi", "abort"}, mutates: true},
		{args: []string{"axi", "status"}, mutates: false},
		{args: []string{"axi", "logs"}, mutates: false},
		{args: []string{"status"}, mutates: false},
		{args: []string{"doctor"}, mutates: false},
		{args: []string{"daemon", "stop", "--force"}, mutates: true},
	}
	for _, tc := range cases {
		cmd, _, err := root.Find(tc.args)
		if err != nil {
			t.Fatalf("find %v: %v", tc.args, err)
		}
		if contains(tc.args, "--check") {
			_ = cmd.Flags().Set("check", "true")
		} else if cmd.Flags().Lookup("check") != nil {
			_ = cmd.Flags().Set("check", "false")
		}
		if got := mutatesPipelineControl(cmd); got != tc.mutates {
			t.Errorf("%v mutates = %v, want %v (path=%s)", tc.args, got, tc.mutates, cmd.CommandPath())
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestGateContextRefusalIsStructuredActionableAndPrivacySafe(t *testing.T) {
	cmd := newAxiRunCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := emitGateContextRefusal(cmd, gatecontext.Result{Nested: true, ManagedGit: true, AgentDescendant: true, RunID: "run-safe", Phase: types.StepDocument})
	if err == nil {
		t.Fatal("expected refusal exit error")
	}
	text := out.String()
	for _, want := range []string{
		"code: nested_gate_context",
		"run: run-safe",
		"phase: document",
		"enclosing executor owns validation, push, PR, and CI",
		"no-mistakes axi status",
		"Return control to the outer executor",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"agent_pid", "peer_pid", "/worktrees/", "/repos/"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("refusal leaked %q:\n%s", forbidden, text)
		}
	}
}
