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
