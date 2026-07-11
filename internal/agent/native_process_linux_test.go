//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCodexAgentLeaderExitTerminatesChildRetainingPipes(t *testing.T) {
	dir := t.TempDir()
	bin := writeNativeAgentScript(t, dir, "codex", `#!/bin/sh
sleep 30 &
echo $! > "$(dirname "$0")/child.pid"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}'
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := (&codexAgent{bin: bin}).runOnce(ctx, RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
	})
	if err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("output = %s, want valid structured result", result.Output)
	}
	assertNativeChildStopped(t, filepath.Join(dir, "child.pid"))
}

func TestClaudeAgentSchemaErrorTerminatesChildRetainingPipes(t *testing.T) {
	dir := t.TempDir()
	bin := writeNativeAgentScript(t, dir, "claude", `#!/bin/sh
sleep 30 &
echo $! > "$(dirname "$0")/child.pid"
printf '%s\n' '{"type":"result","subtype":"success","structured_output":{"status":"http 429"}}'
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := (&claudeAgent{bin: bin}).runOnce(ctx, RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"}},"required":["findings"]}`),
	})
	if err == nil {
		t.Fatal("runOnce() succeeded with schema-invalid structured output")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runOnce() hung on the live child retaining pipes: %v", err)
	}
	if kind, ok := classifyOperationalFailure(err); ok {
		t.Fatalf("schema-invalid output classified as operational %q: %v", kind, err)
	}
	assertNativeChildStopped(t, filepath.Join(dir, "child.pid"))
}

func TestNativeProcessParseErrorTerminatesProcessGroup(t *testing.T) {
	dir := t.TempDir()
	bin := writeNativeAgentScript(t, dir, "native", `#!/bin/sh
sleep 30 &
echo $! > "$(dirname "$0")/child.pid"
printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'
sleep 30
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started, err := startNativeProcess(exec.CommandContext(ctx, bin))
	if err != nil {
		t.Fatalf("startNativeProcess() error = %v", err)
	}
	defer started.closePipes()

	var usage TokenUsage
	var lastMessage string
	var codexErr string
	parseErr := parseCodexEventsWithMaxTokenSize(ctx, started.stdout, nil, &usage, &lastMessage, &codexErr, nil, 32)
	if parseErr == nil || !strings.Contains(parseErr.Error(), "token too long") {
		t.Fatalf("parse error = %v, want token-too-long failure", parseErr)
	}
	if err := started.waitAfterParseError(parseErr); !errors.Is(err, parseErr) {
		t.Fatalf("waitAfterParseError() error = %v, want %v", err, parseErr)
	}
	assertNativeChildStopped(t, filepath.Join(dir, "child.pid"))
}

func TestCodexAgentCancellationTerminatesChildRetainingPipes(t *testing.T) {
	dir := t.TempDir()
	bin := writeNativeAgentScript(t, dir, "codex", `#!/bin/sh
sleep 30 &
echo $! > "$(dirname "$0")/child.pid"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"started"}}'
sleep 30
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cancelledFromChunk := false
	_, err := (&codexAgent{bin: bin}).runOnce(ctx, RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
		OnChunk: func(string) {
			cancelledFromChunk = true
			cancel()
		},
	})
	if err == nil {
		t.Fatal("runOnce() succeeded after cancellation")
	}
	if !cancelledFromChunk {
		t.Fatalf("agent never streamed the event that triggers cancellation: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v, want canceled", ctx.Err())
	}
	assertNativeChildStopped(t, filepath.Join(dir, "child.pid"))
}

func TestNativeProcessWaitDelayClosesRetainedPipes(t *testing.T) {
	dir := t.TempDir()
	bin := writeNativeAgentScript(t, dir, "native", `#!/bin/sh
setsid sleep 30 &
echo $! > "$(dirname "$0")/child.pid"
printf x
`)

	cmd := exec.CommandContext(context.Background(), bin)
	cmd.WaitDelay = 50 * time.Millisecond
	started, err := startNativeProcess(cmd)
	if err != nil {
		t.Fatalf("startNativeProcess() error = %v", err)
	}
	defer started.closePipes()
	_, _ = io.ReadAll(started.stdout)

	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("read escaped child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse escaped child pid %q: %v", raw, err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find escaped child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Kill()
		_ = child.Release()
	})

	if err := started.wait(); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("wait() error = %v, want exec.ErrWaitDelay", err)
	}
}

func writeNativeAgentScript(t *testing.T, dir, name, script string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return bin
}

func assertNativeChildStopped(t *testing.T, pidPath string) {
	t.Helper()
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("read child process state: %v", err)
		}
		fields := strings.Fields(string(stat))
		if len(fields) < 3 {
			t.Fatalf("malformed child process state: %q", stat)
		}
		if fields[2] == "Z" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d remains live in state %s", pid, fields[2])
		}
		runtime.Gosched()
	}
}
