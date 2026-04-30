package config

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentPath_Override(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}
	if got := cfg.AgentPath(); got != "/custom/claude" {
		t.Errorf("AgentPath() = %q, want %q", got, "/custom/claude")
	}
}

func TestAgentPath_DefaultBinaries(t *testing.T) {
	tests := []struct {
		agent types.AgentName
		want  string
	}{
		{types.AgentClaude, "claude"},
		{types.AgentCodex, "codex"},
		{types.AgentRovoDev, "acli"},
		{types.AgentOpenCode, "opencode"},
		{types.AgentPi, "pi"},
	}
	for _, tt := range tests {
		cfg := &Config{Agent: tt.agent}
		if got := cfg.AgentPath(); got != tt.want {
			t.Errorf("AgentPath() for %q = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"DEBUG", slog.LevelInfo}, // case-sensitive, unrecognized defaults to info
	}
	for _, tt := range tests {
		got := ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestResolveAgent_ExplicitAgent(t *testing.T) {
	// When agent is explicitly set (not auto), ResolveAgent returns it as-is.
	cfg := &Config{Agent: types.AgentCodex}
	err := cfg.ResolveAgent(context.Background(), func(string) (string, error) {
		t.Fatal("lookPath should not be called for explicit agent")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
}

func TestResolveAgent_AutoPicksFirstAvailable(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	// Simulate: claude not found, codex found
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
}

func TestResolveAgent_AutoPicksClaude(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "claude" {
			return "/usr/bin/claude", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
}

func TestResolveAgent_AutoRespectsPathOverride(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"opencode": "/custom/opencode"},
	}
	// Only opencode override path exists
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "/custom/opencode" {
			return "/custom/opencode", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentOpenCode {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentOpenCode)
	}
}

func TestResolveAgent_AutoSkipsMissingOverrideAndFallsBack(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "/custom/claude":
			return "", &exec.Error{Name: bin, Err: fs.ErrNotExist}
		case "codex":
			return "/usr/bin/codex", nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
}

func TestResolveAgent_AutoSkipsRovoDevWithoutSubcommand(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	originalProbe := probeRovoDevSupport
	probeRovoDevSupport = func(_ context.Context, bin string) (bool, error) {
		if bin != "/usr/bin/acli" {
			t.Fatalf("unexpected rovodev probe for %q", bin)
		}
		return false, nil
	}
	t.Cleanup(func() {
		probeRovoDevSupport = originalProbe
	})

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "claude", "codex", "opencode", "pi":
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		case "acli":
			return "/usr/bin/acli", nil
		default:
			t.Fatalf("unexpected probe for %q", bin)
			return "", nil
		}
	})

	if err == nil {
		t.Fatal("expected error when rovodev subcommand is unavailable")
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}

func TestResolveAgent_AutoReturnsRovoDevProbeExitError(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	script := filepath.Join(t.TempDir(), "acli")
	contents := []byte("#!/bin/sh\nexit 1\n")
	if runtime.GOOS == "windows" {
		script += ".cmd"
		contents = []byte("@echo off\r\nexit /b 1\r\n")
	}
	if err := os.WriteFile(script, contents, 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "claude", "codex", "opencode", "pi":
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		case "acli":
			return script, nil
		default:
			t.Fatalf("unexpected probe for %q", bin)
			return "", nil
		}
	})

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}

func TestResolveAgent_AutoReturnsOverrideProbeError(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}
	wantErr := &exec.Error{Name: "/custom/claude", Err: fs.ErrPermission}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "/custom/claude" {
			return "", wantErr
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})

	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission error, got %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}

func TestResolveAgent_AutoNoneAvailable(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected error when no agents found")
	}
	if !strings.Contains(err.Error(), "no supported agent found") {
		t.Errorf("expected 'no supported agent found' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("expected config guidance in error, got: %v", err)
	}
}

func TestResolveAgent_AutoNoneAvailableIncludesOverridePaths(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentAuto,
		AgentPathOverride: map[string]string{
			"claude":   "/custom/claude",
			"rovodev":  "/custom/acli",
			"opencode": "/custom/opencode",
		},
	}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})

	if err == nil {
		t.Fatal("expected error when no agents found")
	}
	for _, want := range []string{"/custom/claude", "/custom/opencode", "/custom/acli"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got: %v", want, err)
		}
	}
}

func TestResolveAgent_AutoPassesContextToRovoDevProbe(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	originalProbe := probeRovoDevSupport
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probeRovoDevSupport = func(ctx context.Context, bin string) (bool, error) {
		if bin != "/usr/bin/acli" {
			t.Fatalf("unexpected rovodev probe for %q", bin)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("probe context error = %v, want %v", ctx.Err(), context.Canceled)
		}
		return false, ctx.Err()
	}
	t.Cleanup(func() {
		probeRovoDevSupport = originalProbe
	})

	err := cfg.ResolveAgent(ctx, func(bin string) (string, error) {
		switch bin {
		case "claude", "codex", "opencode", "pi":
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		case "acli":
			return "/usr/bin/acli", nil
		default:
			t.Fatalf("unexpected probe for %q", bin)
			return "", nil
		}
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}
