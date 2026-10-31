package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/spf13/cobra"
)

const restartGuidance = "no-mistakes daemon restart"

func currentDaemonHealth(srv *ipc.Server) {
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok", ClaudeConfigDir: true}, nil
	})
}

func TestAxiRunRefusesAnOlderDaemonOnlyWhenAProfileIsSet(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		current       bool
		refused       bool
	}{
		{name: "older daemon with a profile", profile: absTestPath("/caller/.claude1"), refused: true},
		{name: "older daemon without a profile"},
		{name: "current daemon with a profile", profile: absTestPath("/caller/.claude1"), current: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configure []func(*ipc.Server)
			if tc.current {
				configure = append(configure, currentDaemonHealth)
			}
			launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, configure...)
			t.Setenv(runenv.ClaudeConfigDirEnvVar, tc.profile)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			_ = runAxiRunWithLaunchProof(cmd, false, nil, "goal", "", false, "", "", defaultAxiWait)
			if got := strings.Contains(out.String(), restartGuidance); got != tc.refused {
				t.Fatalf("restart guidance present = %v, want %v:\n%s", got, tc.refused, out.String())
			}
			if tc.refused && len(*launched) != 0 {
				t.Fatalf("a run was launched on a daemon that ignores the profile: %v", *launched)
			}
		})
	}
}

func TestRerunRefusesAnOlderDaemonOnlyWhenAProfileIsSet(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		current       bool
		refused       bool
	}{
		{name: "older daemon with a profile", profile: absTestPath("/caller/.claude1"), refused: true},
		{name: "older daemon without a profile"},
		{name: "current daemon with a profile", profile: absTestPath("/caller/.claude1"), current: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configure := []func(*ipc.Server){}
			if tc.current {
				configure = append(configure, currentDaemonHealth)
			}
			launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, configure...)
			t.Setenv(runenv.ClaudeConfigDirEnvVar, tc.profile)
			cmd := newRerunCmd()
			cmd.SetArgs([]string{})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			if got := err != nil && strings.Contains(err.Error(), restartGuidance); got != tc.refused {
				t.Fatalf("restart guidance in error = %v, want %v: %v\n%s", got, tc.refused, err, out.String())
			}
			if tc.refused && len(*launched) != 0 {
				t.Fatalf("a rerun was launched on a daemon that ignores the profile: %v", *launched)
			}
		})
	}
}

func TestPushHookRefusesAnOlderDaemonOnlyWhenAProfileIsSet(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		current       bool
		refused       bool
	}{
		{name: "older daemon with a profile", profile: absTestPath("/caller/.claude1"), refused: true},
		{name: "older daemon without a profile"},
		{name: "current daemon with a profile", profile: absTestPath("/caller/.claude1"), current: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := paths.WithRoot(makeSocketSafeTempDir(t))
			t.Setenv("NM_HOME", p.Root())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(runenv.ClaudeConfigDirEnvVar, tc.profile)
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok", ClaudeConfigDir: tc.current}, nil
			})
			var pushed []ipc.PushReceivedParams
			srv.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.PushReceivedParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				pushed = append(pushed, params)
				return &ipc.PushReceivedResult{}, nil
			})
			startIPCServer(t, srv, p.Socket())

			out, err := executeCmd("daemon", "notify-push", "--gate", p.RepoDir("repo"), "--ref", "refs/heads/x", "--old", "0", "--new", "1")
			refused := err != nil && strings.Contains(err.Error(), restartGuidance)
			if refused != tc.refused {
				t.Fatalf("restart guidance in error = %v, want %v: %v\n%s", refused, tc.refused, err, out)
			}
			if tc.refused && len(pushed) != 0 {
				t.Fatalf("a push was announced to a daemon that ignores the profile: %v", pushed)
			}
			if !tc.refused && (err != nil || len(pushed) != 1 || pushed[0].ClaudeConfigDir != tc.profile) {
				t.Fatalf("push = %v, err = %v; want one announcement carrying %q", pushed, err, tc.profile)
			}
		})
	}
}
