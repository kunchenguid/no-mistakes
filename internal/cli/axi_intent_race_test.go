package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiRunIntentLateRunRequiresMatchingIntent(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		terminal     bool
		wantPass     bool
	}{
		{name: "hook run without intent"},
		{name: "hook run with different intent", stored: "original goal"},
		{name: "new run with matching intent", stored: "requested goal", wantPass: true},
		{name: "hook run failed before active lookup", terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var database *db.DB
			var repo *db.Repo
			var head string
			ready := make(chan struct{})
			var recorded atomic.Pointer[db.Run]
			var lookups, drives, subscriptions atomic.Int32
			info := func(run *db.Run) *ipc.RunInfo {
				return &ipc.RunInfo{ID: run.ID, RepoID: run.RepoID, Branch: run.Branch, HeadSHA: run.HeadSHA, Status: run.Status}
			}
			launched := olderDaemonFixture(t, nil, func(srv *ipc.Server) {
				srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
					<-ready
					// The initial lookup sees no run. Register the delayed hook's
					// run only after the ownership check and up-to-date Git push.
					if lookups.Add(1) == 1 {
						return &ipc.GetActiveRunResult{}, nil
					}
					var intent *db.RunIntent
					if tc.stored != "" {
						intent = &db.RunIntent{Summary: tc.stored, Source: db.RunIntentSourceAgent}
					}
					run, err := database.InsertRunWithIntent(repo.ID, "feature/omit", head, head, intent, "")
					if err != nil {
						return nil, err
					}
					status := types.RunRunning
					if tc.terminal {
						status = types.RunFailed
					}
					if err := database.UpdateRunStatus(run.ID, status); err != nil {
						return nil, err
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						return nil, err
					}
					recorded.Store(run)
					if tc.terminal {
						return &ipc.GetActiveRunResult{}, nil
					}
					return &ipc.GetActiveRunResult{Run: info(run)}, nil
				})
				srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
					<-ready
					if run := recorded.Load(); run != nil {
						return &ipc.GetRunsResult{Runs: []ipc.RunInfo{*info(run)}}, nil
					}
					return &ipc.GetRunsResult{}, nil
				})
				srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
					drives.Add(1)
					run := info(recorded.Load())
					run.Status = types.RunCompleted
					return &ipc.GetRunResult{Run: run}, nil
				})
				srv.HandleStream(ipc.MethodSubscribe, func(ctx context.Context, raw json.RawMessage) (ipc.StreamFunc, error) {
					subscriptions.Add(1)
					return hangSubscribe(ctx, raw)
				})
			})
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			database, err = db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, err = findRepo(database)
			if err != nil {
				t.Fatal(err)
			}
			local, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			head = cliGit(t, local, "rev-parse", "HEAD")
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, local, "clone", "--bare", local, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
			refs := cliGit(t, gateDir, "show-ref")
			close(ready)

			cmd := newAxiRunCmd()
			cmd.SetArgs([]string{"--intent", " \trequested goal\n"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err = cmd.Execute()
			run := recorded.Load()
			if run == nil || lookups.Load() < 2 {
				t.Fatalf("did not reach delayed hook lookup: %v\n%s", err, out.String())
			}
			if tc.wantPass {
				if err != nil || !strings.Contains(out.String(), "outcome: passed") || drives.Load() == 0 {
					t.Fatalf("matching intent was not driven: %v\n%s", err, out.String())
				}
			} else {
				if err == nil || !strings.Contains(out.String(), run.ID) || !strings.Contains(out.String(), "intent") || !strings.Contains(out.String(), "refusing") {
					t.Fatalf("late run silently dropped explicit intent: %v\n%s", err, out.String())
				}
				if drives.Load() != 0 || subscriptions.Load() != 0 {
					t.Fatal("drove a late run without matching intent")
				}
			}
			after, err := database.GetRun(run.ID)
			if err != nil || !reflect.DeepEqual(run, after) {
				t.Fatalf("late selection mutated persisted run: err=%v before=%+v after=%+v", err, run, after)
			}
			if len(*launched) != 0 || cliGit(t, gateDir, "show-ref") != refs || cliGit(t, local, "rev-parse", "HEAD") != head {
				t.Fatal("late selection launched a replacement or mutated Git refs")
			}
		})
	}
}
