package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNewOMPAgent(t *testing.T) {
	a, err := New(types.AgentOMP, "omp", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Name() != "omp" {
		t.Fatalf("name = %q, want omp", a.Name())
	}
}

func TestNewRejectsOtherHarnesses(t *testing.T) {
	for _, name := range []types.AgentName{"claude", "codex", "pi", "acp:omp", ""} {
		if _, err := New(name, "x", nil); err == nil {
			t.Errorf("New(%q) must fail in an omp-only build", name)
		}
	}
}

func TestOMPBuildArgs(t *testing.T) {
	a := &ompAgent{bin: "omp", extraArgs: []string{"--model", "dashscope/qwen4-max"}}
	got := strings.Join(a.buildArgs(), "\x00")
	for _, want := range []string{"--model\x00dashscope/qwen4-max", "--mode\x00json", "--no-session"} {
		if !strings.Contains(got, want) {
			t.Errorf("buildArgs = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "--no-context-files") {
		t.Errorf("buildArgs = %q, suppression flags must require the opt-out", got)
	}
}

func TestOMPGateBuildArgsAppendSuppression(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "gate.yml")
	if err := os.WriteFile(overlay, []byte(ompGateOverlayYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &ompAgent{bin: "omp", extraArgs: []string{"--config", "operator.yml"}, disableProjectSettings: true, overlayPath: overlay}
	args := a.buildArgs()
	got := strings.Join(args, "\x00")
	for _, want := range []string{"--no-context-files", "--no-rules", "--no-skills", "--no-extensions", "--config\x00" + overlay} {
		if !strings.Contains(got, want) {
			t.Errorf("buildArgs = %q, want %q", got, want)
		}
	}
	if !NeutralizesGateInstructions(a) {
		t.Fatal("gate-neutralizing omp agent must report neutralized")
	}
	if NeutralizesGateInstructions(&ompAgent{bin: "omp"}) {
		t.Fatal("omp agent without the opt-out must not report neutralized")
	}
	if err := EnsureGateNeutralized(NewNoop()); err == nil {
		t.Fatal("noop agent must be refused as a gate agent")
	}
}

func TestOMPRunParsesJSONLAndStructuredOutput(t *testing.T) {
	lines := []string{
		`{"type":"session","version":3,"id":"s1","cwd":"/work"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[{"type":"text","text":"{\"ok\":"}],"provider":"dashscope","model":"qwen4-max"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"true}"}}`,
		`{"type":"message_end","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"{\"ok\":true}"}],"provider":"dashscope","model":"qwen4-max","usage":{"input":30,"output":5,"cacheRead":7,"cacheWrite":3}}}`,
		`{"type":"agent_end","messages":[{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"{\"ok\":true}"}],"provider":"dashscope","model":"qwen4-max","usage":{"input":30,"output":5,"cacheRead":7,"cacheWrite":3}}]}`,
	}
	bin := writeFakeOMP(t, lines, 0)
	a := &ompAgent{bin: bin}
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	res, err := a.Run(context.Background(), RunOpts{Prompt: "do work", CWD: t.TempDir(), JSONSchema: schema})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Output) == "" || !strings.Contains(string(res.Output), `"ok":true`) {
		t.Fatalf("output = %s, want validated JSON", res.Output)
	}
	if res.Model != "qwen4-max" || res.ModelProvider != "dashscope" {
		t.Fatalf("served identity = %q/%q", res.ModelProvider, res.Model)
	}
	if res.Usage.InputTokens != 40 || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v, want input 40 output 5", res.Usage)
	}
}

func TestOMPRunRejectsMalformedStructuredOutput(t *testing.T) {
	bin := writeFakeOMP(t, []string{
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"prose"}],"usage":{"input":1,"output":1}}}`,
	}, 0)
	a := &ompAgent{bin: bin}
	_, err := a.Run(context.Background(), RunOpts{
		Prompt:     "do work",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","required":["ok"]}`),
	})
	if err == nil || !IsStructuredOutputRejected(err) {
		t.Fatalf("err = %v, want structured-output rejection", err)
	}
	var violation *SchemaViolation
	if errors.As(err, &violation) {
		t.Fatalf("prose must not be reported as a schema violation: %v", err)
	}
}

func TestFinalizeTextResultNoSchema(t *testing.T) {
	res, err := finalizeTextResult("omp", "hello", nil, TokenUsage{})
	if err != nil || res.Text != "hello" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func writeFakeOMP(t *testing.T, jsonlLines []string, exitCode int) string {
	t.Helper()
	name := "omp"
	if runtime.GOOS == "windows" {
		t.Skip("fake omp shell script is POSIX-only")
	}
	bin := filepath.Join(t.TempDir(), name)
	lines := []string{"#!/bin/sh", `cat >/dev/null`}
	for _, l := range jsonlLines {
		lines = append(lines, "printf '%s\\n' "+shellQuote(l))
	}
	lines = append(lines, "exit "+itoa(exitCode))
	if err := os.WriteFile(bin, []byte(strings.Join(lines, "\n")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
