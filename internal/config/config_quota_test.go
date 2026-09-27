package config

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// lookPathFor returns a lookPath that resolves the named binaries and reports
// exec.ErrNotFound for everything else, which is how the real probe fails when a
// harness is not installed.
func lookPathFor(binaries ...string) func(string) (string, error) {
	available := map[string]bool{}
	for _, binary := range binaries {
		available[binary] = true
	}
	return func(bin string) (string, error) {
		if available[bin] {
			return "/fake/bin/" + bin, nil
		}
		return "", fmt.Errorf("exec: %q: %w", bin, exec.ErrNotFound)
	}
}

func loadQuotaGlobal(t *testing.T, document string) *GlobalConfig {
	t.Helper()
	cfg, err := LoadGlobalFromBytes([]byte(document))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	return cfg
}

func TestLoadGlobal_QuotaAutoParsesTheCandidateList(t *testing.T) {
	cfg := loadQuotaGlobal(t, `
agent: quota-auto
agent_candidates:
  - claude
  - codex
  - opencode@zai
quota_axi_path: /opt/tools/quota-axi
`)
	if cfg.Agent != types.AgentQuotaAuto || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentQuotaAuto {
		t.Fatalf("agent = %q/%v", cfg.Agent, cfg.Agents)
	}
	want := []string{"claude", "codex", "opencode@zai"}
	if len(cfg.AgentCandidates) != len(want) {
		t.Fatalf("candidates = %v, want %v", cfg.AgentCandidates, want)
	}
	for index, candidate := range want {
		if cfg.AgentCandidates[index] != candidate {
			t.Fatalf("candidates = %v, want %v", cfg.AgentCandidates, want)
		}
	}
	if cfg.QuotaAXIPath != "/opt/tools/quota-axi" {
		t.Fatalf("quota_axi_path = %q", cfg.QuotaAXIPath)
	}
	// The key reaches merged runs too, and is inert for a repository that pins its
	// own agent.
	merged := Merge(cfg, &RepoConfig{})
	if len(merged.AgentCandidates) != 3 || merged.QuotaAXIPath != "/opt/tools/quota-axi" {
		t.Fatalf("merged = %v/%q", merged.AgentCandidates, merged.QuotaAXIPath)
	}
}

func TestLoadGlobal_DefaultsQuotaAXIPath(t *testing.T) {
	if got := DefaultGlobalConfig().QuotaAXIPath; got != "quota-axi" {
		t.Fatalf("quota_axi_path default = %q", got)
	}
}

