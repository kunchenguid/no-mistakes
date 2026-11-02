package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPriorityPushOptionRoundTrip(t *testing.T) {
	if got := formatPriorityPushOption(0); got != "" {
		t.Fatalf("ordinary priority formats to %q, want no option", got)
	}
	for _, priority := range []int{1, 10, 20, types.RunPriorityMax} {
		option := formatPriorityPushOption(priority)
		got, err := parsePriorityPushOptions([]string{"no-mistakes.skip=lint", option})
		if err != nil {
			t.Fatalf("parse %q: %v", option, err)
		}
		if got != priority {
			t.Fatalf("parse %q = %d, want %d", option, got, priority)
		}
	}
	got, err := parsePriorityPushOptions(nil)
	if err != nil || got != types.RunPriorityOrdinary {
		t.Fatalf("no option = %d, %v; want ordinary", got, err)
	}
	got, err = parsePriorityPushOptions([]string{"no-mistakes.priority=10", "no-mistakes.priority=20"})
	if err != nil || got != 20 {
		t.Fatalf("repeated option = %d, %v; want the last one, 20", got, err)
	}
}

func TestParsePriorityPushOptionsRefusesMalformedValues(t *testing.T) {
	for _, value := range []string{"", "high", "1.5", "-1", "101"} {
		if _, err := parsePriorityPushOptions([]string{"no-mistakes.priority=" + value}); err == nil {
			t.Fatalf("priority %q accepted, want refusal", value)
		}
	}
}

func TestRunPriorityFromFlags(t *testing.T) {
	newCmd := func(args ...string) *cobra.Command {
		t.Helper()
		cmd := &cobra.Command{Use: "run"}
		bindRunPriorityFlag(cmd)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	priority, set, err := runPriorityFromFlags(newCmd())
	if err != nil || set || priority != types.RunPriorityOrdinary {
		t.Fatalf("unset flag = %d, %t, %v; want ordinary and not set", priority, set, err)
	}
	priority, set, err = runPriorityFromFlags(newCmd("--priority", "20"))
	if err != nil || !set || priority != 20 {
		t.Fatalf("--priority 20 = %d, %t, %v", priority, set, err)
	}
	priority, set, err = runPriorityFromFlags(newCmd("--priority", "0"))
	if err != nil || !set || priority != 0 {
		t.Fatalf("explicit --priority 0 = %d, %t, %v; want set ordinary", priority, set, err)
	}
	for _, value := range []string{"-1", "101"} {
		if _, _, err := runPriorityFromFlags(newCmd("--priority", value)); err == nil {
			t.Fatalf("--priority %s accepted, want refusal", value)
		}
	}
}

func TestConflictingActiveRunPriority(t *testing.T) {
	active := &ipc.RunInfo{ID: "run-1", Priority: 10}
	if err := conflictingActiveRunPriority(active, 20, false); err != nil {
		t.Fatalf("reattach without --priority refused: %v", err)
	}
	if err := conflictingActiveRunPriority(active, 10, true); err != nil {
		t.Fatalf("reattach with the same priority refused: %v", err)
	}
	err := conflictingActiveRunPriority(active, 20, true)
	if err == nil || !strings.Contains(err.Error(), "priority 10") {
		t.Fatalf("reattach with a different priority = %v, want refusal naming the active priority", err)
	}
	if err := conflictingActiveRunPriority(&ipc.RunInfo{ID: "run-2"}, 0, true); err != nil {
		t.Fatalf("explicit ordinary priority on an ordinary run refused: %v", err)
	}
}

func TestRunObjectRendersOnlyANonOrdinaryPriority(t *testing.T) {
	rv := runView{ID: "run-1", Branch: "fix/red-main", Status: string(types.RunPending), HeadSHA: "abcdef1234567890", Priority: 20}
	if out := axiDoc(runObjectField(rv)); !strings.Contains(out, "  priority: 20\n") {
		t.Fatalf("run object omits its priority:\n%s", out)
	}
	rv.Priority = 0
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "priority") {
		t.Fatalf("ordinary run renders a priority:\n%s", out)
	}
	if got := runViewFromIPC(&ipc.RunInfo{ID: "run-1", Priority: 10}).Priority; got != 10 {
		t.Fatalf("IPC view priority = %d, want 10", got)
	}
	if got := runViewFromDB(&db.Run{ID: "run-1", Priority: 10}, nil, nil).Priority; got != 10 {
		t.Fatalf("DB view priority = %d, want 10", got)
	}
}

// notify-push is what the gate's receive hook runs: it must forward the
// priority push option to the daemon, and refuse a malformed one before
// contacting it.
func TestNotifyPushForwardsPriority(t *testing.T) {
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	gateDir := filepath.Join(p.Root(), "repos", "repo.git")
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	received := make(chan ipc.PushReceivedParams, 4)
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.PushReceivedParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		received <- params
		return &ipc.PushReceivedResult{RunID: "run-1"}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	args := func(options ...string) []string {
		out := []string{"daemon", "notify-push", "--gate", gateDir, "--ref", "refs/heads/fix", "--old", "0000000000000000000000000000000000000000", "--new", "1111111111111111111111111111111111111111"}
		for _, option := range options {
			out = append(out, "--push-option", option)
		}
		return out
	}

	if _, err := executeCmd(args("no-mistakes.priority=20")...); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got.Priority != 20 {
		t.Fatalf("forwarded priority = %d, want 20", got.Priority)
	}
	if _, err := executeCmd(args()...); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got.Priority != types.RunPriorityOrdinary {
		t.Fatalf("forwarded priority without the option = %d, want ordinary", got.Priority)
	}
	if _, err := executeCmd(args("no-mistakes.priority=urgent")...); err == nil {
		t.Fatal("malformed priority push option accepted")
	}
	select {
	case got := <-received:
		t.Fatalf("malformed priority still notified the daemon: %+v", got)
	default:
	}
}

