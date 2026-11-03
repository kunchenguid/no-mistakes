package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func requireQuotaShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell fixtures")
	}
}

// writeQuotaAXI installs a fake quota-axi executable that emits the given
// stdout. It returns the path to put in quota_routing.quota_axi_path.
func writeQuotaAXI(t *testing.T, stdout string) string {
	t.Helper()
	requireQuotaShell(t)
	path := filepath.Join(t.TempDir(), "quota-axi")
	script := "#!/bin/sh\ncat <<'NMEOF'\n" + stdout + "\nNMEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake quota-axi: %v", err)
	}
	return path
}

// writeQuotaAXIFail installs a fake quota-axi that exits non-zero with the
// given stderr, the stand-in for a missing or logged-out CLI.
func writeQuotaAXIFail(t *testing.T) string {
	t.Helper()
	requireQuotaShell(t)
	path := filepath.Join(t.TempDir(), "quota-axi")
	script := "#!/bin/sh\necho 'boom' >&2\nexit 3\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write failing quota-axi: %v", err)
	}
	return path
}

// claudeThenCodexSnapshot is a schema-5 snapshot where codex has more
// spendPriority headroom than claude, so the selector must put codex first.
const claudeThenCodexSnapshot = `{"schemaVersion":5,"providers":[
 {"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":[
   {"scope":"all_models","status":"known","effectivePercentRemaining":40,
    "runway":{"status":"through_reset"},
    "selection":{"spendPriority":40.0}}]}},
 {"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[
   {"scope":"all_models","status":"known","effectivePercentRemaining":80,
    "runway":{"status":"through_reset"},
    "selection":{"spendPriority":80.0}}]}}
]}`

func quotaRoutingConfig(path string) *config.Config {
	cfg := &config.Config{Agent: types.AgentClaude}
	cfg.Agents = []types.AgentName{types.AgentClaude, types.AgentCodex}
	cfg.QuotaRouting.Enabled = true
	cfg.QuotaRouting.QuotaAXIPath = path
	return cfg
}

func TestQuotaRoutingRecordForRun_DisabledNeverRunsQuotaAXI(t *testing.T) {
	// A quota-axi that would fail loudly if executed: with the feature off,
	// the run-setup read must not happen at all.
	cfg := &config.Config{Agent: types.AgentClaude}
	cfg.Agents = []types.AgentName{types.AgentClaude}
	cfg.QuotaRouting.Enabled = false
	cfg.QuotaRouting.QuotaAXIPath = writeQuotaAXIFail(t)

	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record != nil {
		t.Fatalf("feature off must produce no record, got %+v", record)
	}
}

func TestQuotaRoutingRecordForRun_SelectsTheHigherQuotaHead(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil || !record.Enabled {
		t.Fatalf("enabled feature must produce an enabled record, got %+v", record)
	}
	if record.Choice != "codex" {
		t.Fatalf("codex has the higher spendPriority and must be chosen, got %s", record.Choice)
	}
	if got := record.OrderedAgents(cfg.Agents); got[0] != types.AgentCodex || got[1] != types.AgentClaude {
		t.Fatalf("ordered agents must be [codex claude], got %v", got)
	}
	if len(record.Candidates) != 2 {
		t.Fatalf("both candidates must carry evidence, got %d", len(record.Candidates))
	}
	for _, c := range record.Candidates {
		if !c.Eligible || !c.Rankable {
			t.Fatalf("healthy claude and codex must be eligible and rankable, got %+v", c)
		}
	}
}

func TestQuotaRoutingRecordForRun_BuiltInProviderMappingApplies(t *testing.T) {
	// claude and codex map without a declared quota_provider; the evidence
	// rows must name the resolved provider, not an empty one.
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil {
		t.Fatal("expected a record")
	}
	seen := map[types.AgentName]string{}
	for _, c := range record.Candidates {
		seen[c.Agent] = c.Provider
	}
	if seen[types.AgentClaude] != "claude" || seen[types.AgentCodex] != "codex" {
		t.Fatalf("built-in providers must resolve from the agent name, got %v", seen)
	}
}

