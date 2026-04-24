package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
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
	t.Setenv("PATH", os.Getenv("PATH"))

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

func TestPrepareDaemonEnvironment_PreservesExistingNMHome(t *testing.T) {
	t.Setenv("NM_HOME", "/service/root")
	t.Setenv("PATH", os.Getenv("PATH"))

	oldApply := applyShellEnvToProcess
	defer func() { applyShellEnvToProcess = oldApply }()

	applyShellEnvToProcess = func() error {
		if err := os.Setenv("NM_HOME", "/login/shell/root"); err != nil {
			return err
		}
		return os.Setenv("PATH", "/resolved/bin")
	}

	if err := prepareDaemonEnvironment(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("NM_HOME"); got != "/service/root" {
		t.Fatalf("NM_HOME = %q, want %q", got, "/service/root")
	}
	if got := os.Getenv("PATH"); got != "/resolved/bin" {
		t.Fatalf("PATH = %q, want %q", got, "/resolved/bin")
	}
}

// TestPrepareDaemonEnvironment_LogsPathSummary locks in observability for
// #143-style failures. Silent PATH regressions were the reason this bug
// took a forensic dive to diagnose - future bugs should be readable from
// the daemon log alone.
func TestPrepareDaemonEnvironment_LogsPathSummary(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))

	oldApply := applyShellEnvToProcess
	defer func() { applyShellEnvToProcess = oldApply }()
	applyShellEnvToProcess = func() error {
		return os.Setenv("PATH", "/a/bin"+string(os.PathListSeparator)+"/b/bin"+string(os.PathListSeparator)+"/c/bin")
	}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	if err := prepareDaemonEnvironment(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "daemon environment ready") {
		t.Fatalf("expected startup log line, got %q", out)
	}
	if !strings.Contains(out, "path_entries=3") {
		t.Fatalf("expected path_entries=3 in log, got %q", out)
	}
	if !strings.Contains(out, "/a/bin") {
		t.Fatalf("expected full PATH in log for debuggability, got %q", out)
	}
}
