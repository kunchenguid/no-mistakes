package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func cursorCatalogFixture(t *testing.T, body string) (*acpxAgent, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic direct Node shebang fixture requires POSIX; unknown Windows wrappers refuse closed")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for the public SDK fixture")
	}
	dir := t.TempDir()
	files := map[string]string{
		"package.json": `{"name":"acpx","type":"module","bin":{"acpx":"cli.js"},"exports":{"./runtime":"./runtime.js","./agent-registry":"./registry.js"}}`,
		"cli.js":       "#!/usr/bin/env node\nprocess.exit(99);\n",
		"runtime.js":   body,
		"registry.js":  `export function createAgentRegistry({overrides}) { if(overrides.cursor !== 'synthetic-cursor --opaque') throw new Error(); return {inspect: () => ({launch:{kind:'installed',argv:['synthetic-cursor','--opaque']}})}; }`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return &acpxAgent{bin: filepath.Join(dir, "cli.js"), target: "cursor", rawCommand: "synthetic-cursor --opaque", model: "fixture-model[mode=careful]"}, dir
}

func TestCursorModelCatalogOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     CursorModelStatus
	}{
		{"exact opaque ID", `return {availableModelIds:['fixture-model[mode=careful]'],currentModelId:'fixture-model[mode=careful]'}`, CursorModelSupported},
		{"no suffix matching", `return {availableModelIds:['fixture-model'],currentModelId:'fixture-model'}`, CursorModelUnsupported},
		{"empty", `return {availableModelIds:[]}`, CursorModelIndeterminate},
		{"malformed", `return {availableModelIds:[47]}`, CursorModelIndeterminate},
		{"inconsistent current", `return {availableModelIds:['fixture-model[mode=careful]'],currentModelId:'other'}`, CursorModelIndeterminate},
		{"duplicate IDs", `return {availableModelIds:['fixture-model[mode=careful]','fixture-model[mode=careful]']}`, CursorModelIndeterminate},
		{"authentication", `throw new Error('private-token /private/account catalog-content authentication required')`, CursorModelIndeterminate},
		{"nonzero", `process.exit(7)`, CursorModelIndeterminate},
		{"malformed output", `process.stdout.write('not-json'); return {availableModelIds:['fixture-model[mode=careful]']}`, CursorModelIndeterminate},
		{"metadata limit", `return {availableModelIds:['fixture-model[mode=careful]', 'x'.repeat(17000)]}`, CursorModelIndeterminate},
		{"output limit", `process.stdout.write('x'.repeat(100000)); return {availableModelIds:['fixture-model[mode=careful]']}`, CursorModelIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, dir := cursorCatalogFixture(t, "export async function inspectAgentModels() {"+tc.body+"}")
			err := a.preflightCursor(context.Background(), RunOpts{CWD: dir}, "review_agents.reviewer.model")
			if tc.status == CursorModelSupported {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var refusal *CursorModelError
			if !errors.As(err, &refusal) || refusal.Status != tc.status {
				t.Fatalf("got %v, want %s", err, tc.status)
			}
			for _, secret := range []string{"private-token", "/private/account", "catalog-content", dir} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("diagnostic leaked backend/context data: %v", err)
				}
			}
			if !strings.Contains(err.Error(), a.model) || !strings.Contains(err.Error(), "review_agents.reviewer.model") {
				t.Fatalf("missing correction owner/model: %v", err)
			}
		})
	}
}

func TestCursorModelDefaultMustBeAdvertised(t *testing.T) {
	for _, current := range []string{"", ",currentModelId:'fixture-default'"} {
		a, dir := cursorCatalogFixture(t, "export async function inspectAgentModels(){return {availableModelIds:['fixture-default']"+current+"}}")
		a.model = ""
		err := a.preflightCursor(context.Background(), RunOpts{CWD: dir}, "agent_config.cursor")
		if (err == nil) != (current != "") {
			t.Fatalf("default observation %q: %v", current, err)
		}
		if a.model != "" {
			t.Fatal("default observation fabricated an explicit model pin")
		}
	}
}

