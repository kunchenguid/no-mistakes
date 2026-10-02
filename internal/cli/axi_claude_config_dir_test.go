package cli

import (
	"bytes"
	"context"
	"errors"
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
	}{
		{"same profile", "/caller/.claude1", "/caller/.claude1", false},
		{"same profile as the push hook bound it", "/caller/.claude1/", "/caller/.claude1/", false},
		{"unset requests no profile", "/caller/.claude1", "", false},
		{"other profile", "/caller/.claude1", "/caller/.claude2", true},
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
