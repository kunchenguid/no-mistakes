package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRerunSendsOnlyCleanCallerHead(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		name := "clean"
		if dirty {
			name = "dirty"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := paths.WithRoot(makeSocketSafeTempDir(t))
			t.Setenv("NM_HOME", p.Root())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			cliGit(t, dir, "init", "-b", "main")
			cliGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
			chdir(t, dir)
			root, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			repo, err := d.InsertRepo(root, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			wantHead := cliGit(t, dir, "rev-parse", "HEAD")
			if dirty {
				if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("local edits"), 0o644); err != nil {
					t.Fatal(err)
				}
				wantHead = ""
			}
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			requests := make(chan map[string]string, 1)
			srv.Handle(ipc.MethodRerun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params map[string]string
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				requests <- params
				return &ipc.RerunResult{RunID: "rerun-1"}, nil
			})
			done := make(chan error, 1)
			go func() { done <- srv.Serve(p.Socket()) }()
			t.Cleanup(func() { srv.Close(); <-done })
			deadline := time.Now().Add(3 * time.Second)
			for {
				if alive, _ := daemon.IsRunning(p); alive {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("test IPC server did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			cmd := newRerunCmd()
			cmd.SetArgs([]string{"--intent", "keep the caller's changes"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			params := <-requests
			if params["caller_head_sha"] != wantHead || params["repo_id"] != repo.ID || params["intent"] != "keep the caller's changes" {
				t.Fatalf("rerun request = %v, want caller head %q and original repo/intent", params, wantHead)
			}
			if !strings.Contains(out.String(), "Rerun started") {
				t.Fatalf("missing rerun confirmation: %s", out.String())
			}
		})
	}
}
