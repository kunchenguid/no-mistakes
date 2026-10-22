package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestRunAgentFlagAndPushProtocol(t *testing.T) {
	for _, name := range []string{"claude", "codex", "pi"} {
		cmd := newAxiRunCmd()
		if err := cmd.ParseFlags([]string{"--agent", name}); err != nil {
			t.Fatal(err)
		}
		decoded, err := parseRunAgentPushOptions(formatRunAgentPushOptions(types.AgentName(name)))
		if err != nil || string(decoded) != name {
			t.Fatalf("push roundtrip: %s %v", decoded, err)
		}
	}
	for _, args := range [][]string{{"--agent="}, {"--agent", "auto"}, {"--agent", "/tmp/agent"}, {"--agent", "acp:untrusted"}, {"--agent", "claude", "--agent", "codex"}} {
		if err := newAxiRunCmd().ParseFlags(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, options := range [][]string{{"no-mistakes.agent="}, {"no-mistakes.agent=codex", "no-mistakes.agent=claude"}, {"no-mistakes.agent=auto"}} {
		if _, err := parseRunAgentPushOptions(options); err == nil {
			t.Fatalf("accepted %v", options)
		}
	}
}

func TestRunAgentRefusesOlderDaemonBeforeLaunching(t *testing.T) {
	launched := olderDaemonFixture(t, nil)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	err := runAxiRunWithLaunchProof(cmd, false, nil, "validate with caller", "", false, "", "", defaultAxiWait, types.AgentCodex)
	if err == nil || !strings.Contains(out.String(), "daemon cannot honor --agent") {
		t.Fatalf("old daemon not refused: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("started validation on incompatible daemon: %v", *launched)
	}
}
