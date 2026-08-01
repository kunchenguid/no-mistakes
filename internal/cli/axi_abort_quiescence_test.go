package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newAbortQuiescenceFixture drives the public abort surfaces against a fake
// daemon that scripts the exact run-state reads: cancellation is always
// accepted, but what the post-cancel terminal wait observes is controlled per
// test. The registered repo, worktree, socket, and NM_HOME are all real, so
// `axi abort` runs end to end through the CLI.
func newAbortQuiescenceFixture(t *testing.T, getRun func(call int) (*ipc.RunInfo, error)) {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)

	root := t.TempDir()
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	cliGit(t, local, "checkout", "-b", "feature/abort")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepo(registeredRoot, filepath.Join(root, "remote.git"), "main"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: false}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
			ID: "run-quiesce", Branch: "feature/abort", Status: types.RunRunning,
		}}, nil
	})
	srv.Handle(ipc.MethodCancelRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.CancelRunResult{OK: true}, nil
	})
	calls := 0
	srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
		calls++
		run, err := getRun(calls)
		if err != nil {
			return nil, err
		}
		return &ipc.GetRunResult{Run: run}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client, dialErr := ipc.Dial(p.Socket()); dialErr == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	previousTimeout := abortStateWaitTimeout
	abortStateWaitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { abortStateWaitTimeout = previousTimeout })

	chdir(t, local)
}

func runningRunForever(int) (*ipc.RunInfo, error) {
	return &ipc.RunInfo{ID: "run-quiesce", Branch: "feature/abort", Status: types.RunRunning}, nil
}

func assertUnconfirmedAbortOutput(t *testing.T, surface, out string) {
	t.Helper()
	for _, want := range []string{"cancellation was requested", "terminal quiescence", "unconfirmed", "run-quiesce"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s unconfirmed output missing %q:\n%s", surface, want, out)
		}
	}
	for _, forbidden := range []string{"aborted: true", "state: user_owned", "recover_custody", "branch_sync:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s presented %q while the run was not confirmed terminal:\n%s", surface, forbidden, out)
		}
	}
}

// TestAxiAbortRefusesSuccessWhileTerminalQuiescenceUnconfirmed pins the
// accepted abort contract on the ordinary surface: when terminalization is
// delayed past the bounded wait, or the status read fails, abort must exit
// nonzero, state the unconfirmed condition explicitly, include the last
// structured run state when one is available, and never present a completed
// abort or authoritative ownership guidance.
func TestAxiAbortRefusesSuccessWhileTerminalQuiescenceUnconfirmed(t *testing.T) {
	t.Run("terminalization delayed past bounded wait", func(t *testing.T) {
		newAbortQuiescenceFixture(t, runningRunForever)
		out, err := executeCmd("axi", "abort")
		if err == nil {
			t.Fatalf("abort with delayed terminalization must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort", out)
		if !strings.Contains(out, "status: running") {
			t.Errorf("unconfirmed abort omitted the last structured run state:\n%s", out)
		}
	})
	t.Run("status read failure", func(t *testing.T) {
		newAbortQuiescenceFixture(t, func(int) (*ipc.RunInfo, error) {
			return nil, errors.New("scripted status read failure")
		})
		out, err := executeCmd("axi", "abort")
		if err == nil {
			t.Fatalf("abort with failing status reads must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort", out)
	})
}

// TestAxiAbortByRunIDRefusesSuccessWhileTerminalQuiescenceUnconfirmed pins the
// same contract on explicit --run cancellation, which previously reported
// success without any terminal wait at all, and proves the confirmed path
// still reports the completed abort with the observed terminal state.
func TestAxiAbortByRunIDRefusesSuccessWhileTerminalQuiescenceUnconfirmed(t *testing.T) {
	t.Run("terminalization delayed past bounded wait", func(t *testing.T) {
		newAbortQuiescenceFixture(t, runningRunForever)
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		if err == nil {
			t.Fatalf("abort --run with delayed terminalization must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
		if !strings.Contains(out, "status: running") {
			t.Errorf("unconfirmed abort --run omitted the last structured run state:\n%s", out)
		}
	})
	t.Run("status read failure", func(t *testing.T) {
		newAbortQuiescenceFixture(t, func(int) (*ipc.RunInfo, error) {
			return nil, errors.New("scripted status read failure")
		})
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		if err == nil {
			t.Fatalf("abort --run with failing status reads must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
	})
	t.Run("confirmed terminal reports completion", func(t *testing.T) {
		newAbortQuiescenceFixture(t, func(int) (*ipc.RunInfo, error) {
			return &ipc.RunInfo{ID: "run-quiesce", Branch: "feature/abort", Status: types.RunCancelled}, nil
		})
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		if err != nil {
			t.Fatalf("confirmed abort --run: %v\n%s", err, out)
		}
		for _, want := range []string{"aborted: true", "run-quiesce", "cancelled"} {
			if !strings.Contains(out, want) {
				t.Errorf("confirmed abort --run missing %q:\n%s", want, out)
			}
		}
	})
}