// axi run carries --priority to the gate as a git push option, which is what
// the receive hook hands to notify-push. An ordinary run sends none.
func TestTriggerRunSendsThePriorityPushOption(t *testing.T) {
	for _, tc := range []struct {
		priority int
		want     string
	}{{20, "no-mistakes.priority=20"}, {0, ""}} {
		t.Run(formatPriorityPushOption(tc.priority), func(t *testing.T) {
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
			cliGit(t, dir, "config", "user.name", "Test")
			cliGit(t, dir, "config", "user.email", "test@example.com")
			cliGit(t, dir, "commit", "--allow-empty", "-m", "base")
			repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, dir, "clone", "--bare", dir, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)
			cliGit(t, dir, "commit", "--allow-empty", "-m", "fix")
			optionsFile := filepath.Join(t.TempDir(), "push-options")
			hook := "#!/bin/sh\n" +
				"i=0\n" +
				"while [ \"$i\" -lt \"${GIT_PUSH_OPTION_COUNT:-0}\" ]; do\n" +
				"  eval \"printf '%s\\n' \\\"\\$GIT_PUSH_OPTION_$i\\\"\" >> '" + optionsFile + "'\n" +
				"  i=$((i+1))\n" +
				"done\n" +
				"echo submission-recorded >&2\n" +
				"exit 1\n"
			if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetRunsResult{}, nil
			})
			srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetActiveRunResult{}, nil
			})
			done := make(chan error, 1)
			go func() { done <- srv.Serve(p.Socket()) }()
			t.Cleanup(func() { srv.Close(); <-done })
			var client *ipc.Client
			deadline := time.Now().Add(3 * time.Second)
			for {
				client, err = ipc.Dial(p.Socket())
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			defer client.Close()
			chdir(t, dir)
			env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := triggerRun(ctx, env, "main", nil, "repair red main", "", false, "", nil, tc.priority, tc.priority != 0); err == nil || !strings.Contains(err.Error(), "submission-recorded") {
				t.Fatalf("triggerRun error = %v, want the recording hook's refusal", err)
			}
			raw, err := os.ReadFile(optionsFile)
			if err != nil {
				t.Fatal(err)
			}
			options := strings.Split(strings.TrimSpace(string(raw)), "\n")
			var priorityOptions []string
			for _, option := range options {
				if strings.HasPrefix(option, priorityPushOptionPrefix) {
					priorityOptions = append(priorityOptions, option)
				}
			}
			if tc.want == "" {
				if len(priorityOptions) != 0 {
					t.Fatalf("ordinary run sent %v, want no priority option", priorityOptions)
				}
				return
			}
			if len(priorityOptions) != 1 || priorityOptions[0] != tc.want {
				t.Fatalf("priority push options = %v (all: %v), want [%s]", priorityOptions, options, tc.want)
			}
		})
	}
}

// A no-op push reaches AXI's rerun fallback. An explicit `--priority 0` must
// travel as set so the daemon does not inherit the previous run's urgency; an
// omitted priority must not claim it was set.
func TestTriggerRunRerunFallbackCarriesAnExplicitOrdinaryPriority(t *testing.T) {
	for _, tc := range []struct {
		name         string
		priority     int
		set          bool
		wantPriority int
		wantSet      bool
	}{
		{"explicit ordinary", 0, true, 0, true},
		{"omitted", 0, false, 0, false},
		{"explicit urgent", 20, true, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			cliGit(t, dir, "config", "user.name", "Test")
			cliGit(t, dir, "config", "user.email", "test@example.com")
			cliGit(t, dir, "commit", "--allow-empty", "-m", "base")
			repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, dir, "clone", "--bare", dir, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)

			srv := ipc.NewServer()
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetRunsResult{}, nil
			})
			srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetActiveRunResult{}, nil
			})
			requests := make(chan ipc.RerunParams, 1)
			srv.Handle(ipc.MethodRerun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.RerunParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				requests <- params
				return &ipc.RerunResult{RunID: "rerun-1"}, nil
			})
			done := make(chan error, 1)
			go func() { done <- srv.Serve(p.Socket()) }()
			t.Cleanup(func() { srv.Close(); <-done })
			var client *ipc.Client
			deadline := time.Now().Add(3 * time.Second)
			for {
				client, err = ipc.Dial(p.Socket())
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			defer client.Close()
			chdir(t, dir)
			env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			runID, err := triggerRun(ctx, env, "main", nil, "repair red main", "", false, "", nil, tc.priority, tc.set)
			if err != nil || runID != "rerun-1" {
				t.Fatalf("no-op push fallback: run=%q err=%v", runID, err)
			}
			got := <-requests
			if got.Priority != tc.wantPriority || got.PrioritySet != tc.wantSet {
				t.Fatalf("rerun priority = %d (set=%v), want %d (set=%v)", got.Priority, got.PrioritySet, tc.wantPriority, tc.wantSet)
			}
		})
	}
}
