package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSelectedRunRefusesOldDaemonBeforeLaunch(t *testing.T) {
	launched := olderDaemonFixture(t, nil)
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--agent", "codex", "--intent", "verify selection"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "cannot honor --agent") {
		t.Fatalf("old daemon accepted selection: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("old daemon took custody: %v", *launched)
	}
}

func TestRequiredRunAgentOmittedFlagRefusesOldDaemonBeforeLaunch(t *testing.T) {
	t.Setenv("NM_REQUIRED_RUN_AGENT", "codex")
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--intent", "verify selection"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "cannot honor --agent") {
		t.Fatalf("required selection was omitted: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("old daemon took custody: %v", *launched)
	}
}

func TestRequiredRerunAgentOmittedFlagRefusesOldDaemonBeforeLaunch(t *testing.T) {
	t.Setenv("NM_REQUIRED_RUN_AGENT", "codex")
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	cmd := newRerunCmd()
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot honor --agent") {
		t.Fatalf("required rerun selection was omitted: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("old daemon took custody: %v", *launched)
	}
}

func TestSelectedRunRefusesMismatchedActiveRunBeforeLaunch(t *testing.T) {
	var head string
	launched := olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodProbeRunAgent, func(context.Context, json.RawMessage) (interface{}, error) {
			return ipc.ProbeRunAgentResult{OK: true}, nil
		})
		srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
			return ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: "existing-run", Status: types.RunRunning, HeadSHA: head}}, nil
		})
	})
	var err error
	head, err = git.Run(context.Background(), ".", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--agent", "codex", "--intent", "verify selection"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "different agent selection") {
		t.Fatalf("mismatched active run accepted: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("active run was replaced: %v", *launched)
	}
}

func TestRequiredRunAgentRefusesMismatchedActiveRunBeforeLaunch(t *testing.T) {
	t.Setenv("NM_REQUIRED_RUN_AGENT", "codex")
	var head string
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodProbeRunAgent, func(context.Context, json.RawMessage) (interface{}, error) {
			return ipc.ProbeRunAgentResult{OK: true}, nil
		})
		srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
			return ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: "existing-run", Status: types.RunRunning, HeadSHA: head}}, nil
		})
	})
	var err error
	head, err = git.Run(context.Background(), ".", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--intent", "verify selection"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "different agent selection") {
		t.Fatalf("mismatched active run accepted: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("active run was replaced: %v", *launched)
	}
}

func TestRunAgentFlagAndPushOption(t *testing.T) {
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--agent", "codex"}); err != nil {
		t.Fatal(err)
	}
	if got, err := runAgentFromFlags(cmd); err != nil || got != agentcfg.RunAgentCodex {
		t.Fatalf("selection = %q, %v", got, err)
	}
	selection, err := parseRunAgentPushOptions(formatRunAgentPushOptions(agentcfg.RunAgentCodex))
	if err != nil || selection != agentcfg.RunAgentCodex {
		t.Fatalf("push option = %q, %v", selection, err)
	}
	if _, err := parseRunAgentPushOptions([]string{"no-mistakes.run-agent=codex", "no-mistakes.run-agent=codex"}); err == nil {
		t.Fatal("duplicate selection accepted")
	}
}

func TestRunAgentRejectsUnsupportedAndPiSelection(t *testing.T) {
	for _, flags := range [][]string{{"--agent", "claude"}, {"--agent", "codex", "--model", "provider/model"}, {"--agent", "codex", "--effort", "high"}} {
		cmd := newAxiRunCmd()
		if err := cmd.ParseFlags(flags); err != nil {
			t.Fatal(err)
		}
		if _, err := runAgentFromFlags(cmd); err == nil {
			t.Fatalf("accepted %v", flags)
		}
	}
}

func TestRequiredRunAgentFlagResolution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required string
		set      bool
		flags    []string
		want     agentcfg.RunAgent
		wantErr  string
	}{
		{name: "ordinary unset", want: ""},
		{name: "required omission", required: "codex", set: true, want: agentcfg.RunAgentCodex},
		{name: "matching explicit", required: "codex", set: true, flags: []string{"--agent", "codex"}, want: agentcfg.RunAgentCodex},
		{name: "required with Pi model", required: "codex", set: true, flags: []string{"--model", "provider/model"}, wantErr: "--agent cannot be combined with Pi"},
		{name: "conflicting explicit", required: "codex", set: true, flags: []string{"--agent", "claude"}, wantErr: "conflicts with NM_REQUIRED_RUN_AGENT"},
		{name: "explicit clear", required: "codex", set: true, flags: []string{"--agent", ""}, wantErr: "--agent requires codex"},
		{name: "empty requirement", set: true, wantErr: "NM_REQUIRED_RUN_AGENT requires codex"},
		{name: "invalid requirement", required: "claude", set: true, wantErr: "NM_REQUIRED_RUN_AGENT requires codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("NM_REQUIRED_RUN_AGENT", tc.required)
			} else {
				t.Setenv("NM_REQUIRED_RUN_AGENT", "")
				if err := os.Unsetenv("NM_REQUIRED_RUN_AGENT"); err != nil {
					t.Fatal(err)
				}
			}
			cmd := newAxiRunCmd()
			if err := cmd.ParseFlags(tc.flags); err != nil {
				t.Fatal(err)
			}
			got, err := runAgentFromFlags(cmd)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("selection = %q, error = %v; want %q", got, err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("selection = %q, error = %v; want %q", got, err, tc.want)
			}
		})
	}
}