func TestQuotaRoutingRecordForRun_MissingQuotaAXIKeepsConfiguredOrder(t *testing.T) {
	cfg := quotaRoutingConfig(filepath.Join(t.TempDir(), "no-such-quota-axi"))
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil || !record.Enabled {
		t.Fatalf("a failed read still records the enabled decision, got %+v", record)
	}
	if record.Choice != "claude" {
		t.Fatalf("configured head must survive a failed read, got %s", record.Choice)
	}
	if record.Reason == "" {
		t.Fatal("the failure record must carry the reason quota-axi could not be read")
	}
	if len(record.Candidates) != 0 {
		t.Fatalf("no snapshot means no per-candidate evidence, got %d", len(record.Candidates))
	}
}

func TestQuotaRoutingRecordForRun_FailingQuotaAXIKeepsConfiguredOrder(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXIFail(t))
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil || !record.Enabled || record.Choice != "claude" {
		t.Fatalf("a failing quota-axi keeps the configured order, got %+v", record)
	}
	if record.Reason == "" {
		t.Fatal("the failure record must carry the reason")
	}
}

func TestQuotaRoutingRecordForRun_PiWithoutQuotaProviderDisablesReordering(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	// The published intent: a Pi agent with no declared quota_provider and no
	// built-in mapping disables reordering for the whole run with one
	// warning, rather than guessing from the model name. The failure record
	// keeps the configured order, never arms the switch, and records why.
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	cfg.Agents = []types.AgentName{types.AgentPi, types.AgentClaude, types.AgentCodex}
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil || record.Enabled || record.Choice != "pi" {
		t.Fatalf("pi without quota_provider keeps the configured order, got %+v", record)
	}
	if record.Enabled {
		t.Fatal("a pi-disabled run is not a live selection and must not arm the switch")
	}
	if strings.Count(logs.String(), "level=WARN") != 1 || !strings.Contains(logs.String(), "reordering disabled") {
		t.Fatalf("expected one accurate warning, got %q", logs.String())
	}
	if !strings.Contains(record.Reason, "pi has no quota_provider") {
		t.Fatalf("the record must say why reordering was disabled, got %q", record.Reason)
	}
}

func TestQuotaRoutingRecordForRun_PiWithQuotaProviderStillRanks(t *testing.T) {
	// The declared quota_provider makes the Pi candidate a normal candidate:
	// it ranks, and the run reorders.
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	cfg.Agents = []types.AgentName{types.AgentPi, types.AgentClaude}
	cfg.QuotaProviders = map[string]string{"pi": "codex"}
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil || !record.Enabled {
		t.Fatalf("a mapped pi is a normal candidate, got %+v", record)
	}
}

func TestQuotaRoutingRecordForRun_UnmappedAgentKeepsItsPosition(t *testing.T) {
	// opencode has no built-in provider mapping and the config declares no
	// quota_provider for it: it stays eligible in its configured position and
	// every other candidate still ranks against the snapshot.
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	cfg.Agents = []types.AgentName{types.AgentOpenCode, types.AgentCodex}
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record == nil || record.Choice != "codex" {
		t.Fatalf("codex must still rank first, got %+v", record)
	}
	order := record.OrderedAgents(cfg.Agents)
	if order[0] != types.AgentCodex || order[1] != types.AgentOpenCode {
		t.Fatalf("unmapped opencode keeps its configured position, got %v", order)
	}
}

func TestQuotaRoutingRecordForRun_TimeoutKeepsConfiguredOrder(t *testing.T) {
	requireQuotaShell(t)
	path := filepath.Join(t.TempDir(), "quota-axi")
	script := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write slow quota-axi: %v", err)
	}
	cfg := quotaRoutingConfig(path)
	cfg.QuotaRouting.ReadTimeout = 50 * time.Millisecond
	started := time.Now()
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("read must respect the configured timeout, took %s", time.Since(started))
	}
	if record == nil || !record.Enabled || record.Choice != "claude" || record.Reason == "" {
		t.Fatalf("a slow quota-axi keeps the configured order with a reason, got %+v", record)
	}
}