func TestCursorModelTimeoutAndMissingDependencies(t *testing.T) {
	a, dir := cursorCatalogFixture(t, `export async function inspectAgentModels(){await new Promise(()=>setInterval(()=>{},1000));}`)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	var refusal *CursorModelError
	if err := a.preflightCursor(ctx, RunOpts{CWD: dir}, "agent_config.cursor"); !errors.As(err, &refusal) || refusal.Status != CursorModelIndeterminate {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("catalog cancellation exceeded cleanup bound")
	}
	for _, change := range []func(){
		func() { a.bin = filepath.Join(dir, "missing") },
		func() {
			a.bin = filepath.Join(dir, "wrapper")
			if err := os.WriteFile(a.bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			a.bin = filepath.Join(dir, "cli.js")
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"other"}`), 0600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		change()
		if err := a.preflightCursor(context.Background(), RunOpts{CWD: dir}, "agent_config.cursor"); !errors.As(err, &refusal) || refusal.Status != CursorModelIndeterminate {
			t.Fatalf("dependency failure: %v", err)
		}
	}
}

func TestCursorModelFreshInvocationContext(t *testing.T) {
	a, dir := cursorCatalogFixture(t, `import fs from 'node:fs'; export async function inspectAgentModels(opts){fs.appendFileSync(process.env.FIXTURE_LOG,JSON.stringify({cwd:opts.cwd,agentCommand:opts.agentCommand,marker:process.env.FIXTURE_MARKER,gate:process.env.NO_MISTAKES_GATE,compact:process.env.COMPACT_ADVISER_DISABLE})+'\n'); return {availableModelIds:[process.env.FIXTURE_MODEL]}}`)
	log := filepath.Join(dir, "context.jsonl")
	a.subprocessContext = newSubprocessContext(runenv.Overlay{Set: map[string]string{"FIXTURE_LOG": log}})
	for _, model := range []string{a.model, "changed-model"} {
		opts := RunOpts{CWD: dir, Env: []string{"FIXTURE_MARKER=invocation", "FIXTURE_MODEL=" + model}}
		err := a.preflightCursor(context.Background(), opts, "agent_config.cursor")
		if (err == nil) != (model == a.model) {
			t.Fatalf("context changed: %v", err)
		}
	}
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected fresh probe per context, got %d", len(lines))
	}
	var got struct {
		CWD                   string
		AgentCommand          []string
		Marker, Gate, Compact string
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	physicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.CWD != physicalDir || got.Marker != "invocation" || got.Gate != "1" || got.Compact != "1" || fmt.Sprint(got.AgentCommand) != "[synthetic-cursor --opaque]" {
		t.Fatalf("wrong effective context: %+v", got)
	}
}

func TestNonCursorModelsDoNotProbe(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentPi, types.AgentClaude, "acp:other"} {
		if err := PreflightCursorModel(context.Background(), name, "missing", Options{Profile: agentcfg.Profile{Model: "immutable-model"}}, RunOpts{CWD: t.TempDir()}, "unused"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCursorRefusalIsTerminalRegardlessOfOpaqueModelText(t *testing.T) {
	for _, model := range []string{"provider exited: broken", "overloaded 503", "usage limit reached", "rate_limit_error"} {
		for _, status := range []CursorModelStatus{CursorModelUnsupported, CursorModelIndeterminate} {
			refusal := &CursorModelError{Status: status, Owner: "review_agents.reviewer.model", Model: model}
			for _, err := range []error{refusal, fmt.Errorf("outer context: %w", refusal)} {
				if _, retry := classifyTransient(err); retry {
					t.Fatalf("refusal retried for opaque ID %q", model)
				}
				if IsQuotaExhaustionError(err) || isAgentUnavailableError(err) {
					t.Fatalf("refusal became a fallback for opaque ID %q", model)
				}
				first := &fallbackTestAgent{name: "acp:cursor", run: func() (*Result, error) { return nil, err }}
				next := &fallbackTestAgent{name: "claude", run: func() (*Result, error) { return &Result{Text: "must not run"}, nil }}
				_, got := NewFallback([]Agent{first, next}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{})
				var actual *CursorModelError
				if !errors.As(got, &actual) || first.calls != 1 || next.calls != 0 {
					t.Fatalf("refusal triggered fallback: first=%d next=%d err=%v", first.calls, next.calls, got)
				}
			}
		}
	}
}

func TestCursorCatalogMissingExportsAndAmbiguousLaunchRefuse(t *testing.T) {
	for _, tc := range []struct{ name, file, body string }{
		{"missing runtime export", "runtime.js", `export const other = true;`},
		{"missing registry capability", "registry.js", `export function createAgentRegistry(){return {}}`},
		{"package execution plan", "registry.js", `export function createAgentRegistry(){return {inspect:()=>({launch:{kind:'missing',requirements:[{kind:'package',name:'synthetic'}]}})}}`},
		{"installed package runner", "registry.js", `export function createAgentRegistry(){return {inspect:()=>({launch:{kind:'installed',argv:['npx','synthetic-package']}})}}`},
		{"shell wrapper", "registry.js", `export function createAgentRegistry(){return {inspect:()=>({launch:{kind:'installed',argv:['sh','-c','synthetic']}})}}`},
		{"malformed argv", "registry.js", `export function createAgentRegistry(){return {inspect:()=>({launch:{kind:'installed',argv:[]}})}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, dir := cursorCatalogFixture(t, `import fs from 'node:fs'; export async function inspectAgentModels(){fs.writeFileSync('probe-called','unexpected'); return {availableModelIds:['fixture-model[mode=careful]']}}`)
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			var refusal *CursorModelError
			if err := a.preflightCursor(context.Background(), RunOpts{CWD: dir}, "agent_config.cursor"); !errors.As(err, &refusal) || refusal.Status != CursorModelIndeterminate {
				t.Fatalf("unavailable capability/launch: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "probe-called")); !os.IsNotExist(err) {
				t.Fatal("ambiguous launcher reached catalog process")
			}
		})
	}
}

func TestCursorModelExecutablePathHomeAndRegistryStayBound(t *testing.T) {
	first, firstDir := cursorCatalogFixture(t, `export async function inspectAgentModels(){return {availableModelIds:[process.env.HOME.endsWith('allowed-home')?'fixture-model[mode=careful]':'other']}}`)
	second, secondDir := cursorCatalogFixture(t, `export async function inspectAgentModels(){return {availableModelIds:['other']}}`)
	for _, a := range []*acpxAgent{first, second} {
		if err := os.Symlink(a.bin, filepath.Join(filepath.Dir(a.bin), "acpx")); err != nil {
			t.Fatal(err)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	first.bin = "acpx"
	allowedHome := filepath.Join(t.TempDir(), "allowed-home")
	if err := os.Mkdir(allowedHome, 0700); err != nil {
		t.Fatal(err)
	}
	opts := func(dir, home string) RunOpts {
		return RunOpts{CWD: firstDir, Env: []string{"PATH=" + dir + string(os.PathListSeparator) + filepath.Dir(node), "HOME=" + home}}
	}
	if err := first.preflightCursor(context.Background(), opts(firstDir, allowedHome), "agent_config.cursor"); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []RunOpts{opts(firstDir, t.TempDir()), opts(secondDir, allowedHome)} {
		var refusal *CursorModelError
		if err := first.preflightCursor(context.Background(), changed, "agent_config.cursor"); !errors.As(err, &refusal) || refusal.Status != CursorModelUnsupported {
			t.Fatalf("reused unrelated PATH/HOME catalog: %v", err)
		}
	}
	first.rawCommand = "changed-registry --opaque"
	var refusal *CursorModelError
	if err := first.preflightCursor(context.Background(), opts(firstDir, allowedHome), "agent_config.cursor"); !errors.As(err, &refusal) || refusal.Status != CursorModelIndeterminate {
		t.Fatalf("reused unrelated registry catalog: %v", err)
	}
}
