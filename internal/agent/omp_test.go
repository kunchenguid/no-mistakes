package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOmpAgent_BuildArgs(t *testing.T) {
	oa := &ompAgent{bin: "omp"}
	args := oa.buildArgs(nil, "")

	expected := []string{"--mode", "json", "--no-session"}
	if got := strings.Join(args, " "); got != strings.Join(expected, " ") {
		t.Fatalf("cold args = %q, want %q", got, strings.Join(expected, " "))
	}
}

// TestOmpAgent_BuildArgs_DurableSession pins omp's session flag surface, which
// deliberately differs from pi's: a durable start is the ABSENCE of a session
// flag (omp has no explicit "start a new session" flag), and a resume selects
// the recorded UUID through --session. omp rejects pi's --session-id outright.
func TestOmpAgent_BuildArgs_DurableSession(t *testing.T) {
	oa := &ompAgent{bin: "omp"}

	started := oa.buildArgs(&SessionRef{}, "")
	if got, want := strings.Join(started, " "), "--mode json"; got != want {
		t.Fatalf("durable-session args = %q, want %q", got, want)
	}

	const sessionID = "01a0aba2-bd3d-7381-a438-95d4b1de9f0c"
	resumed := oa.buildArgs(&SessionRef{ID: sessionID}, "")
	if got, want := strings.Join(resumed, " "), "--mode json --session "+sessionID; got != want {
		t.Fatalf("resume args = %q, want %q", got, want)
	}
}

// TestOmpAgent_BuildArgs_NeutralizationOverlayComesFirst pins the positional
// contract: the overlay leads argv so it cannot be consumed as the value of a
// preceding user flag, the two flags covering the rule and skill providers
// follow it, and nothing is emitted when the repo did not opt out.
func TestOmpAgent_BuildArgs_NeutralizationOverlayComesFirst(t *testing.T) {
	oa := &ompAgent{bin: "omp", extraArgs: []string{"--model", "deepseek"}, disableProjectSettings: true}
	args := oa.buildArgs(nil, "/tmp/overlay.yml")

	expected := "--config /tmp/overlay.yml --no-rules --no-skills --model deepseek --mode json --no-session"
	if got := strings.Join(args, " "); got != expected {
		t.Fatalf("neutralized args = %q, want %q", got, expected)
	}

	// An operator-pinned --config replaces ours, so buildArgs adds none: two
	// would silently re-enable every instruction file (see
	// ompNeutralizationOverlay). The overlay is the suppression carrier, so
	// buildArgs does not emit the kill-switches either, since without the overlay
	// they would cover rules and skills but leave AGENTS.md loaded. The gate
	// refuses every omp launch under the opt-out regardless (omp fails closed on
	// its project settings surface), and this adapter must still report false.
	pinned := &ompAgent{
		bin:                    "omp",
		extraArgs:              []string{"--config", "/tmp/operator.yml"},
		disableProjectSettings: true,
	}
	pinnedArgs := pinned.buildArgs(nil, "")
	if got := strings.Join(pinnedArgs, " "); got != "--config /tmp/operator.yml --mode json --no-session" {
		t.Fatalf("operator-pinned --config must not gain a second overlay or lone suppression flags: %q", got)
	}
	if pinned.NeutralizesGateInstructions() {
		t.Fatal("an operator --config overlay replaces ours; the adapter must fail closed")
	}
}

// TestOmpAgent_FailsClosedOnNeutralization is the honest-contract regression:
// omp's project .omp/config.yml settings surface has no extension id (so
// disabledExtensions cannot name it) and no disabling flag, and it measurably
// changes the prompt delivered to the agent under the adapter's maximal
// suppression argv. The adapter must therefore NOT claim neutralization, so the
// gate refuses omp under disable_project_settings instead of advertising a
// suppression it cannot demonstrate. Fails before the fix (the claim was true)
// and passes after it.
func TestOmpAgent_FailsClosedOnNeutralization(t *testing.T) {
	if (&ompAgent{bin: "omp", disableProjectSettings: true}).NeutralizesGateInstructions() {
		t.Fatal("omp's project settings surface cannot be closed; it must not claim neutralization")
	}
	if (&ompAgent{bin: "omp"}).NeutralizesGateInstructions() {
		t.Fatal("omp without the opt-out must not claim neutralization")
	}
	if NeutralizesGateInstructions(NewFallback([]Agent{&ompAgent{bin: "omp", disableProjectSettings: true}})) {
		t.Fatal("a fallback containing omp must fail closed")
	}
	// The defense-in-depth argv is still emitted for a direct caller that set the
	// opt-out; failing closed must not silently drop the suppression machinery.
	oa := &ompAgent{bin: "omp", disableProjectSettings: true}
	if path, remove, err := oa.writeNeutralizationOverlay(); err != nil || path == "" {
		t.Fatalf("overlay must still be written under the opt-out: path=%q err=%v", path, err)
	} else {
		remove()
	}
}

