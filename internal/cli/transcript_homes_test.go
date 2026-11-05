package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func TestNotifyPushCarriesTranscriptHomes(t *testing.T) {
	requests := make(chan ipc.PushReceivedParams, 2)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
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