func TestApplyQuotaRoutingRecord(t *testing.T) {
	resolved := []types.AgentName{types.AgentClaude, types.AgentCodex}
	record := quotarouting.NewRecord(true,
		[]types.AgentName{types.AgentClaude, types.AgentCodex},
		[]types.AgentName{types.AgentCodex, types.AgentClaude}, quotarouting.Selection{Choice: types.AgentCodex, Order: []types.AgentName{types.AgentCodex, types.AgentClaude}})

	ordered, switchActive := applyQuotaRoutingRecord(record, resolved)
	if !switchActive {
		t.Fatal("an enabled record activates the mid-run quota switch")
	}
	if ordered[0] != types.AgentCodex || ordered[1] != types.AgentClaude {
		t.Fatalf("record order must win, got %v", ordered)
	}

	if got, active := applyQuotaRoutingRecord(nil, resolved); active || got[0] != types.AgentClaude {
		t.Fatalf("no record must leave the resolved list and the switch alone, got %v active=%v", got, active)
	}

	failure := quotarouting.NewFailureRecord(true, resolved, "quota-axi missing")
	got, active := applyQuotaRoutingRecord(failure, resolved)
	if !active {
		t.Fatal("a failed read must preserve quota switching through configured order")
	}
	if got[0] != types.AgentClaude || got[1] != types.AgentCodex {
		t.Fatalf("a failed read keeps the configured order, got %v", got)
	}
}

// TestNewConfiguredAgent_QuotaRoutingReordersTheChain is the run-setup seam
// test: with the feature on, the built fallback chain starts on the
// quota-selected agent, and the returned record is the enabled choice.
func TestNewConfiguredAgent_QuotaRoutingReordersTheChain(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	ag, record, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, nil)
	if err != nil {
		t.Fatalf("newConfiguredAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != string(types.AgentCodex) {
		t.Fatalf("fallback chain head must be the quota choice codex, got %s", ag.Name())
	}
	if record == nil || !record.Enabled || record.Choice != "codex" {
		t.Fatalf("the setup record must name the enabled choice, got %+v", record)
	}
}

func TestNewConfiguredAgent_QuotaRoutingOffNeverRunsQuotaAXI(t *testing.T) {
	cfg := &config.Config{Agent: types.AgentClaude}
	cfg.Agents = []types.AgentName{types.AgentClaude, types.AgentCodex}
	cfg.QuotaRouting.QuotaAXIPath = writeQuotaAXIFail(t)

	ag, record, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, nil)
	if err != nil {
		t.Fatalf("newConfiguredAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != string(types.AgentClaude) {
		t.Fatalf("feature off keeps the configured head, got %s", ag.Name())
	}
	if record != nil {
		t.Fatalf("feature off produces no record, got %+v", record)
	}
}

func TestNewConfiguredAgent_QuotaRoutingBrokenQuotaAXIKeepsConfiguredHead(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXIFail(t))
	ag, record, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, nil)
	if err != nil {
		t.Fatalf("newConfiguredAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != string(types.AgentClaude) {
		t.Fatalf("a failing quota-axi keeps the configured head, got %s", ag.Name())
	}
	if record == nil || !record.Enabled || record.Choice != "claude" || record.Reason == "" {
		t.Fatalf("the failure must be recorded, got %+v", record)
	}
}

// TestNewConfiguredAgent_StoredRecordRecoveryReorders proves the recovery
// path: a supplied record reorders the chain WITHOUT executing quota-axi, so
// a resumed run keeps its original choice rather than re-selecting against a
// newer snapshot.
func TestNewConfiguredAgent_StoredRecordRecoveryReorders(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXIFail(t)) // must never be executed
	record := quotarouting.NewRecord(true,
		[]types.AgentName{types.AgentClaude, types.AgentCodex},
		[]types.AgentName{types.AgentCodex, types.AgentClaude}, quotarouting.Selection{Choice: types.AgentCodex, Order: []types.AgentName{types.AgentCodex, types.AgentClaude}})

	ag, got, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, record)
	if err != nil {
		t.Fatalf("newConfiguredAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != string(types.AgentCodex) {
		t.Fatalf("a recovered run keeps its recorded head codex, got %s", ag.Name())
	}
	if got != record {
		t.Fatal("the supplied record must pass through unchanged")
	}
}

func TestNewConfiguredAgent_StoredFailureRecordKeepsConfiguredHead(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXIFail(t))
	record := quotarouting.NewFailureRecord(true,
		[]types.AgentName{types.AgentClaude, types.AgentCodex}, "quota-axi missing")

	ag, _, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, record)
	if err != nil {
		t.Fatalf("newConfiguredAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != string(types.AgentClaude) {
		t.Fatalf("a recovered failure record keeps the configured head, got %s", ag.Name())
	}
}