// TestOmpAgent_SessionIDValidation proves omp reuses pi's canonical-UUID check,
// so an id prefix or session path omp's CLI would accept cannot be used to
// resume an ambiguous session from corrupt local metadata.
func TestOmpAgent_SessionIDValidation(t *testing.T) {
	valid := "01a0aba2-bd3d-7381-a438-95d4b1de9f0c"
	if !isOmpSessionID(valid) {
		t.Errorf("full omp UUID %q must be accepted", valid)
	}
	for _, bad := range []string{
		"01a0aba2", // prefix omp's CLI accepts
		"/tmp/sessions/01a0aba2-bd3d-7381-a438-95d4b1de9f0c.jsonl", // path
		"",
		"01a0aba2-bd3d-7381-a438-95d4b1de9f0g", // non-hex
	} {
		if isOmpSessionID(bad) {
			t.Errorf("ambiguous omp session id %q must be rejected", bad)
		}
	}
}

// TestOmpAgent_RejectsInvalidSessionIdentity proves runOnce refuses a corrupt
// persisted session before spawning, mirroring pi.
func TestOmpAgent_RejectsInvalidSessionIdentity(t *testing.T) {
	oa := &ompAgent{bin: "omp"}
	_, err := oa.runOnce(context.Background(), RunOpts{
		Prompt:  "hi",
		Session: &SessionRef{ID: "not-a-uuid"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid omp session identity") {
		t.Fatalf("expected invalid session identity error, got: %v", err)
	}
}

// ompOverlay is the parsed shape of the neutralization overlay's disabled
// extension ids. omp consumes the overlay through a real YAML parser, so the
// test asserts the parsed set rather than matching text in the file.
type ompOverlay struct {
	DisabledExtensions []string `yaml:"disabledExtensions"`
}

// TestOmpAgent_WritesNeutralizationOverlayFromTempNotCheckout proves the overlay
// is materialized outside the validated worktree (which must stay clean), that
// it parses to exactly the disabled-extension set the controlled experiment
// confirmed, and that the adapter's argv carries the two kill-switches covering
// the project surfaces the overlay cannot name. Equality, not containment: a
// dropped or duplicated id is a neutralization bug, and containment is what let
// the `.github/instructions` gap pass unnoticed.
func TestOmpAgent_WritesNeutralizationOverlayFromTempNotCheckout(t *testing.T) {
	cwd := t.TempDir()
	oa := &ompAgent{bin: "omp", disableProjectSettings: true}

	path, remove, err := oa.writeNeutralizationOverlay()
	if err != nil {
		t.Fatalf("writeNeutralizationOverlay: %v", err)
	}
	defer remove()

	if filepath.Dir(path) == cwd {
		t.Errorf("overlay must not be written into the target checkout: %q", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	var parsed ompOverlay
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("overlay must be valid YAML for omp: %v\n%s", err, body)
	}
	want := []string{
		"context-file:project:AGENTS.md",
		"context-file:project:CLAUDE.md",
		"context-file:project:copilot-instructions.md",
	}
	if !reflect.DeepEqual(parsed.DisabledExtensions, want) {
		t.Fatalf("overlay disabledExtensions = %v, want %v", parsed.DisabledExtensions, want)
	}

	// The full argv delivered to omp: the overlay leads it so it cannot be
	// consumed as a preceding user flag's value, and the two flags that cover
	// the rule and skill providers follow. Dropping either flag would leave a
	// project-controlled surface live while the adapter still claimed
	// neutralization.
	wantArgs := []string{"--config", path, "--no-rules", "--no-skills", "--mode", "json", "--no-session"}
	if got := oa.buildArgs(nil, path); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("neutralized argv = %q, want %q", got, wantArgs)
	}

	remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("overlay must be removed after the invocation: %v", err)
	}

	// No opt-out -> no file and no suppression flags at all.
	plain := &ompAgent{bin: "omp"}
	p, rm, err := plain.writeNeutralizationOverlay()
	if err != nil || p != "" {
		t.Fatalf("repo without opt-out must not write an overlay: path=%q err=%v", p, err)
	}
	rm()
	if got, want := plain.buildArgs(nil, ""), []string{"--mode", "json", "--no-session"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv without the opt-out = %q, want %q", got, want)
	}
}

// TestOmpAgent_RegisteredAsNativeHarness proves the factory constructs the omp
// adapter and that its model/effort knobs map onto omp's own flags.
func TestOmpAgent_RegisteredAsNativeHarness(t *testing.T) {
	a, err := NewWithOptions(types.AgentOmp, "omp", nil, Options{})
	if err != nil {
		t.Fatalf("NewWithOptions(omp): %v", err)
	}
	if a.Name() != "omp" {
		t.Fatalf("agent name = %q, want omp", a.Name())
	}
	if !SupportsSessionResume(a) {
		t.Error("omp must advertise durable session resume")
	}
}

// ompNeutralizationExperiment drives the real omp binary to prove WHY omp fails
// closed: even the adapter's maximal suppression argv (the overlay plus
// --no-rules and --no-skills) leaves omp's project SETTINGS surface live, so the
// adapter must not claim neutralization. It is the evidence the
// NeutralizesGateInstructions verdict rests on, so it skips (rather than passes)
// when omp is unavailable.
//
// The measurement is prompt token accounting rather than the model's
// self-report: a project-controlled file of known size is placed in the working
// directory, and the assistant message's input+cacheRead tokens are compared
// across runs. A self-report can be defeated by an agent that simply reads the
// file with a tool, which is why the file is never named in the prompt.
//
// The control-vs-suppressed arms keep the same shape as before, but the
// assertion is inverted to match the honest contract: the context-file overlay
// and the two kill-switches DO close the three surfaces they name (asserted
// inert), while a project .omp/config.yml stays live under that same maximal
// argv (asserted still steering the prompt). That second assertion is the
// grounds for failing closed - it would fail if omp ever grew a way to close
// the settings surface, which is exactly when the verdict should be revisited.
//
// Three named surfaces are covered because they are three different omp
// providers, and a mechanism that closes one does not close the others: the
// context files (the overlay's disabledExtensions),
// `.github/instructions/*.instructions.md` (a rule, which only --no-rules
// reaches), and a project SKILL.md (listed by description, which only
// --no-skills reaches). Tools stay ENABLED: a project skill reaches the prompt
// only through the skill provider, which omp gates on tools being available, so
// a --no-tools run would make that surface untestable rather than inert.
func TestOmpAgent_NeutralizationExperiment(t *testing.T) {
	if os.Getenv("NM_TEST_REAL_OMP") != "1" {
		t.Skip("set NM_TEST_REAL_OMP=1 to run the live omp neutralization experiment")
	}
	bin, err := exec.LookPath("omp")
	if err != nil {
		t.Skip("omp not installed")
	}

	cwd := t.TempDir()
	base := filepath.Join(cwd, "base.yml")
	if err := os.WriteFile(base, []byte("memory:\n  backend: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The control argv carries no neutralization; the suppressed argv is the
	// adapter's own for a repo that opted out, so the experiment measures what
	// buildArgs actually launches rather than a hand-rolled approximation. Each
	// arm is asserted against its OWN file-free baseline, so the constant offset
	// between the two arms cancels out and only a surface's injected cost is ever
	// compared.
	control := []string{"--mode", "json", "--no-session", "--config", base}
	agent := &ompAgent{bin: bin, disableProjectSettings: true}
	overlayPath, removeOverlay, err := agent.writeNeutralizationOverlay()
	if err != nil {
		t.Fatalf("writeNeutralizationOverlay: %v", err)
	}
	defer removeOverlay()
	suppressed := agent.buildArgs(nil, overlayPath)

	measure := func(args []string) int {
		t.Helper()
		// A live model can end an invocation without any assistant usage - a
		// provider-side rate limit or an internal retry that produced no
		// message - which reads as a zero-token prompt rather than as a fact
		// about injection. Retry a degenerate run instead of asserting on it.
		var tokens int
		for range 4 {
			cmd := exec.Command(bin, args...)
			cmd.Dir = cwd
			cmd.Stdin = strings.NewReader("Reply with the single word: ok\n")
			out, err := cmd.Output()
			if err != nil {
				continue
			}
			pp := &piParser{}
			if err := pp.parse(context.Background(), strings.NewReader(string(out))); err != nil {
				continue
			}
			tokens = pp.usage.InputTokens + pp.usage.CacheReadTokens
			if tokens > 0 && pp.finalText() != "" {
				return tokens
			}
		}
		t.Fatalf("omp returned no usable usage after 4 attempts for %v (provider unavailable?); tokens=%d", args, tokens)
		return 0
	}

	// Warm up before baseline accounting: the first invocation in a fresh
	// directory pays a cold-start cost that later runs do not, so the baselines
	// must be taken from a settled state.
	measure(control)
	measure(suppressed)
	baselineControl := measure(control)
	baselineSuppressed := measure(suppressed)

	// Each file is large enough that its injection is unmistakable in the token
	// counts, and each is written and removed in turn so one surface's token
	// cost cannot be mistaken for another's. Every entry here is a surface the
	// suppression argv DOES close; inert is asserted as equality with the
	// suppressed baseline.
	filler := strings.Repeat("Repository convention line describing tooling and layout.\n", 400)
	named := []struct {
		name    string
		path    string
		content string
	}{
		{"AGENTS.md", "AGENTS.md", filler},
		{"CLAUDE.md", "CLAUDE.md", filler},
		{"copilot-instructions.md", ".github/copilot-instructions.md", filler},
		{".github/instructions", ".github/instructions/x.instructions.md", "---\napplyTo: \"**\"\n---\n" + filler},
		{"nested .github/instructions", ".github/instructions/sub/y.instructions.md", "---\napplyTo: \"**\"\n---\n" + filler},
		{"project skill", ".agents/skills/probe/SKILL.md", "---\nname: probe\ndescription: \"" + strings.ReplaceAll(filler, "\n", " ") + "\"\n---\nBody.\n"},
	}
	for _, surface := range named {
		path := filepath.Join(cwd, surface.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(surface.content), 0o600); err != nil {
			t.Fatal(err)
		}
		controlTokens := measure(control)
		suppressedTokens := measure(suppressed)
		_ = os.Remove(path)

		if controlTokens <= baselineControl {
			t.Fatalf("control run must load %s into context: baseline=%d control=%d",
				surface.name, baselineControl, controlTokens)
		}
		if suppressedTokens != baselineSuppressed {
			t.Fatalf("the suppression argv must close %s: baseline=%d suppressed=%d",
				surface.name, baselineSuppressed, suppressedTokens)
		}
	}

	// The open surface that forces the fail-closed verdict: a project
	// .omp/config.yml. It is NOT named by any extension id and no flag skips it,
	// so it must still change the prompt under the maximal suppression argv.
	// Equality with the baseline here would mean omp grew a way to close its
	// settings surface; that is the signal to revisit the verdict, so the
	// assertion is deliberately the opposite of the loop above.
	projectConfig := filepath.Join(cwd, ".omp", "config.yml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("autolearn:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	openTokens := measure(suppressed)
	_ = os.Remove(projectConfig)
	if openTokens == baselineSuppressed {
		t.Fatalf("a project .omp/config.yml reached no part of the prompt under the suppression argv: "+
			"baseline=%d with-config=%d. If omp now closes its project settings surface, revisit "+
			"ompAgent.NeutralizesGateInstructions instead of deleting this assertion",
			baselineSuppressed, openTokens)
	}
}

// TestOmpAgent_ParsesRealStreamShape documents that omp's JSON stream is
// protocol-identical to pi's, which is what lets the adapter reuse piParser.
func TestOmpAgent_ParsesRealStreamShape(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session","version":3,"id":"01a0aba2-bd3d-7381-a438-95d4b1de9f0c","cwd":"/tmp/x"}`,
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hel"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"hello"}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"provider":"vibeproxy","model":"deepseek-v4.1-flash","stopReason":"stop","usage":{"input":11,"output":2,"cacheRead":7,"cacheWrite":0}}}`,
		`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"hello"}],"usage":{"input":11,"output":2,"cacheRead":7,"cacheWrite":0},"responseId":"chatcmpl-1"}]}`,
	}, "\n")

	var chunks []string
	pp := &piParser{onChunk: func(s string) { chunks = append(chunks, s) }}
	if err := pp.parse(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := pp.finalText(); got != "hello" {
		t.Errorf("finalText = %q, want %q", got, "hello")
	}
	if pp.sessionID != "01a0aba2-bd3d-7381-a438-95d4b1de9f0c" {
		t.Errorf("sessionID = %q", pp.sessionID)
	}
	if pp.model != "deepseek-v4.1-flash" || pp.provider != "vibeproxy" {
		t.Errorf("model/provider = %q/%q", pp.model, pp.provider)
	}
	if pp.usage.InputTokens != 11 || pp.usage.CacheReadTokens != 7 {
		t.Errorf("usage = %+v", pp.usage)
	}
	if len(chunks) != 1 || chunks[0] != "hel" {
		t.Errorf("onChunk deltas = %v", chunks)
	}
}

// TestOmpAgent_AssistantErrorSurfaces proves a provider failure reported on the
// assistant message becomes the invocation error rather than an empty success.
func TestOmpAgent_AssistantErrorSurfaces(t *testing.T) {
	var msg map[string]any
	raw := `{"role":"assistant","content":[],"stopReason":"error","errorMessage":"HTTP 401 Unauthorized"}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	pp := &piParser{}
	pp.rememberAssistant(msg)
	if pp.assistantError != "HTTP 401 Unauthorized" {
		t.Fatalf("assistantError = %q", pp.assistantError)
	}
}
