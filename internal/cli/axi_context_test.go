package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiContextUnavailableNeverInitializesState(t *testing.T) {
	root := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", root)
	t.Setenv("NO_MISTAKES_GATE", "1")
	p := paths.WithRoot(root)
	// A stale PID must remain untouched: this command is not lifecycle repair.
	if err := os.WriteFile(p.PIDFile(), []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "context", "--json")
	assertContextUnavailable(t, out, err)
	if got, err := os.ReadFile(p.PIDFile()); err != nil || string(got) != "123456789" {
		t.Fatalf("context modified stale daemon state: %q, %v", got, err)
	}
	if _, err := os.Stat(p.DB()); !os.IsNotExist(err) {
		t.Fatalf("context created a database: %v", err)
	}
}

func TestAxiContextDaemonCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*ipc.Server)
	}{
		{name: "daemon without gate RPC"},
		{name: "old classification has no provenance", configure: func(s *ipc.Server) {
			s.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GateContextResult{Nested: true, ManagedGit: true, AgentDescendant: true, RunID: "must-not-leak", Phase: types.StepName("gate.test.local-ci")}, nil
			})
		}},
		{name: "classifier failure", configure: func(s *ipc.Server) {
			s.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
				return nil, fmt.Errorf("private failure details")
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyLifecycleServer(t, os.Getpid(), tc.configure)
			out, err := executeCmd("axi", "context", "--json")
			assertContextUnavailable(t, out, err)
			if strings.Contains(out, "must-not-leak") || strings.Contains(out, "private failure details") {
				t.Fatalf("unavailable context leaked evidence: %s", out)
			}
		})
	}
}

func TestAxiContextRefusesCallerSelectedRunAndMutationFlags(t *testing.T) {
	for _, args := range [][]string{{"--run", "chosen-run"}, {"--yes"}, {"--action", "approve"}, {"chosen-run"}} {
		if out, err := executeCmd(append([]string{"axi", "context"}, args...)...); err == nil {
			t.Fatalf("context accepted %v: %s", args, out)
		}
	}
}

func assertContextUnavailable(t *testing.T, out string, err error) {
	t.Helper()
	var got map[string]any
	if err == nil {
		t.Fatal("unavailable context exited successfully")
	}
	if decodeErr := json.Unmarshal([]byte(out), &got); decodeErr != nil {
		t.Fatalf("invalid JSON %q: %v", out, decodeErr)
	}
	if got["schema_version"] != float64(1) || got["available"] != false || got["reason"] == "" || len(got) != 3 {
		t.Fatalf("unexpected unavailable response: %s", out)
	}
}