func TestStoredQuotaRoutingRecord_FailsClosedOnGarbage(t *testing.T) {
	if _, err := storedQuotaRoutingRecord(""); err != nil {
		t.Fatalf("an empty payload is a pre-feature run and parses to nil, got %v", err)
	}
	if _, err := storedQuotaRoutingRecord("{not json"); err == nil {
		t.Fatal("a corrupt stored record must fail recovery closed")
	} else if !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("the error must say why: %v", err)
	}
	record, err := storedQuotaRoutingRecord(`{"version":1,"enabled":true,"configured_agents":["claude"],"order":["claude"],"choice":"claude","reason":"ok","candidates":[],"switches":[]}`)
	if err != nil || record == nil {
		t.Fatalf("a valid stored record parses, got %v %+v", err, record)
	}
}

func TestQuotaRoutingUnmappedCandidatesParticipateAsUnknowns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agents   []types.AgentName
		snapshot string
		want     []string
	}{
		{"unknown beats exhausted", []types.AgentName{types.AgentCodex, types.AgentOpenCode},
			`{"schemaVersion":5,"providers":[{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[{"scope":"all_models","status":"known","effectivePercentRemaining":0}]}}]}`,
			[]string{"opencode", "codex"}},
		{"mapped unknown first", []types.AgentName{types.AgentClaude, types.AgentOpenCode}, `{"schemaVersion":5,"providers":[]}`, []string{"claude", "opencode"}},
		{"unmapped unknown first", []types.AgentName{types.AgentOpenCode, types.AgentClaude}, `{"schemaVersion":5,"providers":[]}`, []string{"opencode", "claude"}},
		{"only unmapped", []types.AgentName{types.AgentOpenCode, types.AgentCopilot}, `{"schemaVersion":5,"providers":[]}`, []string{"opencode", "copilot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := quotaRoutingConfig(writeQuotaAXI(t, tc.snapshot))
			cfg.Agents = tc.agents
			record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
			if record == nil || record.Choice != tc.want[0] || !reflect.DeepEqual(record.Order, tc.want) || len(record.Candidates) != len(tc.agents) {
				t.Fatalf("record = %+v, want order %v and evidence for every candidate", record, tc.want)
			}
		})
	}
}

func TestQuotaRoutingUsesTheEffectiveRawModelForScopesAndAccount(t *testing.T) {
	for _, raw := range [][]string{{"--model", "opus"}, {"--model=opus"}} {
		for _, profileModel := range []string{"sonnet", ""} {
			cfg := quotaRoutingConfig(writeQuotaAXI(t, `{"schemaVersion":5,"providers":[
    {"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":[
     {"scope":"all_models","status":"known","effectivePercentRemaining":90,"runway":{"status":"through_reset"},"selection":{"spendPriority":90}},
     {"scope":"model:opus","status":"known","effectivePercentRemaining":0}]}},
    {"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[
     {"scope":"all_models","status":"known","effectivePercentRemaining":80,"runway":{"status":"through_reset"},"selection":{"spendPriority":80}}]}}
   ]}`))
			cfg.AgentConfig = map[string]agentcfg.Profile{"claude": {Model: profileModel}}
			cfg.AgentArgsOverride = map[string][]string{"claude": raw}
			record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
			if record.Choice != "codex" || record.Candidates[0].Eligible || record.Candidates[0].Scope != "model:opus" {
				t.Fatalf("profile=%q raw=%v record=%+v", profileModel, raw, record)
			}
		}
	}
	cfg := quotaRoutingConfig(writeQuotaAXI(t, `{"schemaVersion":6,"providers":[
  {"provider":"codex","accountKey":"codex-home","quotaSemantics":{"status":"known","effectiveAvailability":[
   {"scope":"all_models","status":"known","effectivePercentRemaining":90,"runway":{"status":"through_reset"},"selection":{"spendPriority":90}}]}},
  {"provider":"codex","accountKey":"work","quotaSemantics":{"status":"known","effectiveAvailability":[
   {"scope":"model:opus","status":"known","effectivePercentRemaining":0}]}}
 ]}`))
	cfg.Agents = []types.AgentName{types.AgentPi, types.AgentCodex}
	cfg.AgentConfig = map[string]agentcfg.Profile{"pi": {Model: "codex-native/sonnet"}}
	cfg.AgentArgsOverride = map[string][]string{"pi": {"--model", "work/opus"}}
	cfg.QuotaProviders = map[string]string{"pi": "codex"}
	record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
	if record.Choice != "codex" || record.Candidates[0].Account != "work" || !record.Candidates[0].Eligible || record.Candidates[0].Rankable || record.Candidates[0].Scope != "" {
		t.Fatalf("raw model must bind work account while model-only Pi quota stays unknown: %+v", record)
	}
}

