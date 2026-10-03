package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiRunReattachRequiresTheCallersClaudeProfile(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})

	for _, tc := range []struct {
		name, bound, env string
		refused          bool
		hint             string
	}{
		{name: "same profile", bound: absTestPath("/caller/.claude1"), env: absTestPath("/caller/.claude1")},
		{name: "same profile as the push hook bound it", bound: absTestPath("/caller/.claude1/"), env: absTestPath("/caller/.claude1/")},
		{name: "unset requests no profile", bound: absTestPath("/caller/.claude1"), env: ""},
		{name: "other profile", bound: absTestPath("/caller/.claude1"), env: absTestPath("/caller/.claude2"), refused: true, hint: "Set CLAUDE_CONFIG_DIR to the active run's value to reattach"},
		{name: "run on the daemon's default", bound: "", env: absTestPath("/caller/.claude2"), refused: true, hint: "Unset CLAUDE_CONFIG_DIR to reattach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound := tc.bound
			boundRun := func(status types.RunStatus) *ipc.RunInfo {
				run := fx.running()
				run.Status = status
				run.ClaudeConfigDir = bound
				return run
			}
			fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return boundRun(types.RunRunning), nil })
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return boundRun(types.RunCompleted), nil })
			t.Setenv(runenv.ClaudeConfigDirEnvVar, tc.env)
			cmd := newAxiRunCmd()
			cmd.SetArgs(nil)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			var ee *exitError
			refused := errors.As(err, &ee) && ee.code == 2 && strings.Contains(out.String(), "uses Claude profile "+bound)
			if refused != tc.refused {
				t.Fatalf("refused = %v, want %v: %v\n%s", refused, tc.refused, err, out.String())
			}
			if !strings.Contains(out.String(), tc.hint) {
				t.Fatalf("refusal hint = %q, want it to contain %q", out.String(), tc.hint)
			}
			if !tc.refused && (err != nil || !strings.Contains(out.String(), "claude_config_dir: "+bound)) {
				t.Fatalf("reattach did not report the bound profile: %v\n%s", err, out.String())
			}
		})
	}
}

func TestAxiRunRefusesARelativeClaudeConfigDir(t *testing.T) {
	newAxiTimeoutFixture(t, axiTimeoutOpts{})
	t.Setenv(runenv.ClaudeConfigDirEnvVar, "~/.claude1")
	cmd := newAxiRunCmd()
	cmd.SetArgs(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), "must be an absolute path") {
		t.Fatalf("relative CLAUDE_CONFIG_DIR = %v\n%s", err, out.String())
	}
}

// absTestPath makes a POSIX-style path absolute on every platform: Windows
// needs a volume name, and forward slashes keep TOON output unescaped.
func absTestPath(path string) string {
	return filepath.VolumeName(os.TempDir()) + path
}