func TestLoadGlobal_RefusesTheModeCombinationsItCannotHonor(t *testing.T) {
	cases := map[string]struct {
		document string
		want     string
	}{
		"candidates without the mode": {
			document: "agent: claude\nagent_candidates: [codex]\n",
			want:     "agent_candidates is only meaningful with agent: quota-auto",
		},
		"a candidate that is a mode": {
			document: "agent: quota-auto\nagent_candidates: [claude, auto]\n",
			want:     "names a selection mode",
		},
		"a malformed candidate": {
			document: "agent: quota-auto\nagent_candidates: ['codex@']\n",
			want:     "must name a provider after @",
		},
		"an unbounded candidate list": {
			document: "agent: quota-auto\nagent_candidates: [a1, a2, a3, a4, a5, a6, a7, a8, a9]\n",
			want:     "the limit is 8",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tc.document))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadGlobal_DeduplicatesCandidates(t *testing.T) {
	cfg := loadQuotaGlobal(t, "agent: quota-auto\nagent_candidates: [claude, claude, 'opencode@zai', 'opencode@zai']\n")
	if len(cfg.AgentCandidates) != 2 {
		t.Fatalf("candidates = %v, want duplicates dropped", cfg.AgentCandidates)
	}
}

func TestResolveAgent_QuotaAutoKeepsRunnableCandidatesInOrder(t *testing.T) {
	cfg := Merge(loadQuotaGlobal(t, "agent: quota-auto\nagent_candidates: [claude, codex, 'opencode@zai']\n"), &RepoConfig{})
	// codex is not installed on this machine; the other two are.
	if err := cfg.ResolveAgent(context.Background(), lookPathFor("claude", "opencode")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if !cfg.QuotaAuto {
		t.Fatal("the resolved config must report the quota-auto mode")
	}
	if len(cfg.Agents) != 2 || cfg.Agents[0] != types.AgentClaude || cfg.Agents[1] != types.AgentOpenCode {
		t.Fatalf("agents = %v, want the runnable candidates in the operator's order", cfg.Agents)
	}
	if cfg.Agent != types.AgentClaude {
		t.Fatalf("agent = %q, want the first runnable candidate", cfg.Agent)
	}
	// The candidate list itself is untouched: routing reads the configured
	// spelling, including an explicit provider.
	if len(cfg.AgentCandidates) != 3 {
		t.Fatalf("candidates = %v", cfg.AgentCandidates)
	}
}

func TestResolveAgent_QuotaAutoFailsClosed(t *testing.T) {
	t.Run("no runnable candidate", func(t *testing.T) {
		cfg := Merge(loadQuotaGlobal(t, "agent: quota-auto\nagent_candidates: [claude, codex]\n"), &RepoConfig{})
		err := cfg.ResolveAgent(context.Background(), lookPathFor())
		if err == nil || !strings.Contains(err.Error(), "no runnable agent found") {
			t.Fatalf("error = %v", err)
		}
		// The refusal names what was probed, which is the operator's only clue
		// when the harness binary is the thing missing.
		if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "codex") {
			t.Fatalf("error = %v, want every candidate probed", err)
		}
	})

	t.Run("mode without candidates", func(t *testing.T) {
		cfg := Merge(loadQuotaGlobal(t, "agent: quota-auto\n"), &RepoConfig{})
		err := cfg.ResolveAgent(context.Background(), lookPathFor("claude"))
		if err == nil || !strings.Contains(err.Error(), "needs agent_candidates") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("mode inside a list", func(t *testing.T) {
		cfg := Merge(loadQuotaGlobal(t, "agent: [quota-auto, claude]\nagent_candidates: [codex]\n"), &RepoConfig{})
		err := cfg.ResolveAgent(context.Background(), lookPathFor("claude", "codex"))
		if err == nil || !strings.Contains(err.Error(), "cannot be combined with other agent names") {
			t.Fatalf("error = %v", err)
		}
	})
}

// TestResolveAgent_RepoAgentPinOutranksQuotaAuto proves the mode is
// machine-local: a repository whose trusted config pins an agent gets that agent,
// and the candidate list is left unconsumed.
func TestResolveAgent_RepoAgentPinOutranksQuotaAuto(t *testing.T) {
	global := loadQuotaGlobal(t, "agent: quota-auto\nagent_candidates: [claude, codex]\n")
	cfg := Merge(global, &RepoConfig{Agent: types.AgentPi, Agents: []types.AgentName{types.AgentPi}})
	if err := cfg.ResolveAgent(context.Background(), lookPathFor("pi", "claude", "codex")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if cfg.QuotaAuto {
		t.Fatal("a repository agent pin must not resolve as quota-auto")
	}
	if cfg.Agent != types.AgentPi || len(cfg.Agents) != 1 {
		t.Fatalf("agents = %v/%q, want the repository pin", cfg.Agents, cfg.Agent)
	}
}

// TestResolveAgent_OtherModesAreUnaffected proves the new mode is opt-in: an
// ordinary single agent and an ordered fallback list resolve exactly as before,
// with no candidate list and no routing.
func TestResolveAgent_OtherModesAreUnaffected(t *testing.T) {
	single := Merge(loadQuotaGlobal(t, "agent: claude\n"), &RepoConfig{})
	if err := single.ResolveAgent(context.Background(), lookPathFor("claude")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if single.QuotaAuto || single.Agent != types.AgentClaude || len(single.Agents) != 1 {
		t.Fatalf("single = %v/%q/%v", single.QuotaAuto, single.Agent, single.Agents)
	}

	list := Merge(loadQuotaGlobal(t, "agent: [codex, grok]\n"), &RepoConfig{})
	if err := list.ResolveAgent(context.Background(), lookPathFor("codex", "grok")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if list.QuotaAuto || list.Agent != types.AgentCodex || len(list.Agents) != 2 {
		t.Fatalf("list = %v/%q/%v", list.QuotaAuto, list.Agent, list.Agents)
	}
}

// TestLoadRepo_QuotaCandidateListIsNotARepoSetting proves the routing inputs are
// machine-local: .no-mistakes.yaml has no field for them, so a pushed branch
// cannot select whose credentials and whose paid allowance a run spends.
func TestLoadRepo_QuotaCandidateListIsNotARepoSetting(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, `
agent_candidates: [codex]
quota_axi_path: /tmp/attacker-quota-axi
`)
	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	global := loadQuotaGlobal(t, "agent: quota-auto\nagent_candidates: [claude, codex]\nquota_axi_path: /opt/tools/quota-axi\n")
	merged := Merge(global, repo)
	if len(merged.AgentCandidates) != 2 || merged.AgentCandidates[1] != "codex" {
		t.Fatalf("candidates = %v, want the machine-local list", merged.AgentCandidates)
	}
	if merged.QuotaAXIPath != "/opt/tools/quota-axi" {
		t.Fatalf("quota_axi_path = %q, want the machine-local path", merged.QuotaAXIPath)
	}
	if !merged.QuotaAutoSelected() {
		t.Fatal("the machine-local mode must survive a repository that does not pin an agent")
	}

	// A repository that DOES pin an agent outranks the mode, which is what keeps
	// the trusted repository selection authoritative.
	writeRepoFile(t, dir, "agent: claude\n")
	pinning, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	pinned := Merge(global, pinning)
	if pinned.QuotaAutoSelected() {
		t.Fatal("a repository agent pin must outrank the machine-local routing mode")
	}
	if err := pinned.ResolveAgent(context.Background(), lookPathFor("claude", "codex")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if pinned.Agent != types.AgentClaude || pinned.QuotaAuto {
		t.Fatalf("resolved = %q quotaAuto=%v, want the repository pin", pinned.Agent, pinned.QuotaAuto)
	}
}

// TestRepoConfig_AgentQuotaAutoNeedsTheMachineLocalList proves the mode is
// usable only with the global candidate list: a repository that somehow carries
// the mode has nothing to route between, and resolution says so instead of
// silently falling back to a harness.
func TestRepoConfig_AgentQuotaAutoNeedsTheMachineLocalList(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "agent: quota-auto\n")
	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if repo.Agent != types.AgentQuotaAuto {
		t.Fatalf("agent = %q", repo.Agent)
	}
	merged := Merge(DefaultGlobalConfig(), repo)
	err = merged.ResolveAgent(context.Background(), func(string) (string, error) {
		return "", fs.ErrNotExist
	})
	if err == nil || !strings.Contains(err.Error(), "needs agent_candidates") {
		t.Fatalf("error = %v, want the mode refused without a candidate list", err)
	}
}

// writeRepoFile writes a .no-mistakes.yaml for a repository-config test.
func writeRepoFile(t *testing.T, dir, document string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(document), 0o644); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
}