func TestPipelineQuotaSnapshotIsReadOnceAndNeverByReviewRoles(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXI(t, claudeThenCodexSnapshot))
	counter := filepath.Join(t.TempDir(), "reads")
	t.Setenv("NM_QUOTA_READS", counter)
	script, err := os.ReadFile(cfg.QuotaRouting.QuotaAXIPath)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("#!/bin/sh\n"), []byte("#!/bin/sh\nprintf x >> \"$NM_QUOTA_READS\"\n"), 1)
	if err := os.WriteFile(cfg.QuotaRouting.QuotaAXIPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.ReviewAgents = map[string]config.ReviewAgent{}
	for _, role := range config.ReviewAgentRoles {
		cfg.ReviewAgents[role] = config.ReviewAgent{Agent: types.AgentCodex}
	}
	primary, record, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	recovered, _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, record)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	reads, err := os.ReadFile(counter)
	if err != nil || string(reads) != "x" {
		t.Fatalf("snapshot reads = %q, error = %v; want exactly one", reads, err)
	}
}

func TestQuotaRoutingRecoveryRestoresTheLastSwitch(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXIFail(t))
	record := quotarouting.NewFailureRecord(true, cfg.Agents, "quota-axi missing")
	record = record.AppendSwitch(types.AgentClaude, types.AgentCodex, "quota exceeded", time.Now())
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := storedQuotaRoutingRecord(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	ag, _, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if ag.Name() != "codex" {
		t.Fatalf("active agent = %s, want codex", ag.Name())
	}
}

