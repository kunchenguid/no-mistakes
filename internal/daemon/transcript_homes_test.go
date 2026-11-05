package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type transcriptHomesAgent struct{ want string }

func (a transcriptHomesAgent) Name() string { return "fake" }
func (a transcriptHomesAgent) Close() error { return nil }
func (a transcriptHomesAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	_, data, ok := strings.Cut(opts.Prompt, "\n---\n")
	var packet struct{ Transcript string }
	if !ok || json.Unmarshal([]byte(data), &packet) != nil || !strings.Contains(packet.Transcript, a.want) || strings.Contains(packet.Transcript, "OTHER_ACCOUNT") {
		return nil, fmt.Errorf("summary did not receive only the selected synthetic account")
	}
	b, _ := json.Marshal(map[string]string{"summary": a.want})
	return &agent.Result{Output: b}, nil
}

type transcriptHomesStep struct {
	backend string
	started chan<- string
	release <-chan struct{}
}

func (*transcriptHomesStep) Name() types.StepName { return types.StepIntent }
func (s *transcriptHomesStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	// Only the fake summarizer differs from production intent extraction.
	cfg := *sctx.Config
	cfg.Intent.Enabled, cfg.Intent.Threshold = true, 0.1
	cfg.Intent.SlackDays = 1
	cfg.Intent.DisabledReaders = map[string]bool{"claude": s.backend != "claude", "codex": s.backend != "codex", "pi": true, "copilot": true, "opencode": true, "rovodev": true}
	copyContext := *sctx
	copyContext.Config, copyContext.Agent = &cfg, transcriptHomesAgent{want: sctx.Run.Branch}
	out, err := (&steps.IntentStep{}).Execute(&copyContext)
	if s.started != nil {
		s.started <- sctx.Run.ID
		select {
		case <-s.release:
		case <-sctx.Ctx.Done():
			return nil, sctx.Ctx.Err()
		}
	}
	return out, err
}

func writeDaemonAccountTranscript(t *testing.T, backend, root, repo, text string) {
	t.Helper()
	stamp := time.Now().Add(-time.Minute)
	if backend == "claude" {
		dir := filepath.Join(root, "projects", testClaudeProjectDirName(repo))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"user","cwd":` + testJSONString(t, repo) + `,"timestamp":` + testJSONString(t, stamp.UTC().Format(time.RFC3339)) + `,"uuid":"message-1","message":{"role":"user","content":` + testJSONString(t, text+" update test.txt") + `}}`
		if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"event_msg","payload":{"type":"user_message","message":` + testJSONString(t, text+" update test.txt") + `}}`
	if err := os.WriteFile(filepath.Join(root, "sessions", "rollout.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`CREATE TABLE threads (id TEXT, cwd TEXT, created_at INTEGER, updated_at INTEGER, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO threads VALUES (?, ?, ?, ?, ?)`, "same-id", repo, stamp.Unix(), time.Now().Unix(), filepath.Join("sessions", "rollout.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestPushTranscriptHomesIsolateConcurrentRunsAndReadback(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			home, daemonRoot := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", daemonRoot)
			t.Setenv("CODEX_HOME", daemonRoot)
			started, release := make(chan string, 2), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
				return []pipeline.Step{&transcriptHomesStep{backend: backend, started: started, release: release}}
			})
			repo, base := setupTestGitRepo(t, p, d, "transcript-homes")
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "commit", "-am", "change")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			writeDaemonAccountTranscript(t, backend, filepath.Join(home, "."+backend), repo.WorkingPath, "OTHER_ACCOUNT default")
			writeDaemonAccountTranscript(t, backend, daemonRoot, repo.WorkingPath, "OTHER_ACCOUNT daemon")
			want := make(map[string]*runenv.TranscriptHomes)
			for _, branch := range []string{"selected-one", "selected-two"} {
				root := filepath.Join(t.TempDir(), "account with spaces")
				writeDaemonAccountTranscript(t, backend, root, repo.WorkingPath, branch)
				homes := &runenv.TranscriptHomes{ClaudeConfigDir: root, CodexHome: root}
				gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/"+branch)
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				var result ipc.PushReceivedResult
				err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/" + branch, Old: base, New: head, TranscriptHomes: homes}, &result)
				client.Close()
				if err != nil {
					t.Fatal(err)
				}
				want[result.RunID] = homes
			}
			for range 2 {
				select {
				case <-started:
				case <-time.After(20 * time.Second):
					t.Fatal("both runs must extract intent concurrently")
				}
			}
			unblock()
			for id, homes := range want {
				run := waitForRunTerminalState(t, d, id)
				if run.Status != types.RunCompleted || run.Intent == nil || *run.Intent != run.Branch || run.TranscriptHomes == nil || *run.TranscriptHomes != *homes {
					t.Fatalf("run did not use its own account: %+v", run)
				}
				// Open a new DB handle as recovery does, then drive the real intent
				// step again using only the reloaded selection and synthetic files.
				readback, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				reloaded, err := readback.GetRun(id)
				if err != nil {
					t.Fatal(err)
				}
				reloaded.Intent = nil
				sctx := &pipeline.StepContext{Ctx: context.Background(), Run: reloaded, Repo: repo, WorkDir: repo.WorkingPath, DB: readback, Config: &config.Config{}, Log: func(string) {}}
				out, err := (&transcriptHomesStep{backend: backend}).Execute(sctx)
				readback.Close()
				if err != nil || out == nil || out.Skipped || reloaded.Intent == nil || *reloaded.Intent != reloaded.Branch {
					t.Fatalf("readback extraction = %+v, %v, intent %v", out, err, reloaded.Intent)
				}
			}
		})
	}
}
