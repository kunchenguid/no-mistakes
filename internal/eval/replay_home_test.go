package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const syntheticPiAuth = `{"xai":{"type":"oauth","access":"SYNTHETIC_OAUTH_MARKER_NOT_A_SECRET"}}` + "\n"

const claudeHOMEProbeReply = `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"clean"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"},"usage":{"input_tokens":12,"output_tokens":3}}
`

const piHOMEProbeReply = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"{\"findings\":[],\"risk_level\":\"low\",\"risk_rationale\":\"clean\",\"risk_scope\":\"source-or-external\"}"}]}]}
`

// TestReplayUsesCallerHOMEAndKeepsIsolatedNMHOME is the behavioral half of
// dropping eval's empty-HOME rewrite: candidates inherit the caller's HOME
// (so Pi's ordinary ~/.pi/agent auth discovery works without an injected API
// key) while NM_HOME stays a nested sandbox that cannot see production state.
func TestReplayUsesCallerHOMEAndKeepsIsolatedNMHOME(t *testing.T) {
	ctx := context.Background()
	hideHarnessDirOverrides(t)
	unsetEnv(t, "XAI_API_KEY")

	sentinel := t.TempDir()
	t.Setenv("HOME", sentinel)
	t.Setenv("USERPROFILE", sentinel)
	writeFile(t, filepath.Join(sentinel, ".pi", "agent", "auth.json"), syntheticPiAuth)

	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	probeDir := t.TempDir()
	fakeDir := t.TempDir()
	installHOMEProbeHarness(t, fakeDir, probeDir, "claude", claudeHOMEProbeReply)
	installHOMEProbeHarness(t, fakeDir, probeDir, "pi", piHOMEProbeReply)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("captured cases = %d, want 1", len(cases))
	}
	labelsBefore, err := os.ReadFile(filepath.Join(cases[0].Dir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(cases[0].Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []Candidate{
		{Agent: types.AgentClaude, Model: "test"},
		{Agent: types.AgentPi, Model: "test"},
	} {
		name := string(candidate.Agent)
		probePath := filepath.Join(probeDir, name+".probe")
		_ = os.Remove(probePath)
		_, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: candidate, Repeats: 1})
		if err != nil {
			t.Fatalf("%s Replay: %v", name, err)
		}
		if len(evaluations) != 1 || evaluations[0].Status != "completed" {
			t.Fatalf("%s replay = %#v", name, evaluations)
		}
		obs := readProbe(t, probePath)
		if obs["home"] != sentinel {
			t.Fatalf("%s HOME=%q, want caller sentinel %q", name, obs["home"], sentinel)
		}
		if obs["nm_home"] == "" || obs["nm_home"] == p.Root() {
			t.Fatalf("%s NM_HOME=%q leaked production NM_HOME %q", name, obs["nm_home"], p.Root())
		}
		if obs["xai_api_key"] != "" {
			t.Fatalf("%s XAI_API_KEY=%q, want unset (eval must not inject an API key)", name, obs["xai_api_key"])
		}
		if _, err := os.Stat(filepath.Join(p.Root(), "shared-home-used")); !os.IsNotExist(err) {
			t.Fatalf("%s candidate used production NM_HOME", name)
		}
		if name == "pi" {
			if obs["home_auth_readable"] != "1" {
				t.Fatalf("pi did not see $HOME/.pi/agent/auth.json under the caller HOME")
			}
			if runtime.GOOS != "windows" && !strings.Contains(obs["argv"], "--no-session") {
				t.Fatalf("pi argv=%q, want review --no-session", obs["argv"])
			}
		}
	}

	labelsAfter, err := os.ReadFile(filepath.Join(cases[0].Dir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifestAfter, err := os.ReadFile(filepath.Join(cases[0].Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(labelsAfter) != string(labelsBefore) || string(manifestAfter) != string(manifestBefore) {
		t.Fatal("replay mutated captured labels or manifest")
	}

	pipelineBin := filepath.Join(fakeDir, "pi-pipeline")
	if runtime.GOOS == "windows" {
		pipelineBin += ".cmd"
	}
	installNamedHOMEProbeHarness(t, pipelineBin, filepath.Join(probeDir, "pipeline.probe"), piHOMEProbeReply)
	pipelineAgent, err := agent.NewWithOptions(types.AgentPi, pipelineBin, nil, agent.Options{
		Profile: agentcfg.Profile{Model: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pipelineAgent.Close()
	schema := json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"}},"required":["findings"]}`)
	if _, err := pipelineAgent.Run(ctx, agent.RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: schema,
	}); err != nil {
		t.Fatalf("pipeline-like pi.Run: %v", err)
	}
	pipelineObs := readProbe(t, filepath.Join(probeDir, "pipeline.probe"))
	replayObs := readProbe(t, filepath.Join(probeDir, "pi.probe"))
	if pipelineObs["home"] != replayObs["home"] {
		t.Fatalf("pipeline HOME=%q eval HOME=%q, want eval to inherit the same caller HOME as a pipeline spawn", pipelineObs["home"], replayObs["home"])
	}
	if pipelineObs["home"] != sentinel {
		t.Fatalf("pipeline-like spawn HOME=%q, want sentinel %q", pipelineObs["home"], sentinel)
	}
}

func hideHarnessDirOverrides(t *testing.T) {
	t.Helper()
	unsetEnv(t, "PI_CODING_AGENT_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "GH_CONFIG_DIR")
}

func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		key := key
		orig, ok := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(key, orig)
			} else {
				os.Unsetenv(key)
			}
		})
	}
}

func installHOMEProbeHarness(t *testing.T, fakeDir, probeDir, name, reply string) {
	t.Helper()
	path := filepath.Join(fakeDir, name)
	if runtime.GOOS == "windows" {
		path += ".cmd"
	}
	installNamedHOMEProbeHarness(t, path, filepath.Join(probeDir, name+".probe"), reply)
}

func installNamedHOMEProbeHarness(t *testing.T, path, probePath, reply string) {
	t.Helper()
	var script string
	if runtime.GOOS == "windows" {
		script = "@echo off\r\n" +
			"more >nul\r\n" +
			">\"" + probePath + "\" echo home=%HOME%\r\n" +
			">>\"" + probePath + "\" echo nm_home=%NM_HOME%\r\n" +
			">>\"" + probePath + "\" echo xai_api_key=%XAI_API_KEY%\r\n" +
			"if exist \"%HOME%\\.pi\\agent\\auth.json\" (>>\"" + probePath + "\" echo home_auth_readable=1) else (>>\"" + probePath + "\" echo home_auth_readable=0)\r\n" +
			">>\"" + probePath + "\" echo argv=%*\r\n" +
			"echo " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n" +
			"PROBE=\"" + probePath + "\"\n" +
			"HOME_AUTH=0\n" +
			"[ -f \"$HOME/.pi/agent/auth.json\" ] && HOME_AUTH=1\n" +
			"{\n" +
			"  printf 'home=%s\\n' \"$HOME\"\n" +
			"  printf 'nm_home=%s\\n' \"$NM_HOME\"\n" +
			"  printf 'xai_api_key=%s\\n' \"$XAI_API_KEY\"\n" +
			"  printf 'home_auth_readable=%s\\n' \"$HOME_AUTH\"\n" +
			"  printf 'argv=%s\\n' \"$*\"\n" +
			"} > \"$PROBE\"\n" +
			"cat >/dev/null\n" +
			"cat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readProbe(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("read probe %s: %v", path, err)
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
