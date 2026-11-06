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

func TestPushTranscriptHomesInvalidSelectionWithExplicitIntent(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&steps.IntentStep{}}
	})
	repo, head := setupTestGitRepo(t, p, d, "invalid-transcript-home")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	homes := &runenv.TranscriptHomes{ClaudeConfigDir: "", CodexHome: ".codex-work"}
	if err := client.RequireTranscriptHomes(); err != nil {
		t.Fatalf("current daemon must confirm transcript-home support: %v", err)
	}
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: head, New: head,
		Intent: "explicit fixture intent", TranscriptHomes: homes,
	}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted || run.Intent == nil || *run.Intent != "explicit fixture intent" || run.TranscriptHomes == nil || *run.TranscriptHomes != *homes {
		t.Fatalf("invalid transcript homes blocked explicit intent: %+v", run)
	}
}

func TestPushTranscriptHomesProviderIsolationAndReadback(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			home, daemonRoot := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", daemonRoot)
			t.Setenv("CODEX_HOME", daemonRoot)
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
				return []pipeline.Step{&transcriptHomesStep{backend: backend}}
			})
			repo, base := setupTestGitRepo(t, p, d, "provider-homes")
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "commit", "-am", "change")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
			for _, provider := range []string{"claude", "codex"} {
				writeDaemonAccountTranscript(t, provider, filepath.Join(home, "."+provider), repo.WorkingPath, "OTHER_ACCOUNT default")
				writeDaemonAccountTranscript(t, provider, daemonRoot, repo.WorkingPath, "OTHER_ACCOUNT daemon")
			}
			type homeCase struct {
				name, invalid           string
				claudeValid, codexValid bool
			}
			cases := []homeCase{{name: "both-valid", claudeValid: true, codexValid: true}}
			for _, invalid := range []struct{ name, root string }{
				{"relative", ".account-work"}, {"unexpanded", "~/account"}, {"unresolvable", ""},
			} {
				for _, valid := range []struct {
					name          string
					claude, codex bool
				}{{"claude-valid", true, false}, {"codex-valid", false, true}, {"both-invalid", false, false}} {
					cases = append(cases, homeCase{invalid.name + "-" + valid.name, invalid.root, valid.claude, valid.codex})
				}
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					branch := backend + "-" + tc.name
					claude, codex := tc.invalid, tc.invalid
					if tc.claudeValid {
						claude = filepath.Join(t.TempDir(), "selected claude account")
						writeDaemonAccountTranscript(t, "claude", claude, repo.WorkingPath, branch)
					}
					if tc.codexValid {
						codex = filepath.Join(t.TempDir(), "selected codex account")
						writeDaemonAccountTranscript(t, "codex", codex, repo.WorkingPath, branch)
					}
					t.Setenv("HOME", "")
					t.Setenv("USERPROFILE", "")
					t.Setenv("CLAUDE_CONFIG_DIR", claude)
					t.Setenv("CODEX_HOME", codex)
					homes := runenv.CaptureTranscriptHomes()
					t.Setenv("HOME", home)
					t.Setenv("USERPROFILE", home)
					t.Setenv("CLAUDE_CONFIG_DIR", daemonRoot)
					t.Setenv("CODEX_HOME", daemonRoot)
					gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/"+branch)
					client, err := ipc.Dial(p.Socket())
					if err != nil {
						t.Fatal(err)
					}
					defer client.Close()
					var result ipc.PushReceivedResult
					if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
						Gate: p.RepoDir(repo.ID), Ref: "refs/heads/" + branch, Old: base, New: head, TranscriptHomes: homes,
					}, &result); err != nil {
						t.Fatal(err)
					}
					valid, key := tc.claudeValid, "CLAUDE_CONFIG_DIR"
					if backend == "codex" {
						valid, key = tc.codexValid, "CODEX_HOME"
					}
					check := func(run *db.Run) {
						t.Helper()
						if run == nil || run.Status != types.RunCompleted || run.TranscriptHomes == nil || *run.TranscriptHomes != *homes {
							t.Fatalf("caller selection did not survive run admission/readback: %+v", run)
						}
						if valid && (run.Intent == nil || *run.Intent != branch || run.IntentSource == nil || *run.IntentSource != backend) {
							t.Fatalf("valid %s account lost inference: %+v", backend, run)
						}
						if !valid && run.Intent != nil {
							t.Fatalf("invalid %s account inferred intent: %q", backend, *run.Intent)
						}
					}
					check(waitForRunTerminalState(t, d, result.RunID))
					log, err := os.ReadFile(filepath.Join(p.RunLogDir(result.RunID), "intent.log"))
					if err != nil {
						t.Fatal(err)
					}
					if !valid && !strings.Contains(string(log), key+" transcript home must be an absolute path") {
						t.Fatalf("step log lacks rejected-home diagnostic: %s", log)
					}
					readback, err := db.Open(p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer readback.Close()
					reloaded, err := readback.GetRun(result.RunID)
					if err != nil {
						t.Fatal(err)
					}
					check(reloaded)
					reloaded.Intent = nil
					var logs []string
					sctx := &pipeline.StepContext{
						Ctx: context.Background(), Run: reloaded, Repo: repo, WorkDir: repo.WorkingPath,
						DB: readback, Config: &config.Config{}, Log: func(line string) { logs = append(logs, line) },
					}
					out, err := (&transcriptHomesStep{backend: backend}).Execute(sctx)
					if err != nil || out == nil || out.Skipped == valid {
						t.Fatalf("readback inference: %+v, %v", out, err)
					}
					check(reloaded)
					if !valid && !strings.Contains(strings.Join(logs, "\n"), key+" transcript home must be an absolute path") {
						t.Fatalf("readback log lacks rejected-home diagnostic: %v", logs)
					}
				})
			}
		})
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
			// The caller clone needs a fixture-only origin when the reloaded
			// run is extracted there rather than in its disposable worktree.
			gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
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
