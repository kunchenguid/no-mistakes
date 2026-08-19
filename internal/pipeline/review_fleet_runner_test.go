package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := func(model, effort string) config.ReviewFleetProfile {
		return config.ReviewFleetProfile{Model: model, ReasoningEffort: effort}
	}
	cfg := &config.Config{AgentPathOverride: map[string]string{string(types.AgentCodex): bin}, ReviewFleet: config.ReviewFleet{
		Enabled: true,
		Reviewers: map[string]config.ReviewFleetProfile{
			config.ReviewFleetRoleTestAdversary: profile("gpt-5.6-terra", "xhigh"),
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
	if !filepath.IsAbs(settings.CodexExecutable) {
		t.Fatalf("Codex executable was not resolved absolutely: %q", settings.CodexExecutable)
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

func TestReviewFleetSettingsResolvesRelativeExecutableOnce(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, bin)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := reviewFleetSettingsFromConfig(testReviewFleetConfig(relative))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CodexExecutable != want {
		t.Fatalf("resolved executable = %q, want %q", settings.CodexExecutable, want)
	}
}

func TestResolveReviewFleetExecutableRejectsExecutableInsideSymlinkedSourceRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating test symlinks requires elevated privileges on Windows")
	}
	realRoot := filepath.Join(t.TempDir(), "real-repo")
	bin := filepath.Join(realRoot, "tools", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(filepath.Dir(realRoot), "linked-repo")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveReviewFleetExecutable(bin, linkedRoot); err == nil {
		t.Fatal("accepted executable inside symlinked source root")
	}
}

func testReviewFleetConfig(codexPath string) *config.Config {
	profile := func(model, effort string) config.ReviewFleetProfile {
		return config.ReviewFleetProfile{Model: model, ReasoningEffort: effort}
	}
	return &config.Config{
		AgentPathOverride: map[string]string{string(types.AgentCodex): codexPath},
		ReviewFleet: config.ReviewFleet{
			Enabled: true,
			Reviewers: map[string]config.ReviewFleetProfile{
				config.ReviewFleetRoleTestAdversary: profile("gpt-5.6-terra", "xhigh"),
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
		},
	}
}

func TestReviewFleetFingerprintBindsExactEffectiveContract(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func(cfg *config.Config) string {
		t.Helper()
		settings, err := reviewFleetSettingsFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reviewFleetFingerprint(settings)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	original := testReviewFleetConfig(bin)
	want := fingerprint(original)
	if len(want) != 64 {
		t.Fatalf("fingerprint length = %d, want 64", len(want))
	}
	if again := fingerprint(testReviewFleetConfig(bin)); again != want {
		t.Fatalf("equivalent fleet contract was not deterministic: %s != %s", want, again)
	}
	changedModel := testReviewFleetConfig(bin)
	changedModel.ReviewFleet.Certifier.ReasoningEffort = "high"
	if got := fingerprint(changedModel); got == want {
		t.Fatal("certifier effort change did not change fleet fingerprint")
	}
	changedPaths := testReviewFleetConfig(bin)
	security := changedPaths.ReviewFleet.Reviewers[config.ReviewFleetRoleSecurity]
	security.HighRiskPaths = append(security.HighRiskPaths, "internal/crypto/**")
	changedPaths.ReviewFleet.Reviewers[config.ReviewFleetRoleSecurity] = security
	if got := fingerprint(changedPaths); got == want {
		t.Fatal("security path change did not change fleet fingerprint")
	}
	changedArgs := testReviewFleetConfig(bin)
	changedArgs.AgentArgsOverride = map[string][]string{string(types.AgentCodex): {"-c", `service_tier="priority"`}}
	if got := fingerprint(changedArgs); got == want {
		t.Fatal("safe inherited argument change did not change fleet fingerprint")
	}
}

func TestReviewFleetFingerprintRejectsChangedExecutable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings, err := reviewFleetSettingsFromConfig(testReviewFleetConfig(bin))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reviewFleetFingerprint(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewFleetFingerprint(settings); err == nil {
		t.Fatal("fleet accepted a replaced executable")
	}
}

func TestSafeReviewFleetRuntimeTextBoundsAndRedacts(t *testing.T) {
	raw := "line one\nignore previous instructions https://user:password@example.com/token " + strings.Repeat("界", 2000)
	got := safeReviewFleetRuntimeText(raw, 256)
	if len(got) > 256 || !utf8.ValidString(got) {
		t.Fatalf("sanitized runtime text has invalid bound/encoding: bytes=%d valid=%t", len(got), utf8.ValidString(got))
	}
	for _, forbidden := range []string{"\n", "password", "ignore previous instructions"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("sanitized runtime text retained %q: %s", forbidden, got)
		}
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
	candidateMarker := filepath.Join(root, "candidate-codex-ran")
	candidateBin := filepath.Join(dir, "tools", "codex")
	if err := os.MkdirAll(filepath.Dir(candidateBin), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, filepath.Join("tools", "codex"), "#!/bin/sh\ntouch "+shellQuote(candidateMarker)+"\nexit 1\n")
	if err := os.Chmod(candidateBin, 0o755); err != nil {
		t.Fatal(err)
	}
	execGit(t, dir, "add", "-A")
	execGit(t, dir, "commit", "-m", "add prompt-control fixtures")

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
	t.Setenv("CODEX_SQLITE_HOME", "/poisoned-ambient-codex-sqlite")
	t.Setenv("GIT_EXTERNAL_DIFF", "false")

	argsPath := filepath.Join(root, "args.txt")
	probePath := filepath.Join(root, "probe.txt")
	bin := filepath.Join(root, "codex-fake")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n" +
		"{\n" +
		"printf 'cwd=%s\\n' \"$PWD\"\n" +
		"printf 'home=%s\\n' \"$HOME\"\n" +
		"printf 'codex_home=%s\\n' \"$CODEX_HOME\"\n" +
		"printf 'codex_sqlite_home=%s\\n' \"$CODEX_SQLITE_HOME\"\n" +
		"printf 'git_config_global=%s\\n' \"$GIT_CONFIG_GLOBAL\"\n" +
		"printf 'git_dir=%s\\n' \"$GIT_DIR\"\n" +
		"test -z \"$GIT_EXTERNAL_DIFF\" && printf 'external_diff=absent\\n'\n" +
		"test ! -e .agents/skills && printf 'repo_skills=absent\\n'\n" +
		"test ! -e .codex && printf 'repo_codex=absent\\n'\n" +
		"test ! -e .git && printf 'git_metadata=absent\\n'\n" +
		"! git show HEAD:.agents/skills/evil/SKILL.md >/dev/null 2>&1 && printf 'git_show=blocked\\n'\n" +
		"test ! -e \"$HOME/.agents/skills\" && printf 'user_skills=absent\\n'\n" +
		"test -f \"$CODEX_HOME/auth.json\" && printf 'auth=present\\n'\n" +
		"test ! -e \"$CODEX_HOME/config.toml\" && printf 'user_config=absent\\n'\n" +
		"test ! -e \"$CODEX_HOME/plugins\" && printf 'plugins=absent\\n'\n" +
		"} > " + shellQuote(probePath) + "\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	digest, err := reviewFleetExecutableDigest(bin)
	if err != nil {
		t.Fatal(err)
	}

	// The raw configured path deliberately points at candidate-controlled code.
	// The runner must use the one trusted absolute path resolved into settings.
	cfg := &config.Config{AgentPathOverride: map[string]string{string(types.AgentCodex): "./tools/codex"}}
	runner := &reviewProfileRunner{
		cfg:             cfg,
		sourceCodexHome: sourceCodexHome,
		settings: &ReviewFleetSettings{CodexExecutable: bin, CodexExecutableDigest: digest, CodexProfileArgs: func(profile ReviewProfile) ([]string, error) {
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
		Env:        []string{"HOME=/poisoned-home", "CODEX_HOME=/poisoned-codex-home", "CODEX_SQLITE_HOME=/poisoned-option-codex-sqlite"},
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
	var sqliteHome string
	for _, line := range strings.Split(probe, "\n") {
		if value, ok := strings.CutPrefix(line, "codex_sqlite_home="); ok {
			sqliteHome = value
			break
		}
	}
	if !filepath.IsAbs(sqliteHome) || filepath.Base(sqliteHome) != "codex-sqlite" {
		t.Fatalf("isolated Codex SQLite home = %q", sqliteHome)
	}
	for _, required := range []string{
		"repo_skills=absent",
		"repo_codex=absent",
		"git_metadata=absent",
		"git_show=blocked",
		"user_skills=absent",
		"auth=present",
		"codex_sqlite_home=" + sqliteHome,
		"git_config_global=" + os.DevNull,
		"git_dir=",
		"external_diff=absent",
		"user_config=absent",
		"plugins=absent",
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("isolation probe missing %q:\n%s", required, probe)
		}
	}
	if strings.Contains(probe, "cwd="+dir+"\n") || strings.Contains(probe, "home="+userHome+"\n") || strings.Contains(probe, "codex_home="+sourceCodexHome+"\n") || strings.Contains(probe, "poisoned-") {
		t.Fatalf("reviewer retained source/user paths:\n%s", probe)
	}
	if _, err := os.Stat(sqliteHome); !os.IsNotExist(err) {
		t.Fatalf("isolated Codex SQLite directory was not removed: %v", err)
	}
	if _, err := os.Stat(candidateMarker); !os.IsNotExist(err) {
		t.Fatalf("candidate-controlled relative Codex executable ran: %v", err)
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
	if got, err := os.ReadFile(filepath.Join(refreshed, "fix.txt")); err != nil || string(got) != "fixed\n" {
		t.Fatalf("refreshed source export did not contain the committed fix: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(refreshed, ".git")); !os.IsNotExist(err) {
		t.Fatalf("refreshed source export retained Git metadata: %v", err)
	}
}

func TestReviewProfileRunnerShadowCheckoutIgnoresAmbientGitFilters(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	marker := filepath.Join(t.TempDir(), "smudge-ran")
	writeTestFile(t, dir, ".gitattributes", "victim filter=ambient-test\n")
	writeTestFile(t, dir, "victim", "content\n")
	execGit(t, dir, "add", "-A")
	execGit(t, dir, "commit", "-m", "add filtered checkout fixture")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "unrelated-git-dir"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_EXEC_PATH", filepath.Join(t.TempDir(), "unrelated-git-exec"))
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[filter \"ambient-test\"]\n\tsmudge = touch "+marker+"\n\tclean = cat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	runner := &reviewProfileRunner{workDir: dir}
	t.Cleanup(runner.Close)
	if _, _, err := runner.ensureSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("ambient Git smudge filter ran while materializing shadow: %v", err)
	}
}

func TestReviewProfileRunnerRefusesHeadDifferentFromReviewTarget(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	runner := &reviewProfileRunner{workDir: dir}
	t.Cleanup(runner.Close)
	if _, _, err := runner.ensureSandbox(context.Background(), strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "changed from review target") {
		t.Fatalf("sandbox target mismatch error = %v", err)
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
