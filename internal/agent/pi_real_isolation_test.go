package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// TestPiAgent_RealGateIsolationNoModelCall probes the effective Pi resource
// boundary, not just the argv builder. It starts Pi 0.80.10 in RPC mode with
// poison instruction-bearing resources at automatic global and project
// locations, then invokes an explicit probe extension command. Extension
// commands are handled before agent processing, so this exercises extension,
// skill, prompt-template, context, project-trust, and system-prompt behavior
// without an LLM call. Themes are non-instruction TUI resources and RPC does not
// expose them; --no-themes is instead pinned by the exact argv tests and checked
// against the installed Pi help surface here.
//
// This is opt-in because released no-mistakes builds and most CI workers do not
// install Pi. Run with NM_TEST_REAL_PI_GATE_ISOLATION=1.
func TestPiAgent_RealGateIsolationNoModelCall(t *testing.T) {
	if os.Getenv("NM_TEST_REAL_PI_GATE_ISOLATION") != "1" {
		t.Skip("set NM_TEST_REAL_PI_GATE_ISOLATION=1 to probe installed Pi gate isolation")
	}
	if runtime.GOOS == "windows" {
		t.Skip("real Pi probe fixture uses TypeScript resource paths")
	}
	piBin, err := exec.LookPath("pi")
	if err != nil {
		t.Fatalf("find pi: %v", err)
	}
	versionCmd := exec.Command(piBin, "--version")
	versionOut, err := shellenv.CombinedOutputShellCommand(versionCmd)
	if err != nil {
		t.Fatalf("pi --version: %v: %s", err, versionOut)
	}
	if got := strings.TrimSpace(string(versionOut)); got != "0.80.10" {
		t.Fatalf("real isolation contract is verified for Pi 0.80.10, found %q", got)
	}
	helpCmd := exec.Command(piBin, "--help")
	helpOut, err := shellenv.CombinedOutputShellCommand(helpCmd)
	if err != nil {
		t.Fatalf("pi --help: %v: %s", err, helpOut)
	}
	if !strings.Contains(string(helpOut), "--no-themes") {
		t.Fatalf("installed Pi 0.80.10 help does not advertise --no-themes:\n%s", helpOut)
	}

	root := t.TempDir()
	configDir := filepath.Join(root, "pi-config")
	projectDir := filepath.Join(root, "project")
	mustMkdirAll(t,
		filepath.Join(configDir, "extensions"),
		filepath.Join(configDir, "skills", "poison-skill"),
		filepath.Join(configDir, "prompts"),
		filepath.Join(projectDir, ".pi", "extensions"),
		filepath.Join(projectDir, ".pi", "prompts"),
		filepath.Join(projectDir, ".agents", "skills", "project-poison-skill"),
	)

	autoExtensionMarker := filepath.Join(root, "automatic-extension-loaded")
	reportPath := filepath.Join(root, "probe-report.json")
	probePath := filepath.Join(root, "probe.ts")
	writeTestFile(t, filepath.Join(configDir, "settings.json"), `{
  "defaultProjectTrust": "always"
}
`)
	writeTestFile(t, filepath.Join(configDir, "extensions", "poison.ts"), `
import { writeFileSync } from "node:fs";
export default function () {
  writeFileSync(process.env.NM_PI_AUTO_EXTENSION_MARKER!, "loaded");
}
`)
	writeTestFile(t, filepath.Join(projectDir, ".pi", "extensions", "poison.ts"), `
import { writeFileSync } from "node:fs";
export default function () {
  writeFileSync(process.env.NM_PI_AUTO_EXTENSION_MARKER!, "project-loaded");
}
`)
	writeTestFile(t, filepath.Join(configDir, "skills", "poison-skill", "SKILL.md"), `---
name: poison-skill
description: NM_PI_POISON_SKILL must never enter the gate prompt
---
NM_PI_POISON_SKILL_BODY
`)
	writeTestFile(t, filepath.Join(projectDir, ".agents", "skills", "project-poison-skill", "SKILL.md"), `---
name: project-poison-skill
description: NM_PI_PROJECT_POISON_SKILL must never enter the gate prompt
---
NM_PI_PROJECT_POISON_SKILL_BODY
`)
	writeTestFile(t, filepath.Join(configDir, "prompts", "poison-template.md"), "NM_PI_POISON_TEMPLATE\n")
	writeTestFile(t, filepath.Join(projectDir, ".pi", "prompts", "project-poison-template.md"), "NM_PI_PROJECT_POISON_TEMPLATE\n")
	writeTestFile(t, filepath.Join(configDir, "AGENTS.md"), "NM_PI_POISON_GLOBAL_CONTEXT\n")
	writeTestFile(t, filepath.Join(configDir, "CLAUDE.md"), "NM_PI_POISON_GLOBAL_CLAUDE_CONTEXT\n")
	writeTestFile(t, filepath.Join(configDir, "SYSTEM.md"), "NM_PI_POISON_GLOBAL_SYSTEM\n")
	writeTestFile(t, filepath.Join(configDir, "APPEND_SYSTEM.md"), "NM_PI_POISON_GLOBAL_APPEND_SYSTEM\n")
	writeTestFile(t, filepath.Join(projectDir, "AGENTS.md"), "NM_PI_POISON_PROJECT_CONTEXT\n")
	writeTestFile(t, filepath.Join(projectDir, "CLAUDE.md"), "NM_PI_POISON_PROJECT_CLAUDE_CONTEXT\n")
	writeTestFile(t, filepath.Join(projectDir, ".pi", "SYSTEM.md"), "NM_PI_POISON_PROJECT_SYSTEM\n")
	writeTestFile(t, filepath.Join(projectDir, ".pi", "APPEND_SYSTEM.md"), "NM_PI_POISON_PROJECT_APPEND_SYSTEM\n")
	writeTestFile(t, filepath.Join(projectDir, ".pi", "settings.json"), `{
  "shellCommandPrefix": "NM_PI_POISON_PROJECT_SETTING"
}
`)
	writeTestFile(t, probePath, `
import { writeFileSync } from "node:fs";
export default function (pi: any) {
  pi.registerCommand("nm-isolation-probe", {
    description: "inspect effective startup resources without calling a model",
    handler: async (_args: string, ctx: any) => {
      const options = ctx.getSystemPromptOptions();
      const report = {
        systemPrompt: ctx.getSystemPrompt(),
        customPrompt: options.customPrompt ?? "",
        appendSystemPrompt: options.appendSystemPrompt ?? "",
        contextFiles: (options.contextFiles ?? []).map((item: any) => ({ path: item.path, content: item.content })),
        skills: (options.skills ?? []).map((item: any) => ({ name: item.name, path: item.path, description: item.description })),
        commands: pi.getCommands().map((item: any) => ({ name: item.name, source: item.source, path: item.sourceInfo?.path })),
        projectTrusted: ctx.isProjectTrusted(),
      };
      writeFileSync(process.env.NM_PI_PROBE_REPORT!, JSON.stringify(report));
      ctx.shutdown();
    },
  });
}
`)

	isolatedOutput := runRealPiResourceProbe(t, piBin, projectDir, configDir, probePath, reportPath, autoExtensionMarker, piGateIsolationArgs())
	if strings.Contains(isolatedOutput, "agent_start") || strings.Contains(isolatedOutput, "message_start") {
		t.Fatalf("probe unexpectedly started a model turn:\n%s", isolatedOutput)
	}
	if _, err := os.Stat(autoExtensionMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic extension executed under --no-extensions: %v", err)
	}

	var report struct {
		SystemPrompt       string `json:"systemPrompt"`
		CustomPrompt       string `json:"customPrompt"`
		AppendSystemPrompt string `json:"appendSystemPrompt"`
		ContextFiles       []any  `json:"contextFiles"`
		Skills             []any  `json:"skills"`
		Commands           []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"commands"`
		ProjectTrusted bool `json:"projectTrusted"`
	}
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read Pi probe report: %v\noutput:\n%s", err, isolatedOutput)
	}
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		t.Fatalf("parse Pi probe report: %v: %s", err, reportBytes)
	}
	if !strings.Contains(report.SystemPrompt, "You are an expert coding assistant operating inside pi") {
		t.Fatalf("empty custom system prompt did not retain Pi's generated base prompt: %q", report.SystemPrompt)
	}
	if report.CustomPrompt != "" || report.AppendSystemPrompt != "" {
		t.Fatalf("Pi did not retain the managed empty system-prompt inputs: custom=%q append=%q", report.CustomPrompt, report.AppendSystemPrompt)
	}
	for _, poison := range []string{
		"NM_PI_POISON_SKILL", "NM_PI_PROJECT_POISON_SKILL",
		"NM_PI_POISON_TEMPLATE", "NM_PI_PROJECT_POISON_TEMPLATE",
		"NM_PI_POISON_GLOBAL_CONTEXT", "NM_PI_POISON_GLOBAL_CLAUDE_CONTEXT",
		"NM_PI_POISON_PROJECT_CONTEXT", "NM_PI_POISON_PROJECT_CLAUDE_CONTEXT",
		"NM_PI_POISON_GLOBAL_SYSTEM", "NM_PI_POISON_GLOBAL_APPEND_SYSTEM",
		"NM_PI_POISON_PROJECT_SYSTEM", "NM_PI_POISON_PROJECT_APPEND_SYSTEM",
	} {
		if strings.Contains(report.SystemPrompt, poison) || strings.Contains(report.CustomPrompt, poison) || strings.Contains(report.AppendSystemPrompt, poison) {
			t.Errorf("Pi prompt inputs contain disabled resource marker %q", poison)
		}
	}
	if len(report.ContextFiles) != 0 {
		t.Errorf("Pi loaded context files under --no-context-files: %+v", report.ContextFiles)
	}
	if len(report.Skills) != 0 {
		t.Errorf("Pi loaded skills under --no-skills: %+v", report.Skills)
	}
	for _, command := range report.Commands {
		if command.Source == "skill" || command.Source == "prompt" {
			t.Errorf("Pi loaded disabled %s command %q", command.Source, command.Name)
		}
	}
	if report.ProjectTrusted {
		t.Error("Pi trusted project-local resources despite --no-approve")
	}
}

func runRealPiResourceProbe(t *testing.T, piBin, projectDir, configDir, probePath, reportPath, extensionMarker string, isolationArgs []string) string {
	t.Helper()
	_ = os.Remove(reportPath)
	_ = os.Remove(extensionMarker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"--mode", "rpc", "--no-session"}
	args = append(args, isolationArgs...)
	// Explicit CLI extensions remain loadable under --no-extensions by design;
	// this one only inspects startup state and handles the command before any LLM
	// turn. Production overrides containing --extension are rejected.
	args = append(args, "--extension", probePath)
	cmd := exec.CommandContext(ctx, piBin, args...)
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		"PI_CODING_AGENT_DIR="+configDir,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
		"NM_PI_AUTO_EXTENSION_MARKER="+extensionMarker,
		"NM_PI_PROBE_REPORT="+reportPath,
	)
	cmd.Stdin = strings.NewReader("{\"type\":\"prompt\",\"message\":\"/nm-isolation-probe\"}\n")
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("real Pi resource probe: %v\n%s", err, output)
	}
	return string(output)
}

func mustMkdirAll(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
