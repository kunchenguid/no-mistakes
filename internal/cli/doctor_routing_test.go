package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// TestDoctorValidatesRoutingContract exercises `no-mistakes doctor` and verifies
// it validates the routing contract and probes each routing runner's
// executable for provider availability, instead of recommending a legacy
// single agent.
func TestDoctorValidatesRoutingContract(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	chdir(t, t.TempDir())

	binDir := t.TempDir()
	codexPath := writeFakeDoctorBinary(t, binDir, "codex")
	claudePath := writeFakeDoctorBinary(t, binDir, "claude")

	sep := string(os.PathListSeparator)
	t.Setenv("PATH", binDir+sep+os.Getenv("PATH"))

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	// The routing contract is validated and each runner executable is probed.
	for _, want := range []string{"Routing", "contract", "codex", "claude", codexPath, claudePath} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor routing report missing %q:\n%s", want, out)
		}
	}
	// The legacy single-agent "Agents" section and its non-runner agents are gone.
	for _, gone := range []string{"Agents", "rovodev", "opencode", "copilot"} {
		if strings.Contains(out, gone) {
			t.Fatalf("doctor should no longer recommend legacy agent %q:\n%s", gone, out)
		}
	}
	if strings.Contains(out, "some checks failed") {
		t.Fatalf("healthy routing contract should not report failed checks:\n%s", out)
	}
}

func TestDoctorFailsWhenRequiredProfileHasNoAvailableCandidates(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())
	binDir := t.TempDir()
	writeFakeDoctorBinary(t, binDir, "git")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor command failed: %v\n%s", err, out)
	}
	for _, want := range []string{"profile", "no available candidates", "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor unavailable-provider report missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorAppliesPinnedTrustedRouteBeforeCheckingAvailability(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	binDir := t.TempDir()
	codexExecutable := "doctor-codex"
	writeFakeDoctorBinary(t, binDir, codexExecutable)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	routing := config.DefaultRoutingConfig()
	routing.Runners[types.RunnerCodex] = config.RunnerSpec{
		Executable:    codexExecutable,
		FailureDomain: types.FailureDomainOpenAI,
	}
	routing.Runners[types.RunnerClaude] = config.RunnerSpec{
		Executable:    "doctor-claude-definitely-missing",
		FailureDomain: types.FailureDomainAnthropic,
	}
	authority := routing.Profiles[config.ProfileAuthorityStrong]
	authority.Candidates = []config.Candidate{{
		Runner: types.RunnerClaude,
		Model:  "claude-fable-5",
		Effort: types.EffortXHigh,
	}}
	routing.Profiles[config.ProfileAuthorityStrong] = authority
	for purpose := range routing.Routes {
		routing.Routes[purpose] = config.Route{config.ProfileReviewStrong}
	}
	globalYAML, err := yaml.Marshal(struct {
		Routing config.RoutingConfig `yaml:"routing"`
	}{Routing: routing})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), globalYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, "", "git", "init", "--bare", origin)
	run(t, work, "git", "init", "--initial-branch=main")
	run(t, work, "git", "config", "user.email", "test@test.com")
	run(t, work, "git", "config", "user.name", "Test")
	run(t, work, "git", "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("routes:\n  branch_commit_suggestion: authority_strong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".no-mistakes.yaml")
	run(t, work, "git", "commit", "-m", "trusted doctor route")
	run(t, work, "git", "push", "origin", "main:refs/heads/main")

	run(t, work, "git", "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("routes:\n  branch_commit_suggestion: fix_fast\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".no-mistakes.yaml")
	run(t, work, "git", "commit", "-m", "feature doctor route")
	chdir(t, work)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor command failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "authority_strong has no available candidates") {
		t.Fatalf("doctor did not validate pinned trusted route availability:\n%s", out)
	}
	wantGateDiagnostic := `routing profile "authority_strong" has no runnable candidate (looked for: doctor-claude-definitely-missing); the gate cannot validate without a configured runner`
	if !strings.Contains(out, wantGateDiagnostic) {
		t.Fatalf("doctor gate validation did not use the trusted effective route; missing %q:\n%s", wantGateDiagnostic, out)
	}
	if strings.Contains(out, "fix_fast has no available candidates") {
		t.Fatalf("doctor honored the checked-out feature route instead of trusted policy:\n%s", out)
	}
	if !strings.Contains(out, "some checks failed") {
		t.Fatalf("doctor reported success despite unavailable trusted route:\n%s", out)
	}
}

func TestDoctorTrustedOverrideRemovesUnavailableGlobalRoute(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	binDir := t.TempDir()
	codexExecutable := "doctor-codex"
	writeFakeDoctorBinary(t, binDir, codexExecutable)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	routing := config.DefaultRoutingConfig()
	routing.Runners[types.RunnerCodex] = config.RunnerSpec{
		Executable:    codexExecutable,
		FailureDomain: types.FailureDomainOpenAI,
	}
	routing.Runners[types.RunnerClaude] = config.RunnerSpec{
		Executable:    "doctor-claude-definitely-missing",
		FailureDomain: types.FailureDomainAnthropic,
	}
	authority := routing.Profiles[config.ProfileAuthorityStrong]
	authority.Candidates = []config.Candidate{{
		Runner: types.RunnerClaude,
		Model:  "claude-fable-5",
		Effort: types.EffortXHigh,
	}}
	routing.Profiles[config.ProfileAuthorityStrong] = authority
	for purpose := range routing.Routes {
		routing.Routes[purpose] = config.Route{config.ProfileReviewStrong}
	}
	routing.Routes[types.PurposeBranchCommitSuggestion] = config.Route{config.ProfileAuthorityStrong}
	globalYAML, err := yaml.Marshal(struct {
		Routing config.RoutingConfig `yaml:"routing"`
	}{Routing: routing})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), globalYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, "", "git", "init", "--bare", origin)
	run(t, work, "git", "init", "--initial-branch=main")
	run(t, work, "git", "config", "user.email", "test@test.com")
	run(t, work, "git", "config", "user.name", "Test")
	run(t, work, "git", "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("routes:\n  branch_commit_suggestion: review_strong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".no-mistakes.yaml")
	run(t, work, "git", "commit", "-m", "trusted doctor route")
	run(t, work, "git", "push", "origin", "main:refs/heads/main")

	run(t, work, "git", "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("routes:\n  branch_commit_suggestion: authority_strong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".no-mistakes.yaml")
	run(t, work, "git", "commit", "-m", "untrusted doctor route")
	chdir(t, work)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor command failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "every routed profile has a runnable candidate") {
		t.Fatalf("doctor did not validate the trusted effective routing view:\n%s", out)
	}
	for _, unexpected := range []string{
		"authority_strong has no available candidates",
		`routing profile "authority_strong" has no runnable candidate`,
		"some checks failed",
	} {
		if strings.Contains(out, unexpected) {
			t.Fatalf("doctor honored unavailable global or untrusted routes (%q):\n%s", unexpected, out)
		}
	}
}

func writeFakeDoctorBinary(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(dir, name+".cmd")
		if err := os.WriteFile(dst, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}
