package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The generated prompt is an owned interface. These assertions implement the
// permission contract in issue #1486, not a claim about model obedience.
func assertBackingGitPrompt(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{
		"selective read-only access",
		"authentic backing Git metadata",
		"original absolute paths",
		"original `.git` pointer",
		"Git/common directories, index/history, and inherited gate markers",
		"operational databases/configuration, control sockets, credentials, trust stores, unrelated repositories, or foreign endpoints",
		"report the blocker",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("generated permission contract missing %q", want)
		}
	}
	if strings.Contains(prompt, "Do not modify or read from the gate's bare repository") {
		t.Error("blanket backing-repository read ban contradicts the metadata allowance")
	}
}

func TestExecutionContextPrompt_BackingGitContract(t *testing.T) {
	t.Parallel()
	assertBackingGitPrompt(t, ExecutionContextPromptSection(t.TempDir()))
}

// Exercise stdin, argv, and prompt-file delivery through real adapter spawns
// with synthetic CLI responses. HTTP delivery is covered by the existing
// OpenCode and RovoDev full-flow fixtures.
func TestExecutionContextPrompt_NativeDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixtures require POSIX sh")
	}
	for _, tc := range []struct {
		name     string
		newAgent func(string) Agent
		capture  string
		response string
	}{
		{"claude", func(bin string) Agent { return &claudeAgent{bin: bin} }, "cat > captured", `{"type":"result","subtype":"success","is_error":false,"result":"done"}`},
		{"codex", func(bin string) Agent { return &codexAgent{bin: bin} }, "cat > captured", `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`},
		{"grok", func(bin string) Agent { return &grokAgent{bin: bin} }, `previous=""
for arg do
  if [ "$previous" = "--prompt-file" ]; then cat "$arg" > captured; fi
  previous="$arg"
done`, `{"type":"result","subtype":"success","is_error":false,"result":"done"}`},
		{"pi", func(bin string) Agent { return &piAgent{bin: bin} }, "cat > captured", `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"done"}]}]}`},
		{"copilot", func(bin string) Agent { return &copilotAgent{bin: bin} }, "cat > captured", `{"type":"assistant.message","data":{"content":"done"}}`},
		{"antigravity", func(bin string) Agent { return &antigravityAgent{bin: bin} }, `printf '%s\n' "$@" > captured`, `{"event":"result","result":{"status":"SUCCESS","response":"done"}}`},
		{"acp", func(bin string) Agent { return &acpxAgent{bin: bin, target: "gemini"} }, "cat > captured", `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"done"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "fake-agent")
			script := "#!/bin/sh\nset -eu\n" + tc.capture + "\nprintf '%s\\n' '" + tc.response + "'\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			prompt := "Perform the assigned phase.\n" + ExecutionContextPromptSection(dir)
			a := WithSteering(tc.newAgent(bin), "")
			t.Cleanup(func() { _ = a.Close() })
			if _, err := a.Run(context.Background(), RunOpts{Prompt: prompt, CWD: dir}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "captured"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), prompt) {
				t.Fatal("adapter did not deliver the complete generated execution context")
			}
			assertBackingGitPrompt(t, string(got))
		})
	}
}

func TestExecutionContextPrompt_OpenCodeV2Delivery(t *testing.T) {
	f := &opencodeV2Fixture{pages: []string{v2Page(`{"summary":"done"}`)}, defaultModel: true}
	a := v2Agent(f.server(t))
	dir := t.TempDir()
	prompt := ExecutionContextPromptSection(dir)
	if _, err := a.Run(context.Background(), RunOpts{Prompt: prompt, CWD: dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.prompt, prompt) {
		t.Fatal("OpenCode V2 did not receive the complete generated execution context")
	}
	assertBackingGitPrompt(t, f.prompt)
}
