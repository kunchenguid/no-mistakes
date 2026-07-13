package agent

import (
	"runtime"
	"strings"
	"testing"
)

func resolveAgentEnv(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

func TestGitSafeEnv_DisablesInteractiveGit(t *testing.T) {
	t.Setenv("GIT_EDITOR", "vim")

	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))

	if resolved["GIT_EDITOR"] != "true" {
		t.Errorf("GIT_EDITOR = %q, want \"true\"", resolved["GIT_EDITOR"])
	}
	if resolved["GIT_SEQUENCE_EDITOR"] != "true" {
		t.Errorf("GIT_SEQUENCE_EDITOR = %q, want \"true\"", resolved["GIT_SEQUENCE_EDITOR"])
	}
	if resolved["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want \"0\"", resolved["GIT_TERMINAL_PROMPT"])
	}
}

// TestGitSafeEnv_CouplesPWDToWorkdir guards the regression where assigning
// cmd.Env dropped os/exec's automatic PWD=cmd.Dir, making os.Getwd in the agent
// report a symlink-resolved path instead of the worktree path.
func TestGitSafeEnv_CouplesPWDToWorkdir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")

	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))

	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		if resolved["PWD"] != "/somewhere/else" {
			t.Errorf("PWD = %q, want ambient PWD on %s", resolved["PWD"], runtime.GOOS)
		}
		return
	}

	if resolved["PWD"] != "/work/dir" {
		t.Errorf("PWD = %q, want \"/work/dir\"", resolved["PWD"])
	}
}

// TestGitSafeEnv_StampsGateRoleMarker locks in the ambient-authority containment
// marker: every spawned gate agent must carry NO_MISTAKES_GATE=1 so a
// cooperating orchestration harness in the target repo can recognize the gate
// agent and refuse to let it drive the fleet. If this regresses, a gate agent
// validating a firstmate-shaped repo becomes indistinguishable from a real
// fleet operator.
func TestGitSafeEnv_StampsGateRoleMarker(t *testing.T) {
	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))
	if resolved[GateRoleEnvVar] != "1" {
		t.Errorf("%s = %q, want \"1\"", GateRoleEnvVar, resolved[GateRoleEnvVar])
	}
}

// TestGitSafeEnv_GateMarkerWinsOverAmbient guards that a target repo (or a
// confused parent) cannot pre-empt the marker with its own ambient value: the
// stamp is appended last, and exec resolves duplicate keys to the last
// occurrence.
func TestGitSafeEnv_GateMarkerWinsOverAmbient(t *testing.T) {
	t.Setenv(GateRoleEnvVar, "0")
	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))
	if resolved[GateRoleEnvVar] != "1" {
		t.Errorf("%s = %q, want \"1\" (managed stamp must win over ambient)", GateRoleEnvVar, resolved[GateRoleEnvVar])
	}
}
