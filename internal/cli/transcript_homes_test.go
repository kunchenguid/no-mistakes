package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func TestProofRunCarriesTranscriptHomesWithCapableDaemon(t *testing.T) {
	want := runenv.TranscriptHomes{ClaudeConfigDir: filepath.Join(t.TempDir(), "claude account"), CodexHome: filepath.Join(t.TempDir(), "codex account")}
	t.Setenv("CLAUDE_CONFIG_DIR", want.ClaudeConfigDir)
	t.Setenv("CODEX_HOME", want.CodexHome)
	requests := make(chan ipc.StartFreshRunParams, 1)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		serveTranscriptHomesProbe(srv)
		srv.Handle(ipc.MethodClaimLaunchReceipt, func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.ClaimLaunchReceiptResult{}, nil
		})
		srv.Handle(ipc.MethodStartFreshRun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var params ipc.StartFreshRunParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			requests <- params
			return &ipc.StartFreshRunResult{Receipt: ipc.LaunchReceipt{RunID: "selected-account-run"}}, nil
		})
	})
	env, err := openAxiRunEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env.close()
	local, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := env.p.RepoDir(env.repo.ID)
	cliGit(t, local, "clone", "--bare", local, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
	head := cliGit(t, local, "rev-parse", "HEAD")
	receipt, err := triggerProofRun(context.Background(), env, "feature/omit", head, nil, "goal", "", false, "nonce", "generation", "", nil, db.RunPRText{})
	if err != nil || receipt == nil || receipt.RunID != "selected-account-run" {
		t.Fatalf("supporting daemon must launch: receipt=%+v err=%v", receipt, err)
	}
	select {
	case got := <-requests:
		if got.TranscriptHomes == nil || *got.TranscriptHomes != want || got.HeadSHA != head || got.LaunchNonce != "nonce" || got.ValidationGeneration != "generation" {
			t.Fatalf("proof launch lost caller context: %+v", got)
		}
	default:
		t.Fatal("no fresh launch request received")
	}
}

func TestNotifyPushCarriesTranscriptHomes(t *testing.T) {
	requests := make(chan ipc.PushReceivedParams, 2)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		serveTranscriptHomesProbe(srv)
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
	local, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "clone", "--bare", local, p.RepoDir(repo.ID))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, custom := range []bool{false, true} {
		want := &runenv.TranscriptHomes{ClaudeConfigDir: filepath.Join(home, ".claude"), CodexHome: filepath.Join(home, ".codex")}
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		t.Setenv("CODEX_HOME", "")
		if custom {
			want = &runenv.TranscriptHomes{ClaudeConfigDir: filepath.Join(t.TempDir(), "claude account"), CodexHome: filepath.Join(t.TempDir(), "codex account")}
			t.Setenv("CLAUDE_CONFIG_DIR", want.ClaudeConfigDir)
			t.Setenv("CODEX_HOME", want.CodexHome)
		}
		if out, err := executeCmd("daemon", "notify-push", "--gate", p.RepoDir(repo.ID), "--ref", "refs/heads/feature", "--old", "base", "--new", "head"); err != nil {
			t.Fatalf("notify-push: %v: %s", err, out)
		}
		select {
		case got := <-requests:
			if got.TranscriptHomes == nil || *got.TranscriptHomes != *want {
				t.Fatalf("caller homes dropped by IPC: %+v, want %+v", got.TranscriptHomes, want)
			}
		default:
			t.Fatal("no notification received")
		}
	}
}

func serveTranscriptHomesProbe(srv *ipc.Server) {
	srv.Handle(ipc.MethodProbeCommitIdentity, func(context.Context, json.RawMessage) (interface{}, error) {
		return map[string]bool{"ok": true}, nil
	})
	srv.Handle(ipc.MethodProbeTranscriptHomes, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.ProbeTranscriptHomesResult{OK: true}, nil
	})
}

// The privacy oracle is that an older daemon receives no launch request and
// the gate does not move. Its permissive legacy handlers would otherwise start
// discovery without the account selection and could read a competing account.
func TestTranscriptHomesRefuseOlderDaemonBeforeLaunch(t *testing.T) {
	for _, launch := range []string{"notify-push", "ordinary", "proof", "rerun"} {
		t.Run(launch, func(t *testing.T) {
			launched := olderDaemonFixture(t, func() (interface{}, error) {
				return &ipc.ProbeOmitIntentResult{OK: true}, nil
			})
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "selected claude"))
			t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "selected codex"))
			env, err := openAxiRunEnv()
			if err != nil {
				t.Fatal(err)
			}
			defer env.close()
			local, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			gateDir := env.p.RepoDir(env.repo.ID)
			cliGit(t, local, "clone", "--bare", local, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
			before := cliGit(t, gateDir, "show-ref")
			cliGit(t, local, "commit", "--allow-empty", "-m", "new submission")
			head := cliGit(t, local, "rev-parse", "HEAD")
			switch launch {
			case "notify-push":
				_, err = executeCmd("daemon", "notify-push", "--gate", gateDir, "--ref", "refs/heads/feature/omit", "--old", "base", "--new", head)
			case "ordinary":
				_, err = triggerRun(context.Background(), env, "feature/omit", nil, "", "", false, "", nil)
			case "proof":
				_, err = triggerProofRun(context.Background(), env, "feature/omit", head, nil, "goal", "", false, "nonce", "generation", "", nil, db.RunPRText{})
			case "rerun":
				_, err = executeCmd("rerun")
			}
			if err == nil || !strings.Contains(err.Error(), "transcript-home selection") || !strings.Contains(err.Error(), "no-mistakes daemon restart") {
				t.Fatalf("missing capability must refuse with restart guidance: %v", err)
			}
			if len(*launched) != 0 {
				t.Fatalf("legacy daemon could discover another account's transcripts: %v", *launched)
			}
			if after := cliGit(t, gateDir, "show-ref"); after != before {
				t.Fatalf("refused launch moved the gate: before %s, after %s", before, after)
			}
		})
	}
}
