package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A synthetic public acpx package keeps catalog discovery non-generating. Its
// model IDs are opaque fixtures, never a real account's advertised catalog.
func writeCursorCatalogPackage(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic direct Node shebang fixture requires POSIX")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node required for public acpx SDK fixture")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "model-work")
	files := map[string]string{
		"package.json": `{"name":"acpx","version":"0.20.0","type":"module","bin":{"acpx":"cli.js"},"exports":{"./runtime":"./runtime.js","./agent-registry":"./registry.js"}}`,
		"cli.js":       "#!/usr/bin/env node\nimport {appendFileSync} from 'node:fs'; appendFileSync(" + strconvQuote(counter) + ", 'model work\\n');\n",
		"runtime.js":   `import {appendFileSync} from "node:fs"; export async function inspectAgentModels() { appendFileSync(` + strconvQuote(filepath.Join(dir, "catalog-probes")) + `,"probe\n"); return {availableModelIds: ["fixture-supported[mode=careful]"], currentModelId: "fixture-supported[mode=careful]"}; }`,
		"registry.js":  `export function createAgentRegistry() { return {inspect: async () => ({launch: {kind: "installed", argv: ['fixture-cursor', 'acp']}, missingRequirements: []})}; }`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "cli.js"), counter
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }

func TestCursorUnsupportedSelectionPreservesActiveRun(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunPending, types.RunRunning} {
		t.Run(string(status), func(t *testing.T) { cursorUnsupportedSelectionPreservesRun(t, status) })
	}
}
func cursorUnsupportedSelectionPreservesRun(t *testing.T, status types.RunStatus) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "cursor-preflight")
	bin, counter := writeCursorCatalogPackage(t)
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: cursor\nacpx_path: "+bin+"\nacp_registry_overrides:\n  cursor: "+bin+"\nagent_config:\n  cursor: {model: 'fixture-absent[mode=careful]'}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	active, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(active.ID, status); err != nil {
		t.Fatal(err)
	}
	oldWorktree := filepath.Join(t.TempDir(), "old-work")
	if err := os.MkdirAll(oldWorktree, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(oldWorktree, "preserve")
	if err := os.WriteFile(marker, []byte("existing work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(active.ID, oldWorktree); err != nil {
		t.Fatal(err)
	}
	dependent := &mockPassStep{name: types.StepTest}
	m := NewRunManager(d, p, func() []pipeline.Step { return []pipeline.Step{dependent} })
	defer m.Shutdown()
	cancelled := false
	m.cancels[active.ID] = func(error) { cancelled = true }
	_, startErr := m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "catalog check", "", false, "", nil)
	// Check preservation first: this is the causal admission regression, even
	// when a later adapter or pipeline stage would independently fail.
	if cancelled {
		t.Fatal("unsupported Cursor model cancelled the previous run before admission")
	}
	got, err := d.GetRun(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != status {
		t.Fatalf("previous run changed status: %s", got.Status)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "existing work" {
		t.Fatalf("previous work changed: %q %v", body, err)
	}
	if startErr == nil || !strings.Contains(startErr.Error(), "unsupported") || !strings.Contains(startErr.Error(), "fixture-absent[mode=careful]") {
		t.Fatalf("expected exact unsupported selection diagnostic, got %v", startErr)
	}
	if dependent.execCnt.Load() != 0 {
		t.Fatal("dependent pipeline work ran")
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatalf("dependent model work ran: %v", err)
	}
}

func TestCursorAdmissionRolesAliasesAndDeduplication(t *testing.T) {
	bin, _ := writeCursorCatalogPackage(t)
	dir := filepath.Dir(bin)
	supported := "fixture-supported[mode=careful]"
	cfg := &config.Config{Agent: types.AgentCursor, ACPXPath: bin, ACPRegistryOverrides: map[string]string{"cursor": bin}, AgentConfig: map[string]agentcfg.Profile{"cursor": {Model: supported}, "acp:cursor": {Model: "unused-absent"}}, ReviewAgents: map[string]config.ReviewAgent{
		config.RoleReviewer:           {Agent: "acp:cursor", Model: supported, Fallback: []config.ReviewAgent{{Agent: types.AgentCursor, Model: supported}}},
		config.RoleFixer:              {Agent: types.AgentCursor},
		config.RoleReviewerAfterRound: {Agent: types.AgentCursor, Model: supported},
		config.RoleFixerAfterRound:    {Agent: types.AgentCursor, Model: supported},
	}}
	for i := 0; i < 2; i++ {
		if err := preflightCursorSelections(context.Background(), cfg, dir, runenv.Overlay{}); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, "catalog-probes"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(body), "probe\n"); got != i+1 {
			t.Fatalf("admission dedup/staleness: got %d probes after %d admissions", got, i+1)
		}
	}
	for _, role := range config.ReviewAgentRoles {
		t.Run(role, func(t *testing.T) {
			clone := *cfg
			clone.ReviewAgents = map[string]config.ReviewAgent{role: {Agent: "acp:cursor"}}
			err := preflightCursorSelections(context.Background(), &clone, dir, runenv.Overlay{})
			var refusal *agent.CursorModelError
			if !errors.As(err, &refusal) || refusal.Status != agent.CursorModelUnsupported || refusal.Owner != "review_agents."+role+".model" {
				t.Fatalf("selected alias/role precedence: %v", err)
			}
		})
	}
	clone := *cfg
	clone.ReviewAgents = map[string]config.ReviewAgent{config.RoleReviewer: {Agent: types.AgentCursor, Fallback: []config.ReviewAgent{{Agent: "acp:cursor"}}}}
	err := preflightCursorSelections(context.Background(), &clone, dir, runenv.Overlay{})
	var refusal *agent.CursorModelError
	if !errors.As(err, &refusal) || refusal.Owner != "review_agents.reviewer.fallback[0].model" {
		t.Fatalf("fallback selection not checked: %v", err)
	}
}

func TestCursorRecoveryChecksFreshSelectionAndPreservesWork(t *testing.T) {
	m, run, _ := gatePinFixture(t, "", nil)
	bin, _ := writeCursorCatalogPackage(t)
	write := func(model string) {
		t.Helper()
		if err := os.WriteFile(m.paths.ConfigFile(), []byte("agent: cursor\nacpx_path: "+bin+"\nacp_registry_overrides:\n  cursor: "+bin+"\nagent_config:\n  cursor: {model: '"+model+"'}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("fixture-supported[mode=careful]")
	plan, err := m.prepareRecoveredRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.agent.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(plan.workDir, "recovery-work")
	if err := os.WriteFile(marker, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated, err := m.db.InsertRun(run.RepoID, "unrelated", run.HeadSHA, run.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := false
	m.cancels[unrelated.ID] = func(error) { cancelled = true }
	write("changed-absent[mode=careful]")
	_, err = m.prepareRecoveredRun(context.Background(), run)
	var refusal *agent.CursorModelError
	if !errors.As(err, &refusal) || refusal.Status != agent.CursorModelUnsupported {
		t.Fatalf("recovery reused stale success: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "catalog-probes"))
	if err != nil || strings.Count(string(body), "probe\n") != 2 {
		t.Fatalf("fresh recovery probes missing: %q %v", body, err)
	}
	if cancelled {
		t.Fatal("recovery cancelled unrelated work")
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "retain" {
		t.Fatalf("recovery work changed: %q %v", body, err)
	}
	stored, err := m.db.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("parked recovery run changed: %+v %v", stored, err)
	}
}

func TestCursorRefusalPrecedesPreparation(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, _ := setupTestGitRepo(t, p, d, "cursor-before-prepare")
	bin, modelCounter := writeCursorCatalogPackage(t)
	prepCounter := filepath.Join(t.TempDir(), "prepare-called")
	command := "printf prepared > " + strconvQuote(prepCounter)
	head := commitDefaultBranchConfig(t, repo.WorkingPath, "test:\n  prepare: true\ncommands:\n  prepare: "+strconvQuote(command)+"\n")
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: cursor\nacpx_path: "+bin+"\nacp_registry_overrides:\n  cursor: "+bin+"\nagent_config:\n  cursor: {model: 'absent'}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, func() []pipeline.Step { return []pipeline.Step{&steps.TestStep{}} })
	defer m.Shutdown()
	_, err = m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "catalog before prepare", "", false, "", nil)
	var refusal *agent.CursorModelError
	if !errors.As(err, &refusal) || refusal.Status != agent.CursorModelUnsupported {
		t.Fatalf("did not reach catalog refusal: %v", err)
	}
	for _, counter := range []string{prepCounter, modelCounter} {
		if _, err := os.Stat(counter); !os.IsNotExist(err) {
			t.Fatalf("dependent work ran: %v", err)
		}
	}
}

func TestCursorAdmissionIgnoresNonCursorAndUnusedProfiles(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentPi, types.AgentClaude, types.AgentRovoDev, "acp:other"} {
		cfg := &config.Config{Agent: name, AgentPathOverride: map[string]string{string(name): "missing-unused-binary"}, AgentConfig: map[string]agentcfg.Profile{string(name): {Model: "immutable-model"}, "cursor": {Model: "unused-unsupported"}}}
		if err := preflightCursorSelections(context.Background(), cfg, t.TempDir(), runenv.Overlay{}); err != nil {
			t.Fatalf("non-Cursor admission gained a probe: %s %v", name, err)
		}
		if cfg.Agent != name || cfg.AgentProfileFor(name).Model != "immutable-model" || len(cfg.Agents) != 0 {
			t.Fatalf("non-Cursor selection changed: %+v", cfg)
		}
	}
}

func TestCursorAdmissionOptOutRefusesBeforeCatalogLaunch(t *testing.T) {
	bin, _ := writeCursorCatalogPackage(t)
	cfg := &config.Config{Agent: types.AgentCursor, ACPXPath: bin, ACPRegistryOverrides: map[string]string{"cursor": bin}, DisableProjectSettings: true}
	err := preflightCursorSelections(context.Background(), cfg, filepath.Dir(bin), runenv.Overlay{})
	if err == nil || !strings.Contains(err.Error(), "does not neutralize") {
		t.Fatalf("trusted opt-out was bypassed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "catalog-probes")); !os.IsNotExist(err) {
		t.Fatal("unverified instruction-suppression adapter launched during catalog admission")
	}
}

func TestCursorPrimaryAliasDiagnosticNamesCorrectableField(t *testing.T) {
	for _, route := range []string{"admission", "invocation"} {
		t.Run(route, func(t *testing.T) {
			bin, modelCounter := writeCursorCatalogPackage(t)
			supported := "fixture-supported[mode=careful]"
			requested := "fixture-selected-alias-absent[mode=careful]"
			cfg := &config.Config{Agent: "acp:cursor", ACPXPath: bin, ACPRegistryOverrides: map[string]string{"cursor": bin}, AgentConfig: map[string]agentcfg.Profile{
				"cursor":     {Model: "fixture-unselected-cursor-absent"},
				"acp:cursor": {Model: requested},
			}}
			// The public invocation consumer must also succeed after correcting its
			// named selection field; this fixture emits a valid acpx reply only then.
			script := `#!/usr/bin/env node
import fs from 'node:fs';
fs.appendFileSync(` + strconvQuote(modelCounter) + `, 'model work\n');
fs.readFileSync(0);
console.log(JSON.stringify({method:'session/update',params:{update:{sessionUpdate:'agent_message_chunk',text:'alias correction accepted'}}}));
`
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			check := func() error {
				if route == "admission" {
					return preflightCursorSelections(context.Background(), cfg, filepath.Dir(bin), runenv.Overlay{})
				}
				ag, _, err := newConfiguredAgent(context.Background(), cfg, t.TempDir(), exec.LookPath, runenv.Overlay{}, nil)
				if err != nil {
					return err
				}
				defer ag.Close()
				result, err := ag.Run(context.Background(), agent.RunOpts{CWD: filepath.Dir(bin), Prompt: "synthetic correction check"})
				if err == nil && (result == nil || result.Text != "alias correction accepted") {
					t.Fatalf("corrected invocation reply: %+v", result)
				}
				return err
			}
			var refusal *agent.CursorModelError
			err := check()
			if !errors.As(err, &refusal) || refusal.Status != agent.CursorModelUnsupported || refusal.Model != requested {
				t.Fatalf("selected alias was not checked: %v", err)
			}
			if refusal.Owner != "agent_config['acp:cursor'].model" {
				t.Fatalf("diagnostic names a field that cannot correct the selected alias: %s", refusal.Owner)
			}
			if _, err := os.Stat(modelCounter); !os.IsNotExist(err) {
				t.Fatal("unsupported alias started dependent model work")
			}
			// Apply the correction to exactly the key named by the diagnostic. The
			// sibling cursor profile stays unsupported and cannot win this selection.
			cfg.AgentConfig["acp:cursor"] = agentcfg.Profile{Model: supported}
			if err := check(); err != nil {
				t.Fatalf("named field did not correct the selection: %v", err)
			}
			if cfg.AgentProfileFor(types.AgentCursor).Model != "fixture-unselected-cursor-absent" {
				t.Fatal("correction changed the unselected sibling profile")
			}
			if route == "invocation" {
				body, err := os.ReadFile(modelCounter)
				if err != nil || string(body) != "model work\n" {
					t.Fatalf("corrected invocation count: %q %v", body, err)
				}
			}
		})
	}
}
