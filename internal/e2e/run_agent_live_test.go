//go:build e2e

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCallerSelectedAgentGuardsReceiptsAndRerun(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeReviewAgentsRoutingScenario(t)})
	commitTrustedRepoConfig(t, h, "disable_project_settings: true\n")
	configPath := filepath.Join(h.NMHome, "config.yaml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	before = []byte(strings.Replace(string(before), "agent_path_override:\n", "agent_path_override:\n  pi: "+filepath.Join(h.BinDir, "missing-pi")+"\n", 1))
	if err := os.WriteFile(configPath, before, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/caller-guards"
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "probe caller selection guards")
	proofArgs := []string{"axi", "run", "--agent", "codex", "--intent", "validate caller guards", "--launch-nonce", "caller-proof", "--validation-generation", "caller-generation"}
	out, err := h.Run(proofArgs...)
	if err != nil || !strings.Contains(out, "launch_receipt:") || !strings.Contains(out, "requested_agent: codex") {
		t.Fatalf("proof launch: %v\n%s", err, out)
	}
	t.Logf("nonce-bound CLI launch:\n%s", out)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	invs := invocationsForRun(t, h, parked.ID)
	if len(invs) == 0 {
		t.Fatal("selected launch recorded no subprocesses")
	}
	for _, inv := range invs {
		args := strings.Join(inv.Args, " ")
		if inv.Agent != "codex" || !strings.Contains(args, "project_doc_max_bytes=0") || !strings.Contains(args, "--ignore-rules") {
			t.Fatalf("selected backend lost trusted instruction suppression: agent=%s args=%v", inv.Agent, inv.Args)
		}
		t.Logf("captured selected Codex subprocess retains project_doc_max_bytes=0 and --ignore-rules")
	}
	if out, err := h.Run(proofArgs...); err != nil || !strings.Contains(out, parked.ID) || !strings.Contains(out, "disposition: reused") {
		t.Fatalf("matching receipt replay: %v\n%s", err, out)
	} else {
		t.Logf("matching receipt replay:\n%s", out)
	}
	conflict := append([]string(nil), proofArgs...)
	conflict[3] = "claude"
	if out, err := h.Run(conflict...); err == nil || !strings.Contains(out, "conflicting launch agent") {
		t.Fatalf("conflicting receipt replay: %v\n%s", err, out)
	} else {
		t.Logf("conflicting receipt replay refused:\n%s", out)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"rerun", "--agent", "auto"}, "supported explicit backend"},
		{[]string{"rerun", "--agent", "/untrusted/agent"}, "supported explicit backend"},
		{[]string{"rerun", "--agent", "acp:untrusted"}, "supported explicit backend"},
		{[]string{"rerun", "--agent="}, "nonempty"},
		{[]string{"rerun", "--agent", "codex", "--agent", "claude"}, "only once"},
		{[]string{"rerun", "--agent", "pi"}, "missing-pi"},
		{[]string{"rerun", "--agent", "grok"}, "does not neutralize"},
		{[]string{"rerun", "--agent", "codex", "--model", "openai/pi-only", "--effort", "high"}, "conflicts"},
	} {
		out, err := h.Run(tc.args...)
		if err == nil || !strings.Contains(out, tc.want) {
			t.Fatalf("guard %v: %v\n%s", tc.args, err, out)
		}
		assertParkedRunIntact(t, h, parked.ID, branch)
		t.Logf("%v refused with active run unchanged:\n%s", tc.args, out)
	}
	// The database's durable contract must refuse a rewrite even while the
	// daemon owns the live run, rather than merely hiding a setter in Go.
	database, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("UPDATE runs SET requested_agent = 'claude' WHERE id = ?", parked.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("stored backend rewrite: %v", err)
	} else {
		t.Logf("live persisted backend rewrite refused: %v", err)
	}
	if out, err := h.Run("rerun", "--agent", "claude"); err != nil {
		t.Fatalf("selected rerun: %v\n%s", err, out)
	} else {
		t.Logf("explicit Claude rerun:\n%s", out)
	}
	rerun := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if rerun.ID == parked.ID || rerun.RequestedAgent != types.AgentClaude || h.RunInfo(parked.ID).RequestedAgent != types.AgentCodex {
		t.Fatalf("rerun changed the previous selection or lost the new one: %+v", rerun)
	}
	if out, err := h.Run("rerun"); err != nil {
		t.Fatalf("configured rerun: %v\n%s", err, out)
	}
	configured := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if configured.ID == rerun.ID || configured.RequestedAgent != "" {
		t.Fatalf("flagless rerun inherited a backend pin: %+v", configured)
	}
	for _, run := range []string{rerun.ID, configured.ID} {
		invs := invocationsForRun(t, h, run)
		if len(invs) == 0 {
			t.Fatal("rerun launched no subprocesses")
		}
		for _, inv := range invs {
			if inv.Agent != "claude" || !strings.Contains(strings.Join(inv.Args, " "), "--setting-sources user") {
				t.Fatalf("rerun lost configured backend or suppression: %+v", inv)
			}
		}
		t.Logf("rerun %s used Claude with --setting-sources user", run)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("launch or rerun rewrote operator configuration: %v", err)
	}
}

