//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeOpencodeHelpWithFlag is a shell script that emulates an opencode binary
// whose `serve --help` advertises --no-project-instructions (a supported
// binary from the paired OpenCode change).
const fakeOpencodeHelpWithFlag = `#!/bin/sh
# Emulate ` + "`opencode serve --help`" + ` on a binary that supports the flag.
if [ "$1" = "serve" ] && [ "$2" = "--help" ]; then
  cat <<'HELP'
opencode serve

starts a headless opencode server

Options:
      --no-project-instructions  disable project-level agent instructions  [boolean]
      --pure         run without external plugins                          [boolean]
      --port         port to listen on                                     [number]
HELP
  exit 0
fi
exit 0
`

// fakeOpencodeHelpWithoutFlag emulates an older opencode binary whose
// `serve --help` does NOT advertise --no-project-instructions.
const fakeOpencodeHelpWithoutFlag = `#!/bin/sh
if [ "$1" = "serve" ] && [ "$2" = "--help" ]; then
  cat <<'HELP'
opencode serve

starts a headless opencode server

Options:
      --pure         run without external plugins                          [boolean]
      --port         port to listen on                                     [number]
HELP
  exit 0
fi
exit 0
`

// writeFakeOpencode writes a fake opencode binary script to a temp dir and
// returns its path.
func writeFakeOpencode(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return bin
}

// TestProbeOpencodeNoProjectInstructions_RealProbeAdmitsSupported proves the
// real probe (not a mock) passes against a fake binary whose serve --help
// lists --no-project-instructions. This is the end-to-end proof that the
// help-text scan works against a yargs-style help output.
func TestProbeOpencodeNoProjectInstructions_RealProbeAdmitsSupported(t *testing.T) {
	bin := writeFakeOpencode(t, fakeOpencodeHelpWithFlag)
	original := probeOpencodeNoProjectInstructions
	t.Cleanup(func() { probeOpencodeNoProjectInstructions = original })
	probeOpencodeNoProjectInstructions = func(ctx context.Context, b string) error {
		return original(ctx, b)
	}
	if err := probeOpencodeNoProjectInstructions(context.Background(), bin); err != nil {
		t.Fatalf("supported binary must pass the real probe, got: %v", err)
	}
}

// TestProbeOpencodeNoProjectInstructions_RealProbeRefusesUnsupported proves
// the real probe fails closed with a concrete diagnostic against a fake binary
// whose serve --help omits --no-project-instructions (older OpenCode).
func TestProbeOpencodeNoProjectInstructions_RealProbeRefusesUnsupported(t *testing.T) {
	bin := writeFakeOpencode(t, fakeOpencodeHelpWithoutFlag)
	original := probeOpencodeNoProjectInstructions
	t.Cleanup(func() { probeOpencodeNoProjectInstructions = original })
	probeOpencodeNoProjectInstructions = func(ctx context.Context, b string) error {
		return original(ctx, b)
	}
	err := probeOpencodeNoProjectInstructions(context.Background(), bin)
	if err == nil {
		t.Fatal("unsupported binary must fail the real probe")
	}
	for _, want := range []string{"--no-project-instructions", "disable_project_settings", "upgrade OpenCode"} {
		if !contains(err.Error(), want) {
			t.Errorf("diagnostic must mention %q, got: %v", want, err)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOfString(s, substr) >= 0)
}

func indexOfString(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
