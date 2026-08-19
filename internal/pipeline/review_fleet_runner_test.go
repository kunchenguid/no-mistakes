package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
		"-c", `shell_environment_policy.inherit="core"`,
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

func TestReviewProfileRunnerIsColdAndIsolatesSkillsPluginsAndEnvironment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills", "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, filepath.Join(".agents", "skills", "evil", "SKILL.md"), "malicious repository skill\n")
	writeTestFile(t, dir, filepath.Join(".codex", "config.toml"), "model = 'malicious'\n")
	execGit(t, dir, "add", "-A")
	execGit(t, dir, "commit", "-m", "add prompt-control fixtures")
	wantHead := strings.TrimSpace(gitCommandOutput(t, dir, "rev-parse", "HEAD"))

	userHome := filepath.Join(root, "user-home")
	sourceCodexHome := filepath.Join(userHome, ".codex")
	if err := os.MkdirAll(filepath.Join(userHome, ".agents", "skills", "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceCodexHome, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".agents", "skills", "evil", "SKILL.md"), []byte("malicious user skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCodexHome, "auth.json"), []byte(`{"token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCodexHome, "config.toml"), []byte("model = 'malicious'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCodexHome, "plugins", "evil.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", userHome)
	t.Setenv("CODEX_HOME", sourceCodexHome)

	argsPath := filepath.Join(root, "args.txt")
	probePath := filepath.Join(root, "probe.txt")
	bin := filepath.Join(root, "codex-fake")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n" +
		"{\n" +
		"printf 'cwd=%s\\n' \"$PWD\"\n" +
		"printf 'home=%s\\n' \"$HOME\"\n" +
		"printf 'codex_home=%s\\n' \"$CODEX_HOME\"\n" +
		"printf 'head=%s\\n' \"$(git rev-parse HEAD)\"\n" +
		"printf 'status=%s\\n' \"$(git status --porcelain)\"\n" +
		"test ! -e .agents/skills && printf 'repo_skills=absent\\n'\n" +
		"test ! -e .codex && printf 'repo_codex=absent\\n'\n" +
		"test ! -e \"$HOME/.agents/skills\" && printf 'user_skills=absent\\n'\n" +
		"test -f \"$CODEX_HOME/auth.json\" && printf 'auth=present\\n'\n" +
		"test ! -e \"$CODEX_HOME/config.toml\" && printf 'user_config=absent\\n'\n" +
		"test ! -e \"$CODEX_HOME/plugins\" && printf 'plugins=absent\\n'\n" +
		"test ! -e .git/objects/info/alternates && printf 'alternates=absent\\n'\n" +
		"test -z \"$(git remote)\" && printf 'remotes=absent\\n'\n" +
		"} > " + shellQuote(probePath) + "\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{AgentPathOverride: map[string]string{string(types.AgentCodex): bin}}
	runner := &reviewProfileRunner{
		cfg:             cfg,
		sourceCodexHome: sourceCodexHome,
		settings: &ReviewFleetSettings{CodexProfileArgs: func(profile ReviewProfile) ([]string, error) {
			return []string{
				"-m", profile.Model,
				"-c", `model_reasoning_effort="` + profile.Reasoning + `"`,
				"--sandbox", "read-only",
				"--ephemeral",
				"-c", "project_doc_max_bytes=0",
				"-c", `shell_environment_policy.inherit="core"`,
				"--ignore-rules",
				"--ignore-user-config",
			}, nil
		}},
		workDir: dir,
	}
	t.Cleanup(runner.Close)
	result, err := runner.Run(context.Background(), ReviewProfile{Role: "security", Model: "gpt-test", Reasoning: "high"}, agent.RunOpts{
		CWD:        dir,
		Env:        []string{"HOME=/poisoned-home", "CODEX_HOME=/poisoned-codex-home"},
		Session:    &agent.SessionRef{ID: "must-not-resume"},
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Output) == 0 {
		t.Fatal("runner returned no structured output")
	}
	probeRaw, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeRaw)
	for _, required := range []string{
		"head=" + wantHead,
		"status=",
		"repo_skills=absent",
		"repo_codex=absent",
		"user_skills=absent",
		"auth=present",
		"user_config=absent",
		"plugins=absent",
		"alternates=absent",
		"remotes=absent",
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("isolation probe missing %q:\n%s", required, probe)
		}
	}
	if strings.Contains(probe, "cwd="+dir+"\n") || strings.Contains(probe, "home="+userHome+"\n") || strings.Contains(probe, "codex_home="+sourceCodexHome+"\n") {
		t.Fatalf("reviewer retained source/user paths:\n%s", probe)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, dir, "status", "--porcelain")); got != "" {
		t.Fatalf("source worktree changed during review: %q", got)
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
	for _, required := range []string{"--sandbox\nread-only", "--ephemeral", "project_doc_max_bytes=0", `shell_environment_policy.inherit="core"`, "--ignore-rules", "--ignore-user-config", "gpt-test", `model_reasoning_effort="high"`} {
		if !strings.Contains(args, required) {
			t.Fatalf("runner args missing %q: %s", required, args)
		}
	}
	sandboxRoot := runner.sandboxRoot
	runner.Close()
	if _, err := os.Stat(sandboxRoot); !os.IsNotExist(err) {
		t.Fatalf("review isolation root was not removed: %v", err)
	}
}

func TestReviewProfileRunnerSharesOneShadowAndRefreshesAfterFixCommit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	runner := &reviewProfileRunner{workDir: dir}
	t.Cleanup(runner.Close)

	const callers = 8
	dirs := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			checkout, _, err := runner.ensureSandbox(context.Background())
			if err != nil {
				errs <- err
				return
			}
			dirs <- checkout
		}()
	}
	wg.Wait()
	close(dirs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for checkout := range dirs {
		if first == "" {
			first = checkout
		}
		if checkout != first {
			t.Fatalf("parallel reviewers received different shadows: %q and %q", first, checkout)
		}
	}
	if first == "" {
		t.Fatal("parallel reviewers received no shadow checkout")
	}

	writeTestFile(t, dir, "fix.txt", "fixed\n")
	execGit(t, dir, "add", "-A")
	execGit(t, dir, "commit", "-m", "apply review fix")
	wantHead := strings.TrimSpace(gitCommandOutput(t, dir, "rev-parse", "HEAD"))
	refreshed, _, err := runner.ensureSandbox(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed == first {
		t.Fatal("fix commit reused the stale review shadow")
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("stale review shadow was not removed: %v", err)
	}
	if got := strings.TrimSpace(gitCommandOutput(t, refreshed, "rev-parse", "HEAD")); got != wantHead {
		t.Fatalf("refreshed shadow head = %s, want %s", got, wantHead)
	}
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
