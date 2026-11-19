package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTriggerRunRefusesAnAdoptedRunBoundToAnotherClaudeProfile(t *testing.T) {
	callerProfile := absTestPath("/caller/.claude1")
	otherProfile := absTestPath("/caller/.claude2")
	for _, tc := range []struct {
		name, caller, bound string
		refused             bool
	}{
		{name: "same profile", caller: callerProfile, bound: callerProfile},
		{name: "other profile", caller: callerProfile, bound: otherProfile, refused: true},
		{name: "run on the daemon's default", caller: callerProfile, bound: "", refused: true},
		{name: "unset caller", caller: "", bound: otherProfile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := paths.WithRoot(makeSocketSafeTempDir(t))
			t.Setenv("NM_HOME", p.Root())
			t.Setenv(runenv.ClaudeConfigDirEnvVar, tc.caller)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			cliGit(t, dir, "init", "-b", "main")
			cliGit(t, dir, "config", "--local", "user.name", "Test")
			cliGit(t, dir, "config", "--local", "user.email", "test@example.com")
			cliGit(t, dir, "commit", "--allow-empty", "-m", "initial")
			chdir(t, dir)
			root, err := git.FindGitRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := d.InsertRepo(root, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			head := cliGit(t, dir, "rev-parse", "HEAD")
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, dir, "clone", "--bare", dir, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)

			srv := ipc.NewServer()
			serveTranscriptHomesProbe(srv)
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok", ClaudeConfigDir: true}, nil
			})
			srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetRunsResult{}, nil
			})
			srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
					ID: "run-concurrent", RepoID: repo.ID, Branch: "main", HeadSHA: head,
					Status: types.RunRunning, ClaudeConfigDir: tc.bound,
				}}, nil
			})
			startIPCServer(t, srv, p.Socket())
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			runID, err := triggerRun(ctx, env, "main", nil, "goal", "", false, "", nil)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "run-concurrent uses Claude profile") {
					t.Fatalf("adopted a run bound to another profile: id=%q err=%v", runID, err)
				}
				return
			}
			if err != nil || runID != "run-concurrent" {
				t.Fatalf("run = %q, err = %v; want run-concurrent adopted", runID, err)
			}
		})
	}
}
