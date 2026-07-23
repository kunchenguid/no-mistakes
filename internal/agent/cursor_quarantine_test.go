package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeginCursorInstructionQuarantine_MovesAndRestores(t *testing.T) {
	cwd := t.TempDir()
	agentsBody := []byte("AYE_CAPTAIN_CANARY\n")
	claudeBody := []byte("# CLAUDE\n")
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), agentsBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), claudeBody, 0o644); err != nil {
		t.Fatal(err)
	}
	rulesDir := filepath.Join(cwd, ".cursor", "rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ruleBody := []byte("rule-canary\n")
	if err := os.WriteFile(filepath.Join(rulesDir, "gate.mdc"), ruleBody, 0o644); err != nil {
		t.Fatal(err)
	}
	// Unrelated .cursor content must stay put.
	mcpBody := []byte(`{"mcpServers":{}}`)
	if err := os.WriteFile(filepath.Join(cwd, ".cursor", "mcp.json"), mcpBody, 0o644); err != nil {
		t.Fatal(err)
	}

	q, err := beginCursorInstructionQuarantine(cwd)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, rel := range cursorInstructionSurfaces() {
		if _, err := os.Lstat(filepath.Join(cwd, rel)); !os.IsNotExist(err) {
			t.Errorf("%s still visible at CWD during quarantine (err=%v)", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cursor", "mcp.json")); err != nil {
		t.Errorf("unrelated .cursor/mcp.json must remain: %v", err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	foundQuarantine := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), cursorQuarantineDirPrefix) {
			foundQuarantine = true
			break
		}
	}
	if !foundQuarantine {
		t.Error("expected a private quarantine directory under CWD")
	}

	if err := q.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	gotAgents, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md"))
	if err != nil || string(gotAgents) != string(agentsBody) {
		t.Errorf("AGENTS.md restore = %q err=%v, want %q", gotAgents, err, agentsBody)
	}
	gotClaude, err := os.ReadFile(filepath.Join(cwd, "CLAUDE.md"))
	if err != nil || string(gotClaude) != string(claudeBody) {
		t.Errorf("CLAUDE.md restore = %q err=%v, want %q", gotClaude, err, claudeBody)
	}
	gotRule, err := os.ReadFile(filepath.Join(rulesDir, "gate.mdc"))
	if err != nil || string(gotRule) != string(ruleBody) {
		t.Errorf("rules restore = %q err=%v, want %q", gotRule, err, ruleBody)
	}
	gotMCP, err := os.ReadFile(filepath.Join(cwd, ".cursor", "mcp.json"))
	if err != nil || string(gotMCP) != string(mcpBody) {
		t.Errorf("mcp.json disturbed: %q err=%v", gotMCP, err)
	}
	entries, err = os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), cursorQuarantineDirPrefix) {
			t.Errorf("quarantine dir %q left behind after restore", e.Name())
		}
	}
}

func TestBeginCursorInstructionQuarantine_MissingSurfacesOK(t *testing.T) {
	cwd := t.TempDir()
	q, err := beginCursorInstructionQuarantine(cwd)
	if err != nil {
		t.Fatalf("begin with no surfaces: %v", err)
	}
	if err := q.Restore(); err != nil {
		t.Fatalf("restore empty: %v", err)
	}
}

func TestBeginCursorInstructionQuarantine_RestoreAfterSimulatedFailure(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("keep-me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte("claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rulesParent := filepath.Join(cwd, ".cursor")
	if err := os.MkdirAll(filepath.Join(rulesParent, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}

	q, err := beginCursorInstructionQuarantine(cwd)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Caller must restore even when the subsequent agent Run fails.
	if err := q.Restore(); err != nil {
		t.Fatalf("restore after simulated error path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing after restore-on-error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md missing after restore-on-error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rulesParent, "rules")); err != nil {
		t.Errorf(".cursor/rules missing after restore-on-error: %v", err)
	}
}

func TestIsCursorGateTarget(t *testing.T) {
	if !isCursorGateTarget("cursor") {
		t.Error("cursor target must qualify")
	}
	if isCursorGateTarget("gemini") {
		t.Error("generic ACP target must not qualify")
	}
	if isCursorGateTarget("") {
		t.Error("empty target must not qualify")
	}
}
