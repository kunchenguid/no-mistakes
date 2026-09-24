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

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func TestFormatOmitIntentPushOption(t *testing.T) {
	if got := formatOmitIntentPushOption(true); got != "no-mistakes.omit-intent" {
		t.Fatalf("formatOmitIntentPushOption(true) = %q", got)
	}
	if got := formatOmitIntentPushOption(false); got != "" {
		t.Fatalf("unrequested omit = %q, want no option at all", got)
	}
}

func TestParseOmitIntentPushOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []string
		want    bool
	}{
		{"absent", []string{"no-mistakes.pr-base-branch=epic"}, false},
		{"present once", []string{"no-mistakes.omit-intent"}, true},
		{"repeated", []string{"no-mistakes.omit-intent", "no-mistakes.omit-intent"}, true},
		{"among others", []string{"no-mistakes.skip=review", "no-mistakes.omit-intent", "no-mistakes.pr-base-branch=x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOmitIntentPushOptions(tc.options)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("parseOmitIntentPushOptions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConflictingActiveRunOmitIntent(t *testing.T) {
	t.Parallel()
	omitted := &ipc.RunInfo{ID: "run-omit", OmitIntent: true}
	if err := conflictingActiveRunOmitIntent(omitted, true); err != nil {
		t.Fatalf("flag reattaching to an omitting run should reattach: %v", err)
	}
	publishing := &ipc.RunInfo{ID: "run-publish"}
	if err := conflictingActiveRunOmitIntent(publishing, false); err != nil {
		t.Fatalf("no flag should always reattach: %v", err)
	}
	err := conflictingActiveRunOmitIntent(publishing, true)
	if err == nil {
		t.Fatal("expected conflict when --no-publish-intent would be discarded by reattach")
	}
	if !strings.Contains(err.Error(), "run-publish") {
		t.Fatalf("error = %v, want it to name the active run", err)
	}
}

// olderDaemonFixture serves a fake daemon that predates the omit-intent
// capability: it answers health and run lookups but does not know the probe
// method, and it records any launch RPC it receives so a test can prove
// nothing was started.
func olderDaemonFixture(t *testing.T, probe func() (interface{}, error)) (launched *[]string) {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	root := t.TempDir()
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	cliGit(t, local, "commit", "--allow-empty", "-m", "base")
	cliGit(t, local, "checkout", "-b", "feature/omit")
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
	launched = &[]string{}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: false}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{}, nil
	})
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	if probe != nil {
		srv.Handle(ipc.MethodProbeOmitIntent, func(context.Context, json.RawMessage) (interface{}, error) { return probe() })
	}
	// An older daemon decodes these permissively: omit_intent would be dropped
	// and the run stamped to publish. Any arrival is the failure under test.
	for _, method := range []string{ipc.MethodPushReceived, ipc.MethodStartFreshRun, ipc.MethodRerun, ipc.MethodClaimLaunchReceipt} {
		method := method
		srv.Handle(method, func(context.Context, json.RawMessage) (interface{}, error) {
			*launched = append(*launched, method)
			return &ipc.RerunResult{RunID: "run-published"}, nil
		})
	}
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
	chdir(t, local)
	return launched
}

func TestAxiRunNoPublishIntentRefusesOlderDaemon(t *testing.T) {
	probes := []struct {
		name  string
		probe func() (interface{}, error)
	}{
		{name: "probe method unknown", probe: nil},
		{name: "probe declined", probe: func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: false}, nil }},
		{name: "probe undecodable", probe: func() (interface{}, error) { return json.RawMessage(`"yes"`), nil }},
	}
	// Omission can be requested by the flag or by the local global default,
	// and an unreadable global config cannot rule it out; every path must
	// reach the probe and refuse the older daemon.
	requests := []struct {
		name         string
		flag         bool
		globalConfig string
	}{
		{name: "flag", flag: true},
		{name: "global default false", globalConfig: "intent:\n  publish_intent: false\n"},
		{name: "global config unreadable", globalConfig: "intent: [\n"},
	}
	for _, rq := range requests {
		for _, tc := range probes {
			t.Run(rq.name+"/"+tc.name, func(t *testing.T) {
				launched := olderDaemonFixture(t, tc.probe)
				writeGlobalConfig(t, rq.globalConfig)
				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				cmd.SetOut(&out)
				err := runAxiRunWithLaunchProof(cmd, false, nil, "private goal", "", rq.flag, "", "", defaultAxiWait)
				if err == nil {
					t.Fatalf("axi run should refuse an older daemon when omission may apply:\n%s", out.String())
				}
				if !strings.Contains(out.String(), "too old to honor --no-publish-intent") {
					t.Fatalf("output should name the daemon capability, got:\n%s", out.String())
				}
				if len(*launched) != 0 {
					t.Fatalf("run was started on a daemon that would publish the intent: %v", *launched)
				}
			})
		}
	}
}

// TestAxiRunPublishingRunReusesOlderDaemon pins the only case that skips the
// probe: nothing requested omission (flag unset, global default true), so an
// older daemon can still serve a publishing run.
func TestAxiRunPublishingRunReusesOlderDaemon(t *testing.T) {
	olderDaemonFixture(t, nil)
	writeGlobalConfig(t, "intent:\n  publish_intent: true\n")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	_ = runAxiRunWithLaunchProof(cmd, false, nil, "public goal", "", false, "", "", defaultAxiWait)
	if strings.Contains(out.String(), "too old to honor --no-publish-intent") {
		t.Fatalf("publishing run was refused on an older daemon:\n%s", out.String())
	}
}

// TestRerunRefusesOlderDaemonWithoutFlagOrGlobalDefault pins the rerun path:
// omission can be inherited from the selected prior run, which only the daemon
// knows, so the probe runs even when neither the flag nor the global default
// requests omission. An older daemon refuses the rerun; nothing is published.
func TestRerunRefusesOlderDaemonWithoutFlagOrGlobalDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func() (interface{}, error)
	}{
		{name: "probe method unknown", probe: nil},
		{name: "probe declined", probe: func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: false}, nil }},
		{name: "probe undecodable", probe: func() (interface{}, error) { return json.RawMessage(`"yes"`), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launched := olderDaemonFixture(t, tc.probe)
			writeGlobalConfig(t, "intent:\n  publish_intent: true\n")
			var out bytes.Buffer
			cmd := newRerunCmd()
			cmd.SetArgs([]string{})
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("rerun should refuse an older daemon:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), "too old to honor --no-publish-intent") {
				t.Fatalf("error should name the daemon capability, got: %v", err)
			}
			if len(*launched) != 0 {
				t.Fatalf("rerun was started on a daemon that would publish an inherited omission: %v", *launched)
			}
		})
	}
}

func writeGlobalConfig(t *testing.T, yaml string) {
	t.Helper()
	if yaml == "" {
		return
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAxiRunNoPublishIntentPassesCapableDaemonProbe(t *testing.T) {
	olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	_ = runAxiRunWithLaunchProof(cmd, false, nil, "private goal", "", true, "", "", defaultAxiWait)
	if strings.Contains(out.String(), "too old to honor --no-publish-intent") {
		t.Fatalf("capable daemon was refused:\n%s", out.String())
	}
}
