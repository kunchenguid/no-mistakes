package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidateReviewFleetIsolationRejectsMutatingOverrides(t *testing.T) {
	if _, err := validateReviewFleetIsolation([]string{
		"--dangerously-bypass-approvals-and-sandbox",
		"--sandbox",
		"workspace-write",
	}); err == nil {
		t.Fatal("unsafe review-fleet args were accepted")
	}
	safe := []string{
		"-m", "gpt-test",
		"-c", `model_reasoning_effort="high"`,
		"--sandbox", "read-only",
		"--ephemeral",
		"-c", "project_doc_max_bytes=0",
		"--ignore-rules",
		"--ignore-user-config",
	}
	if _, err := validateReviewFleetIsolation(safe); err != nil {
		t.Fatalf("safe review-fleet args rejected: %v", err)
	}
}

func TestReviewFleetSettingsFromConfigUsesFixedRolesAndEscalatedArgs(t *testing.T) {
	profile := func(model, effort string) config.ReviewFleetProfile {
		return config.ReviewFleetProfile{Model: model, ReasoningEffort: effort}
	}
	cfg := &config.Config{ReviewFleet: config.ReviewFleet{
		Enabled: true,
		Reviewers: map[string]config.ReviewFleetProfile{
			config.ReviewFleetRoleTestAdversary: profile("gpt-5.6-luna", "max"),
			config.ReviewFleetRoleCorrectness:   profile("gpt-5.6-terra", "high"),
			config.ReviewFleetRoleArchitecture:  profile("gpt-5.6-terra", "high"),
			config.ReviewFleetRoleSecurity: {
				Model:                    "gpt-5.6-terra",
				ReasoningEffort:          "high",
				HighRiskPaths:            []string{"internal/auth/**"},
				EscalatedReasoningEffort: "xhigh",
			},
		},
		Consolidator: profile("gpt-5.6-terra", "high"),
		Certifier:    profile("gpt-5.6-sol", "xhigh"),
	}}

	settings, err := reviewFleetSettingsFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := []string{"test-adversary", "correctness", "architecture", "security"}
	for i, want := range wantRoles {
		if got := settings.Reviewers[i].Role; got != want {
			t.Fatalf("reviewer %d role = %q, want %q", i, got, want)
		}
	}
	if settings.Certifier.Role != config.ReviewFleetProfileCertifier || settings.Certifier.Model != "gpt-5.6-sol" {
		t.Fatalf("certifier = %#v", settings.Certifier)
	}
	security := settings.Reviewers[3]
	security.SecurityEscalated = true
	args, err := settings.CodexProfileArgs(security)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "gpt-5.6-terra") || !strings.Contains(joined, `model_reasoning_effort="xhigh"`) {
		t.Fatalf("escalated security args = %v", args)
	}
}

func TestReviewProfileRunnerIsColdAndSuppressesProjectSettings(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	bin := filepath.Join(dir, "codex-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{AgentPathOverride: map[string]string{string(types.AgentCodex): bin}}
	runner := &reviewProfileRunner{
		cfg: cfg,
		settings: &ReviewFleetSettings{CodexProfileArgs: func(profile ReviewProfile) ([]string, error) {
			return []string{
				"-m", profile.Model,
				"-c", `model_reasoning_effort="` + profile.Reasoning + `"`,
				"--sandbox", "read-only",
				"--ephemeral",
				"-c", "project_doc_max_bytes=0",
				"--ignore-rules",
				"--ignore-user-config",
			}, nil
		}},
		workDir: dir,
	}
	result, err := runner.Run(context.Background(), ReviewProfile{Role: "security", Model: "gpt-test", Reasoning: "high"}, agent.RunOpts{
		CWD:        dir,
		Session:    &agent.SessionRef{ID: "must-not-resume"},
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Output) == 0 {
		t.Fatal("runner returned no structured output")
	}
	argsRaw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(argsRaw)
	for _, forbidden := range []string{"resume", "must-not-resume", "dangerously-bypass-approvals-and-sandbox", "workspace-write"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("cold/read-only runner args contain %q: %s", forbidden, args)
		}
	}
	for _, required := range []string{"--sandbox\nread-only", "--ephemeral", "project_doc_max_bytes=0", "--ignore-rules", "--ignore-user-config", "gpt-test", `model_reasoning_effort="high"`} {
		if !strings.Contains(args, required) {
			t.Fatalf("runner args missing %q: %s", required, args)
		}
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