func TestMissingQuotaSnapshotStillSwitchesOnANativeQuotaError(t *testing.T) {
	requireQuotaShell(t)
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	pi := filepath.Join(dir, "pi")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\necho 'quota exceeded' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pi, []byte(`#!/bin/sh
cat >/dev/null
printf '%s' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := quotaRoutingConfig(filepath.Join(dir, "missing-quota-axi"))
	cfg.Agents = []types.AgentName{types.AgentClaude, types.AgentPi}
	cfg.QuotaProviders = map[string]string{"pi": "codex"}
	cfg.AgentPathOverride = map[string]string{"claude": claude, "pi": pi}
	ag, record, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	switches := 0
	for i := 0; i < 2; i++ {
		result, err := ag.Run(context.Background(), agent.RunOpts{CWD: dir, Prompt: "hello", OnLifecycle: func(event agent.LifecycleEvent) {
			if event.Phase == agent.LifecyclePhaseQuotaSwitch {
				switches++
			}
		}})
		if err != nil || result == nil || result.Text != "ok" || result.Provider != "pi" {
			t.Fatalf("result = %+v, error = %v", result, err)
		}
	}
	if switches != 1 || record.Choice != "claude" || record.Reason == "" {
		t.Fatalf("switches = %d, initial record = %+v", switches, record)
	}
}

func TestQuotaRoutingRecoveryRefusesAnUnavailableActiveAgent(t *testing.T) {
	cfg := quotaRoutingConfig(writeQuotaAXIFail(t))
	cfg.Agents = []types.AgentName{types.AgentClaude}
	record := quotarouting.NewFailureRecord(true, []types.AgentName{types.AgentClaude, types.AgentCodex}, "quota-axi missing")
	record = record.AppendSwitch(types.AgentClaude, types.AgentCodex, "quota exceeded", time.Now())
	ag, _, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, record)
	if ag != nil || err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("recovery must not return to exhausted claude: agent=%v error=%v", ag, err)
	}
}

func TestQuotaRoutingRecoveryWithoutARecordNeverReadsANewSnapshot(t *testing.T) {
	manager, run, _ := gatePinFixture(t, "", nil)
	marker := filepath.Join(t.TempDir(), "snapshot-read")
	t.Setenv("NM_QUOTA_RECOVERY_MARKER", marker)
	executable := writeQuotaAXI(t, claudeThenCodexSnapshot)
	script, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("#!/bin/sh\n"), []byte("#!/bin/sh\nprintf called > \"$NM_QUOTA_RECOVERY_MARKER\"\n"), 1)
	if err := os.WriteFile(executable, script, 0o755); err != nil {
		t.Fatal(err)
	}
	global, err := os.ReadFile(manager.paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	global = append(global, []byte("quota_routing:\n  enabled: true\n  quota_axi_path: "+executable+"\n")...)
	if err := os.WriteFile(manager.paths.ConfigFile(), global, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		plan, err := manager.prepareRecoveredRun(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if plan.cfg.QuotaRouting.Enabled || plan.agent.Name() != "claude" {
			t.Fatalf("old run changed its routing: enabled=%v agent=%s", plan.cfg.QuotaRouting.Enabled, plan.agent.Name())
		}
		if err := plan.agent.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("recovery executed quota-axi: %v", err)
	}
	stored, err := manager.db.GetRun(run.ID)
	if err != nil || stored.QuotaRouting != "" {
		t.Fatalf("original absent record changed: %+v error=%v", stored, err)
	}
}

func TestQuotaRoutingRecoversCanonicalACPIdentities(t *testing.T) {
	for _, tc := range []struct{ stored, resolved, event, adapter string }{
		{"cursor", "cursor", "acp:cursor", "acp:cursor"},
		{"cursor", "acp:cursor", "cursor", "acp:cursor"},
		{"acp:cursor", "cursor", "cursor", "acp:cursor"},
		{"devin", "devin", "acp:devin", "acp:devin"},
		{"devin", "acp:devin", "devin", "acp:devin"},
		{"acp:devin", "devin", "devin", "acp:devin"},
	} {
		for _, switched := range []bool{false, true} {
			cfg := quotaRoutingConfig(filepath.Join(t.TempDir(), "unavailable-quota-axi"))
			cfg.Agents = []types.AgentName{types.AgentClaude, types.AgentName(tc.resolved)}
			order := []types.AgentName{types.AgentName(tc.stored), types.AgentClaude}
			if switched {
				order[0], order[1] = order[1], order[0]
			}
			record := quotarouting.NewRecord(true, []types.AgentName{types.AgentClaude, types.AgentName(tc.stored)}, order, quotarouting.Selection{Choice: order[0]})
			if switched {
				record = record.AppendSwitch(types.AgentClaude, types.AgentName(tc.event), "quota exceeded", time.Now())
			}
			payload, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := storedQuotaRoutingRecord(string(payload))
			if err != nil {
				t.Fatal(err)
			}
			ag, _, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}, restored)
			if err != nil {
				t.Fatalf("stored=%s resolved=%s event=%s switched=%v: %v", tc.stored, tc.resolved, tc.event, switched, err)
			}
			if ag.Name() != tc.adapter {
				t.Errorf("recovered=%s want=%s", ag.Name(), tc.adapter)
			}
			if err := ag.Close(); err != nil {
				t.Fatal(err)
			}
			ordered := restored.OrderedAgents(cfg.Agents)
			index := 0
			if switched {
				index = 1
			}
			if ordered[index] != types.AgentName(tc.resolved) {
				t.Fatalf("configured spelling changed: %v", ordered)
			}
		}
	}
}

func TestQuotaRoutingUsesProvidersFromTheOtherACPSpelling(t *testing.T) {
	for _, tc := range []struct{ selected, configured string }{
		{"cursor", "acp:cursor"}, {"acp:cursor", "cursor"},
		{"devin", "acp:devin"}, {"acp:devin", "devin"},
	} {
		snapshot := writeQuotaAXI(t, `{"schemaVersion":5,"providers":[
   {"provider":"cursor","quotaSemantics":{"status":"known","effectiveAvailability":[{"scope":"all_models","status":"known","effectivePercentRemaining":0}]}},
   {"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":[{"scope":"all_models","status":"known","effectivePercentRemaining":80,"runway":{"status":"through_reset"},"selection":{"spendPriority":2}}]}}
  ]}`)
		global, err := config.LoadGlobalFromBytes([]byte("agent: [" + tc.selected + ", claude]\nquota_routing:\n  enabled: true\nagent_config:\n  " + tc.configured + ": {quota_provider: cursor}\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Merge(global, &config.RepoConfig{})
		cfg.QuotaRouting.QuotaAXIPath = snapshot
		record := quotaRoutingRecordForRun(context.Background(), cfg, cfg.Agents)
		if record == nil || record.Choice != "claude" || len(record.Candidates) != 2 || record.Candidates[0].Eligible || record.Candidates[0].Agent != types.AgentName(tc.selected) || record.Candidates[0].Provider != "cursor" {
			t.Fatalf("selected=%s configured=%s record=%+v", tc.selected, tc.configured, record)
		}
	}
}
