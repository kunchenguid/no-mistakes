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

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
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
			root, err := git.FindGitRoot(dir)
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
			commitDuringWait := make(chan struct{}, 1)
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetRunsResult{}, nil
			})
			srv.Handle(ipc.MethodGetActiveRun, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
				select {
				case <-commitDuringWait:
					if _, err := git.Run(ctx, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "concurrent local commit"); err != nil {
						return nil, err
					}
				default:
				}
				return &ipc.GetActiveRunResult{}, nil
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
			if _, present := params["caller_head_sha"]; dirty && present {
				t.Fatal("dirty caller must omit caller_head_sha from the wire request")
			}
			t.Logf("CLI output: %sIPC request: %v", out.String(), params)
			if !dirty {
				// Exercise AXI's real no-op Git push and rerun fallback too.
				gateDir := p.RepoDir(repo.ID)
				cliGit(t, dir, "clone", "--bare", dir, gateDir)
				cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
				cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				runID, err := triggerRun(ctx, env, "main", wantHead, nil, "keep the caller's changes", "")
				if err != nil || runID != "rerun-1" {
					t.Fatalf("no-op push fallback: run=%s err=%v", runID, err)
				}
				params = <-requests
				if params["caller_head_sha"] != wantHead {
					t.Fatalf("AXI omitted known head: %v", params)
				}
				t.Logf("AXI no-op push fallback IPC request: %v; run_id=%s", params, runID)

				for _, phase := range []string{"during_wait", "before_push"} {
					t.Run("commit_"+phase, func(t *testing.T) {
						// Change HEAD on either side of the push without changing
						// the startup snapshot passed to triggerRun.
						if phase == "during_wait" {
							commitDuringWait <- struct{}{}
						} else {
							cliGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "commit before push")
						}
						ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						if _, err := triggerRun(ctx, env, "main", wantHead, nil, "keep the caller's changes", ""); err != nil {
							t.Fatal(err)
						}
						params := <-requests
						currentHead := cliGit(t, dir, "rev-parse", "HEAD")
						if currentHead == wantHead {
							t.Fatal("fixture did not advance the caller HEAD")
						}
						if params["caller_head_sha"] != currentHead {
							t.Errorf("fallback sent stale caller head %s, want current clean head %s", params["caller_head_sha"], currentHead)
						}
						gateHead := cliGit(t, gateDir, "rev-parse", "refs/heads/main")
						wantGate := wantHead
						if phase == "before_push" {
							wantGate = currentHead
						}
						if gateHead != wantGate {
							t.Fatalf("gate head = %s, want %s", gateHead, wantGate)
						}
						t.Logf("startup=%s current=%s gate=%s request=%s", wantHead, currentHead, gateHead, params["caller_head_sha"])
					})
				}
			}
		})
	}
}

func TestRerunRefusesDifferentCleanHeadCLI(t *testing.T) {
	for _, selection := range []string{"gate", "preserved"} {
		t.Run(selection, func(t *testing.T) {
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
			t.Cleanup(func() { d.Close() })
			startTestDaemon(t, p, d)
			cliGit(t, dir, "init", "-b", "main")
			cliGit(t, dir, "config", "user.name", "Test")
			cliGit(t, dir, "config", "user.email", "test@example.com")
			cliGit(t, dir, "commit", "--allow-empty", "-m", "submitted")
			chdir(t, dir)
			root, err := git.FindGitRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := d.InsertRepo(root, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, dir, "clone", "--bare", dir, gateDir)
			submitted := cliGit(t, dir, "rev-parse", "HEAD")
			prior, err := d.InsertRun(repo.ID, "main", submitted, submitted)
			if err != nil {
				t.Fatal(err)
			}
			selected := submitted
			if selection == "preserved" {
				cliGit(t, dir, "commit", "--allow-empty", "-m", "pipeline rewrite")
				selected = cliGit(t, dir, "rev-parse", "HEAD")
				cliGit(t, dir, "push", gateDir, selected+":"+custody.RecoveryRef(prior.ID))
			}
			if err := d.UpdateRunStatusWithVerifiedHead(prior.ID, types.RunFailed, selected); err != nil {
				t.Fatal(err)
			}
			cliGit(t, dir, "commit", "--allow-empty", "--amend", "-m", "corrected local head")
			callerHead := cliGit(t, dir, "rev-parse", "HEAD")
			callerRefs := cliGit(t, dir, "show-ref")
			gateRefs := cliGit(t, gateDir, "show-ref")
			if status := cliGit(t, dir, "status", "--porcelain"); status != "" {
				t.Fatalf("caller is dirty: %s", status)
			}
			var output bytes.Buffer
			cmd := newRerunCmd()
			cmd.SetArgs([]string{})
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			err = cmd.Execute()
			if err == nil {
				t.Fatalf("CLI accepted differing clean head: %s", output.String())
			}
			for _, want := range []string{"refusing rerun", selected, callerHead, "no-mistakes axi status", "no-mistakes axi run"} {
				if !strings.Contains(err.Error(), want) || !strings.Contains(output.String(), want) {
					t.Fatalf("CLI refusal missing %q: err=%v output=%s", want, err, output.String())
				}
			}
			if strings.Contains(output.String(), "Rerun started") {
				t.Fatalf("CLI falsely confirmed a rerun: %s", output.String())
			}
			t.Logf("CLI output:\n%s", output.String())
			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil || len(runs) != 1 || runs[0].ID != prior.ID || runs[0].Status != types.RunFailed {
				t.Fatalf("refusal changed run history: runs=%+v err=%v", runs, err)
			}
			if got := cliGit(t, dir, "show-ref"); got != callerRefs {
				t.Fatalf("caller refs changed: before=%s after=%s", callerRefs, got)
			}
			if got := cliGit(t, gateDir, "show-ref"); got != gateRefs {
				t.Fatalf("gate refs changed: before=%s after=%s", gateRefs, got)
			}
			t.Logf("persisted runs=%d; prior status=%s; caller and gate refs unchanged\ncaller refs:\n%s\ngate refs:\n%s", len(runs), runs[0].Status, callerRefs, gateRefs)
		})
	}
}
