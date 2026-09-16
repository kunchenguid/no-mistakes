package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
// preceding user flag, and no --config is emitted when the repo did not opt out.
func TestOmpAgent_BuildArgs_NeutralizationOverlayComesFirst(t *testing.T) {
	oa := &ompAgent{bin: "omp", extraArgs: []string{"--model", "deepseek"}, disableProjectSettings: true}
	args := oa.buildArgs(nil, "/tmp/overlay.yml")

	expected := "--config /tmp/overlay.yml --model deepseek --mode json --no-session"
	if got := strings.Join(args, " "); got != expected {
		t.Fatalf("neutralized args = %q, want %q", got, expected)
	}

	// An operator-pinned --config replaces ours, so buildArgs adds none: two
	// would silently re-enable every instruction file (see
	// ompNeutralizationOverlay).
	pinned := &ompAgent{
		bin:                    "omp",
		extraArgs:              []string{"--config", "/tmp/operator.yml"},
		disableProjectSettings: true,
	}
	if got := strings.Join(pinned.buildArgs(nil, ""), " "); strings.Contains(got, "overlay") {
		t.Fatalf("operator-pinned --config must not gain a second overlay: %q", got)
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

// TestOmpAgent_WritesNeutralizationOverlayFromTempNotCheckout proves the overlay
// is materialized outside the validated worktree (which must stay clean) and
// names every project instruction file the controlled experiment confirmed.
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
	for _, want := range []string{
		"context-file:project:AGENTS.md",
		"context-file:project:CLAUDE.md",
		"context-file:project:copilot-instructions.md",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("overlay missing %q: %s", want, body)
		}
	}
	remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("overlay must be removed after the invocation: %v", err)
	}

	// No opt-out -> no file at all.
	plain := &ompAgent{bin: "omp"}
	p, rm, err := plain.writeNeutralizationOverlay()
	if err != nil || p != "" {
		t.Fatalf("repo without opt-out must not write an overlay: path=%q err=%v", p, err)
	}
	rm()
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

// ompNeutralizationExperiment drives the real omp binary to compare project
// instruction reachability with and without the neutralization overlay. It is
// the control-vs-neutralized experiment the adapter's claim rests on, so it
// skips (rather than passes) when omp is unavailable.
//
// The measurement is prompt token accounting rather than the model's
// self-report: an AGENTS.md of known size is placed in the working directory,
// and the assistant message's input+cacheRead tokens are compared across three
// runs. Without the overlay the file's contents are injected into the system
// prompt and the count rises by the file's own cost; with the overlay the count
// must return exactly to the file-free baseline. A self-report can be defeated
// by an agent that simply reads the file with a tool, which is why the file is
// never named in the prompt and the delta is what is asserted.
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
	neutral := filepath.Join(cwd, "neutral.yml")
	if err := os.WriteFile(base, []byte("memory:\n  backend: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(neutral, []byte("memory:\n  backend: none\n"+ompNeutralizationOverlay), 0o600); err != nil {
		t.Fatal(err)
	}

	measure := func(overlay string) int {
		t.Helper()
		cmd := exec.Command(bin, "--mode", "json", "--no-session", "--no-tools", "--config", overlay)
		cmd.Dir = cwd
		cmd.Stdin = strings.NewReader("Reply with the single word: ok\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("omp run failed: %v", err)
		}
		pp := &piParser{}
		if err := pp.parse(context.Background(), strings.NewReader(string(out))); err != nil {
			t.Fatalf("parse omp stream: %v", err)
		}
		return pp.usage.InputTokens + pp.usage.CacheReadTokens
	}

	baseline := measure(base)

	// A project instruction file large enough that its injection is
	// unmistakable in the token counts.
	filler := strings.Repeat("Repository convention line describing tooling and layout.\n", 400)
	agentsPath := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(filler), 0o600); err != nil {
		t.Fatal(err)
	}

	withControl := measure(base)
	if withControl <= baseline {
		t.Fatalf("control run must load AGENTS.md into context: baseline=%d control=%d", baseline, withControl)
	}

	withNeutral := measure(neutral)
	if withNeutral != baseline {
		t.Fatalf("neutralized run must not load AGENTS.md: baseline=%d neutralized=%d", baseline, withNeutral)
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
