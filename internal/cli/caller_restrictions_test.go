package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func cliRestrictions(t *testing.T) *callerrestrictions.Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller.txt")
	if err := os.WriteFile(path, []byte("No downloads.\nNo repeated tests.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := callerrestrictions.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCallerRestrictionsFlagsAndPushTransport(t *testing.T) {
	r := cliRestrictions(t)
	for _, cmd := range []*cobra.Command{newAxiRunCmd(), newRerunCmd()} {
		if err := cmd.ParseFlags([]string{"--caller-restrictions", r.SourcePath}); err != nil {
			t.Fatal(err)
		}
		got, err := captureCallerRestrictions(cmd)
		if err != nil || got.Content != r.Content || got.Source != callerrestrictions.Source {
			t.Fatalf("flag capture: %+v %v", got, err)
		}
	}
	options := formatCallerRestrictionsPushOptions(r)
	got, err := parseCallerRestrictionsPushOptions(options)
	if err != nil || got == nil || *got != *r {
		t.Fatalf("push round trip: %+v %v", got, err)
	}
	if _, err := parseCallerRestrictionsPushOptions(append(options, options...)); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := parseCallerRestrictionsPushOptions([]string{callerRestrictionsPushPrefix + "invalid"}); err == nil {
		t.Fatal("corrupt transport accepted")
	}
	if got, err := parseCallerRestrictionsPushOptions(nil); err != nil || got != nil {
		t.Fatal("unset control changed")
	}
}

func TestCallerRestrictionsNotifyPushDeliversCapturedBytes(t *testing.T) {
	want := cliRestrictions(t)
	requests := make(chan ipc.PushReceivedParams, 1)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		serveTranscriptHomesProbe(srv)
		srv.Handle(ipc.MethodProbeCallerRestrictions, func(context.Context, json.RawMessage) (interface{}, error) { return map[string]bool{"ok": true}, nil })
		srv.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var p ipc.PushReceivedParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			requests <- p
			return &ipc.PushReceivedResult{RunID: "synthetic-run"}, nil
		})
	})
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := findRepo(d)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := os.Getwd()
	cliGit(t, local, "clone", "--bare", local, p.RepoDir(repo.ID))
	options := formatCallerRestrictionsPushOptions(want)
	if err := os.WriteFile(want.SourcePath, []byte("changed after capture"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("daemon", "notify-push", "--gate", p.RepoDir(repo.ID), "--ref", "refs/heads/feature", "--old", "base", "--new", "head", "--push-option", options[0]); err != nil {
		t.Fatalf("notify: %s %v", out, err)
	}
	got := <-requests
	if got.CallerRestrictions == nil || *got.CallerRestrictions != *want || got.New != "head" {
		t.Fatalf("IPC lost captured restrictions: %+v", got)
	}
}

type restrictionsOldDaemon struct{ calls int }

func (c *restrictionsOldDaemon) Call(method string, _, _ any) error {
	c.calls++
	return fmt.Errorf("method not found: %s", method)
}

func TestCallerRestrictionsRefuseOlderDaemonBeforeLaunch(t *testing.T) {
	client := &restrictionsOldDaemon{}
	if err := requireDaemonCallerRestrictions(client, cliRestrictions(t)); err == nil || !strings.Contains(err.Error(), "before starting a run") {
		t.Fatalf("old daemon accepted: %v", err)
	}
	if err := requireDaemonCallerRestrictions(client, nil); err != nil || client.calls != 1 {
		t.Fatal("unset control probed new capability")
	}
}

func TestCallerRestrictionsTriggerRunChecksCompetingRun(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, name := range []string{"missing", "different content", "different source path", "different capture time", "matching", "unset request"} {
			t.Run(fmt.Sprintf("terminal=%t/%s", terminal, name), func(t *testing.T) {
				requested := cliRestrictions(t)
				held := *requested
				stored := &held
				switch name {
				case "missing":
					stored = nil
				case "different content", "unset request":
					held.Content = "Keep the synthetic repository offline.\n"
					held.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(held.Content)))
				case "different source path":
					held.SourcePath = filepath.Join(t.TempDir(), "other-caller.txt")
				case "different capture time":
					held.CapturedAt++
				}
				var competing atomic.Pointer[ipc.RunInfo]
				var headLookups atomic.Int32
				launched := olderDaemonFixture(t, nil, func(srv *ipc.Server) {
					serveTranscriptHomesProbe(srv)
					srv.Handle(ipc.MethodProbeCallerRestrictions, func(context.Context, json.RawMessage) (interface{}, error) {
						return map[string]bool{"ok": true}, nil
					})
					srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
						if terminal {
							return &ipc.GetActiveRunResult{}, nil
						}
						return &ipc.GetActiveRunResult{Run: competing.Load()}, nil
					})
					srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
						if headLookups.Add(1) == 1 {
							return &ipc.GetRunsResult{}, nil
						}
						return &ipc.GetRunsResult{Runs: []ipc.RunInfo{*competing.Load()}}, nil
					})
				})
				p, err := paths.New()
				if err != nil {
					t.Fatal(err)
				}
				database, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				repo, err := findRepo(database)
				if err != nil {
					t.Fatal(err)
				}
				local, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				gateDir := p.RepoDir(repo.ID)
				cliGit(t, local, "clone", "--bare", local, gateDir)
				cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
				cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
				head := cliGit(t, local, "rev-parse", "HEAD")
				status := types.RunRunning
				if terminal {
					status = types.RunCompleted
				}
				competing.Store(&ipc.RunInfo{ID: "competing-run", RepoID: repo.ID, Branch: "feature/omit", HeadSHA: head, Status: status, CallerRestrictionsDigest: stored.Digest()})
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				env := &axiEnv{p: p, d: database, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if name != "unset request" {
					ctx = callerrestrictions.WithLaunchRecord(ctx, requested)
				}
				runID, err := triggerRunWithPRText(ctx, env, "feature/omit", nil, "", "", false, "", nil, db.RunPRText{})
				if name == "matching" || name == "unset request" {
					if err != nil || runID != "competing-run" {
						t.Fatalf("compatible attachment: run=%q error=%v", runID, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "conflicting caller restrictions") || runID != "" {
					t.Fatalf("incompatible attachment accepted: run=%q error=%v", runID, err)
				}
				if len(*launched) != 0 {
					t.Fatalf("attachment unexpectedly launched another run: %v", *launched)
				}
			})
		}
	}
}
