package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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
