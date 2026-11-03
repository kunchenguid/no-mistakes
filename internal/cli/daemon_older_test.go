package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func legacyLifecycleServer(t *testing.T, pid int, configure func(*ipc.Server)) *paths.Paths {
	t.Helper()
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if pid > 0 {
		if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprint(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	if configure != nil {
		configure(srv)
	}
	startIPCServer(t, srv, p.Socket())
	return p
}

func TestOlderDaemonLifecycleCommands(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			legacyLifecycleServer(t, os.Getpid(), nil)
			var calls []string
			oldStop, oldStart, oldGuarded := daemonStopFn, daemonStartFn, daemonStartGuardedFn
			daemonStopFn = func(*paths.Paths) error { calls = append(calls, "stop"); return nil }
			daemonStartFn = func(*paths.Paths) error { calls = append(calls, "start"); return nil }
			daemonStartGuardedFn = func(*paths.Paths, func() error) error {
				t.Fatal("older daemon must be stopped before reinstalling")
				return nil
			}
			t.Cleanup(func() { daemonStopFn, daemonStartFn, daemonStartGuardedFn = oldStop, oldStart, oldGuarded })
			out, err := executeCmd("daemon", action)
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", action, err, out)
			}
			want := []string{"stop", "start"}
			if action == "stop" {
				want = []string{"stop"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
		})
	}
}

func TestOlderDaemonLifecycleKeepsActiveRunGuard(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			p := legacyLifecycleServer(t, os.Getpid(), nil)
			createLifecycleGuardRuns(t, p)
			oldStop, oldStart := daemonStopFn, daemonStartFn
			daemonStopFn = func(*paths.Paths) error { t.Fatal("active runs must prevent stop"); return nil }
			daemonStartFn = func(*paths.Paths) error { t.Fatal("active runs must prevent start"); return nil }
			t.Cleanup(func() { daemonStopFn, daemonStartFn = oldStop, oldStart })
			out, err := executeCmd("daemon", action)
			if err == nil || !strings.Contains(out+err.Error(), "active pipeline runs") {
				t.Fatalf("expected active-run refusal: %v\n%s", err, out)
			}
		})
	}
}

func TestOlderDaemonStartForceReplacesDespiteActiveRuns(t *testing.T) {
	p := legacyLifecycleServer(t, os.Getpid(), nil)
	createLifecycleGuardRuns(t, p)
	var calls []string
	oldStop, oldStart := daemonStopFn, daemonStartFn
	daemonStopFn = func(*paths.Paths) error { calls = append(calls, "stop"); return nil }
	daemonStartFn = func(*paths.Paths) error { calls = append(calls, "start"); return nil }
	t.Cleanup(func() { daemonStopFn, daemonStartFn = oldStop, oldStart })
	out, err := executeCmd("daemon", "start", "--force")
	if err != nil {
		t.Fatalf("forced start failed: %v\n%s", err, out)
	}
	if want := []string{"stop", "start"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestGateControlOlderDaemonFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pid       int
		configure func(*ipc.Server)
		args      []string
		want      string
	}{
		{name: "update", pid: os.Getpid(), args: []string{"update"}},
		{name: "daemon ancestry", pid: os.Getppid(), args: []string{"daemon", "restart"}, want: "nested_gate_context"},
		{name: "uninstall", pid: os.Getpid(), args: []string{"daemon", "uninstall"}},
		{name: "missing PID", args: []string{"daemon", "stop"}, want: "read daemon PID"},
		{name: "other mutations", pid: os.Getpid(), args: []string{"axi", "run"}, want: "method not found"},
		{name: "answer refuses", args: []string{"daemon", "start"}, want: "nested_gate_context", configure: func(s *ipc.Server) {
			s.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GateContextResult{Nested: true}, nil
			})
		}},
		{name: "other RPC error", pid: os.Getpid(), args: []string{"daemon", "stop"}, want: "broken classifier", configure: func(s *ipc.Server) {
			s.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
				return nil, fmt.Errorf("broken classifier")
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyLifecycleServer(t, tc.pid, tc.configure)
			root := newRootCmd()
			cmd, _, err := root.Find(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd.SetContext(context.Background())
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err = guardGateControl(cmd)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("guard refused: %v\n%s", err, out.String())
				}
				if !strings.Contains(out.String(), "predates gate_context") {
					t.Fatal("compatibility fallback must explain the older daemon")
				}
			} else if err == nil || !strings.Contains(out.String()+err.Error(), tc.want) {
				t.Fatalf("want %q refusal, got %v\n%s", tc.want, err, out.String())
			}
		})
	}
}
