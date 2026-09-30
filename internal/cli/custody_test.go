package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCustodyCommandsFenceRequestsThroughDaemon(t *testing.T) {
	for _, action := range []string{"release", "reconcile", "rebind"} {
		t.Run(action, func(t *testing.T) {
			f := newCLISyncFixture(t)
			original, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			database, err := os.ReadFile(original.DB())
			if err != nil {
				t.Fatal(err)
			}
			// Darwin Unix-domain socket paths must stay below 104 bytes.
			t.Setenv("NM_HOME", makeSocketSafeTempDir(t))
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.DB(), database, 0600); err != nil {
				t.Fatal(err)
			}
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) { return &ipc.GateContextResult{}, nil })
			called := false
			srv.Handle(ipc.MethodCustodyOperation, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var request ipc.CustodyOperationParams
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				called = true
				if request.RunID != f.runID || request.HeadSHA != f.old || request.Action != action || canonicalTestPath(request.WorkDir) != canonicalTestPath(f.local) {
					t.Errorf("wrong generation: %+v", request)
				}
				if action == "rebind" && request.PublicationBranch != "existing" {
					t.Errorf("wrong destination: %+v", request)
				}
				return nil, errors.New("owned head cannot be archived")
			})
			startIPCServer(t, srv, p.Socket())
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			args := []string{"axi", "custody", action, "--run", f.runID}
			if action == "rebind" {
				args = []string{"publication", "rebind", "--run", f.runID, "--branch", "existing"}
			}
			root.SetArgs(args)
			err = root.Execute()
			var exit *exitError
			if !errors.As(err, &exit) || exit.code != 1 || !called || !strings.Contains(out.String(), "owned head cannot be archived") {
				t.Fatalf("err=%v called=%v output=%s", err, called, out.String())
			}
		})
	}
}

func TestCustodyCommandsRequireExplicitGenerationAndDestination(t *testing.T) {
	for _, args := range [][]string{{"custody", "release"}, {"axi", "custody", "reconcile"}, {"publication", "rebind", "--run", "run"}} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != 2 || !strings.Contains(out.String(), "is required") {
			t.Fatalf("%v: err=%v output=%s", args, err, out.String())
		}
	}
}

func canonicalTestPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func TestCustodyStatusSeparatesPublicationAndCustodyIdentity(t *testing.T) {
	target := "existing"
	rv := runViewFromIPC(&ipc.RunInfo{ID: "selected", Branch: "validation", PublicationBranch: &target})
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	emitDoc(cmd, runObjectField(rv))
	if !strings.Contains(out.String(), "branch: validation") || !strings.Contains(out.String(), "publication_branch: existing") {
		t.Fatalf("ambiguous ownership: %s", out.String())
	}
}
