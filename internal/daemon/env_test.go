package daemon

import (
	"os"
	"testing"
)

func TestPrepareDaemonEnvironment_RemovesClaudeSessionVarsAndAppliesShellEnv(t *testing.T) {
	for key, value := range map[string]string{
		"CLAUDECODE":                       "1",
		"CLAUDE_CODE_ENTRYPOINT":           "shell",
		"CLAUDE_CODE_ENTRY_POINT":          "shell",
		"CLAUDE_CODE_SESSION_ID":           "session",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN": "token",
	} {
		t.Setenv(key, value)
	}

	oldApply := applyShellEnvToProcess
	defer func() { applyShellEnvToProcess = oldApply }()

	applied := false
	applyShellEnvToProcess = func() error {
		applied = true
		return os.Setenv("PATH", "/resolved/bin")
	}

	if err := prepareDaemonEnvironment(); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("expected shell env application")
	}
	for _, key := range []string{
		"CLAUDECODE",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_ENTRY_POINT",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN",
	} {
		if got := os.Getenv(key); got != "" {
			t.Fatalf("expected %s to be cleared, got %q", key, got)
		}
	}
	if got := os.Getenv("PATH"); got != "/resolved/bin" {
		t.Fatalf("expected applied PATH, got %q", got)
	}
}