func TestCallerSelectedAgentOutageHasNoFallbackAndRetryKeepsSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("provider outage fixture uses a POSIX shell")
	}
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	t.Setenv("OPENAI_API_KEY", "caller-fixture-key")
	flag := filepath.Join(h.BinDir, "provider-down")
	if err := os.WriteFile(flag, nil, 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(h.BinDir, "codex-outage")
	credentialLog := filepath.Join(h.BinDir, "credential-checks")
	script := "#!/bin/sh\n[ \"$OPENAI_API_KEY\" = caller-fixture-key ] || exit 22\nprintf 'credential preserved\\n' >> " + shellQuote(credentialLog) + "\nif [ -e " + shellQuote(flag) + " ]; then echo 'You have hit your usage limit' >&2; exit 1; fi\nexec " + shellQuote(filepath.Join(h.BinDir, "codex")) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	quotaProbe := filepath.Join(h.BinDir, "quota-probe")
	quotaLog := filepath.Join(h.BinDir, "quota-calls")
	if err := os.WriteFile(quotaProbe, []byte("#!/bin/sh\nprintf called > "+shellQuote(quotaLog)+"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(h.NMHome, "config.yaml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(before), "agent: claude", "agent: [claude, codex]", 1)
	cfg = strings.Replace(cfg, "agent_path_override:\n", "agent_path_override:\n  codex: "+wrapper+"\n", 1)
	cfg += "quota_routing:\n  enabled: true\n  quota_axi_path: " + quotaProbe + "\n"
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/caller-retry"
	h.CommitChange(branch, "feature.txt", "retry fixture\n", "probe caller engine outage")
	out, err := h.Run("axi", "run", "--agent", "codex", "--intent", "keep caller backend despite provider outage")
	t.Logf("selected Codex provider outage:\n%s", out)
	failed := h.WaitForRun(branch, 60*time.Second)
	if failed.Status != types.RunFailed || failed.RequestedAgent != types.AgentCodex {
		t.Fatalf("selected engine silently fell back: err=%v run=%+v", err, failed)
	}
	if invs := h.AgentInvocations(); len(invs) != 0 {
		t.Fatalf("outage launched another backend: %+v", invs)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	out, err = h.Run("axi", "retry", "--run", failed.ID)
	if err != nil {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	t.Logf("retry after provider recovery:\n%s", out)
	retried := h.WaitForRun(branch, 90*time.Second)
	if retried.ID == failed.ID || retried.Status != types.RunCompleted || retried.RequestedAgent != types.AgentCodex {
		t.Fatalf("retry lost backend selection: %+v", retried)
	}
	invs := invocationsForRun(t, h, retried.ID)
	if len(invs) == 0 {
		t.Fatal("retry launched no subprocesses")
	}
	for _, inv := range invs {
		if inv.Agent != "codex" {
			t.Fatalf("retry switched engine: %+v", inv)
		}
		t.Logf("captured retry subprocess: agent=%s", inv.Agent)
	}
	if _, err := os.Stat(quotaLog); !os.IsNotExist(err) {
		t.Fatalf("explicit selection consulted quota routing: %v", err)
	}
	credentials, err := os.ReadFile(credentialLog)
	if err != nil || strings.Count(string(credentials), "credential preserved") != len(invs)+1 {
		t.Fatalf("operator credential was not preserved on every invocation: %v\n%s", err, credentials)
	}
	t.Logf("configured executable received the fixture credential for failure and retry; quota probe was never invoked")
}

func TestCallerSelectedPiAcceptsItsModelProfile(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeReviewAgentsRoutingScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/caller-pi"
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "probe explicit Pi selection")
	out, err := h.Run("axi", "run", "--agent", "pi", "--model", "openai/pinned-pi", "--effort", "high", "--intent", "Pi-only profile on explicit Pi backend")
	if err != nil {
		t.Fatalf("explicit Pi launch: %v\n%s", err, out)
	}
	t.Logf("explicit Pi CLI launch:\n%s", out)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if parked.RequestedAgent != types.AgentPi || parked.PiProfile == nil || parked.PiProfile.Model != "openai/pinned-pi" || parked.PiProfile.Effort != "high" {
		t.Fatalf("explicit Pi selection or model profile lost: %+v", parked)
	}
	invs := invocationsForRun(t, h, parked.ID)
	if len(invs) == 0 {
		t.Fatal("explicit Pi launch recorded no subprocesses")
	}
	for _, inv := range invs {
		args := strings.Join(inv.Args, " ")
		if inv.Agent != "pi" || !strings.Contains(args, "--model openai/pinned-pi") || !strings.Contains(args, "--thinking high") {
			t.Fatalf("explicit Pi subprocess lost model or effort: %+v", inv)
		}
		t.Logf("captured subprocess: agent=pi model=openai/pinned-pi effort=high")
	}
}
