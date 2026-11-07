package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommitIdentityNotifyPushBindsSnapshotAndRefusesAmbiguousCheckout(t *testing.T) {
	requests := make(chan ipc.PushReceivedParams, 1)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		serveTranscriptHomesProbe(srv)
		srv.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var params ipc.PushReceivedParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			requests <- params
			return &ipc.PushReceivedResult{RunID: "snapshot-run"}, nil
		})
	})
	env, err := openAxiRunEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer env.close()
	main := env.repo.WorkingPath
	gateDir := env.p.RepoDir(env.repo.ID)
	cliGit(t, main, "clone", "--bare", main, gateDir)
	linked := filepath.Join(t.TempDir(), "linked")
	cliGit(t, main, "worktree", "add", "-b", "linked", linked)
	cliGit(t, main, "config", "extensions.worktreeConfig", "true")
	cliGit(t, linked, "config", "--worktree", "user.name", "Linked Submitter")
	cliGit(t, linked, "config", "--worktree", "user.email", "linked@example.test")
	wire := `{"author_name":"Linked Submitter","author_email":"linked@example.test","committer_name":"Linked Committer","committer_email":"committer@example.test"}`
	snapshot := commitIdentityPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(wire))
	for _, tc := range []struct {
		name    string
		options []string
		wantErr string
	}{
		{"snapshot", []string{snapshot}, ""},
		{"conflicting identities without snapshot", nil, "worktree identities differ"},
		{"bad encoding", []string{commitIdentityPushOptionPrefix + "!"}, "decode commit identity"},
		{"bad json", []string{commitIdentityPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte("{"))}, "parse commit identity"},
		{"null", []string{commitIdentityPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte("null"))}, "incomplete or invalid"},
		{"incomplete", []string{commitIdentityPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(`{"author_name":"Submitter"}`))}, "incomplete or invalid"},
		{"duplicate", []string{snapshot, snapshot}, "duplicate commit identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"daemon", "notify-push", "--gate", gateDir, "--ref", "refs/heads/linked", "--old", "base", "--new", "head"}
			for _, option := range tc.options {
				args = append(args, "--push-option", option)
			}
			out, err := executeCmd(args...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("notify-push: %v: %s, want %s", err, out, tc.wantErr)
				}
				select {
				case got := <-requests:
					t.Fatalf("refused identity reached daemon: %+v", got)
				default:
				}
				return
			}
			if err != nil {
				t.Fatalf("notify-push: %v: %s", err, out)
			}
			got := <-requests
			want := git.CommitIdentity{AuthorName: "Linked Submitter", AuthorEmail: "linked@example.test", CommitterName: "Linked Committer", CommitterEmail: "committer@example.test"}
			if got.CommitIdentity == nil || *got.CommitIdentity != want {
				t.Fatalf("snapshot replaced by main checkout: %+v", got.CommitIdentity)
			}
		})
	}
}

func TestCommitIdentityPushPathsCarryLinkedCheckoutSnapshot(t *testing.T) {
	for _, proof := range []bool{false, true} {
		name := "ordinary"
		if proof {
			name = "proof"
		}
		t.Run(name, func(t *testing.T) {
			olderDaemonFixture(t, nil, func(srv *ipc.Server) {
				serveTranscriptHomesProbe(srv)
				srv.Handle(ipc.MethodClaimLaunchReceipt, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.ClaimLaunchReceiptResult{}, nil
				})
				srv.Handle(ipc.MethodStartFreshRun, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.StartFreshRunResult{Receipt: ipc.LaunchReceipt{RunID: "snapshot-run"}}, nil
				})
				srv.Handle(ipc.MethodGetActiveRun, func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
					var params ipc.GetActiveRunParams
					if err := json.Unmarshal(raw, &params); err != nil {
						return nil, err
					}
					p, err := paths.New()
					if err != nil {
						return nil, err
					}
					gateDir := p.RepoDir(params.RepoID)
					if _, err := os.Stat(filepath.Join(gateDir, "identity-push-options")); err != nil {
						return &ipc.GetActiveRunResult{}, nil
					}
					head, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/linked")
					if err != nil {
						return nil, err
					}
					return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: "snapshot-run", HeadSHA: head, SubmittedHeadSHA: &head, Branch: "linked", Status: types.RunRunning}}, nil
				})
			})
			env, err := openAxiRunEnv()
			if err != nil {
				t.Fatal(err)
			}
			defer env.close()
			main := env.repo.WorkingPath
			gateDir := env.p.RepoDir(env.repo.ID)
			cliGit(t, main, "clone", "--bare", main, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, main, "remote", "add", gate.RemoteName, gateDir)
			linked := filepath.Join(t.TempDir(), "linked")
			cliGit(t, main, "worktree", "add", "-b", "linked", linked)
			cliGit(t, main, "config", "extensions.worktreeConfig", "true")
			cliGit(t, linked, "config", "--worktree", "user.name", "Linked Submitter")
			cliGit(t, linked, "config", "--worktree", "user.email", "linked@example.test")
			cliGit(t, linked, "commit", "--allow-empty", "-m", "linked submission")
			head := cliGit(t, linked, "rev-parse", "HEAD")
			optionsPath := filepath.Join(gateDir, "identity-push-options")
			hook := "#!/bin/sh\n: > identity-push-options\ni=0\nwhile [ \"$i\" -lt \"${GIT_PUSH_OPTION_COUNT:-0}\" ]; do\n  printenv \"GIT_PUSH_OPTION_$i\" >> identity-push-options\n  i=$((i + 1))\ndone\n"
			if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}
			chdir(t, linked)
			if proof {
				if receipt, err := triggerProofRun(context.Background(), env, "linked", head, nil, "goal", "", false, "nonce", "generation", "", nil, db.RunPRText{}); err != nil || receipt == nil || receipt.RunID != "snapshot-run" {
					t.Fatalf("proof trigger: %+v, %v", receipt, err)
				}
			} else if runID, err := triggerRun(context.Background(), env, "linked", nil, "goal", "", false, "", nil); err != nil || runID != "snapshot-run" {
				t.Fatalf("ordinary trigger: %s, %v", runID, err)
			}
			data, err := os.ReadFile(optionsPath)
			if err != nil {
				t.Fatal(err)
			}
			var got git.CommitIdentity
			found := false
			for _, option := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				encoded, ok := strings.CutPrefix(option, "no-mistakes.commit-identity=")
				if !ok {
					continue
				}
				wire, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(wire, &got); err != nil {
					t.Fatal(err)
				}
				found = true
			}
			want := git.CommitIdentity{AuthorName: "Linked Submitter", AuthorEmail: "linked@example.test", CommitterName: "Linked Submitter", CommitterEmail: "linked@example.test"}
			if !found || got != want {
				t.Fatalf("pushed snapshot = %+v, found=%v, want %+v", got, found, want)
			}
		})
	}
}

func TestCommitIdentityPlainPushRequiresLiveCheckoutAgreement(t *testing.T) {
	for _, mode := range []string{"agreement", "different author", "different committer", "missing identity", "unreadable identity", "explicit roles"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-global"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("GIT_CONFIG_PARAMETERS", "")
			for _, key := range []string{
				"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
				"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE", "EMAIL",
			} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			requests := make(chan ipc.PushReceivedParams, 1)
			olderDaemonFixture(t, nil, func(srv *ipc.Server) {
				serveTranscriptHomesProbe(srv)
				srv.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var params ipc.PushReceivedParams
					if err := json.Unmarshal(raw, &params); err != nil {
						return nil, err
					}
					requests <- params
					return &ipc.PushReceivedResult{RunID: "agreement-run"}, nil
				})
			})
			env, err := openAxiRunEnv()
			if err != nil {
				t.Fatal(err)
			}
			defer env.close()
			main := env.repo.WorkingPath
			gateDir := env.p.RepoDir(env.repo.ID)
			cliGit(t, main, "clone", "--bare", main, gateDir)
			cliGit(t, gateDir, "config", "core.hooksPath", "hooks")
			cliGit(t, main, "remote", "add", gate.RemoteName, gateDir)
			if err := os.WriteFile(filepath.Join(gateDir, "hooks", "post-receive"), []byte(git.PostReceiveHookScript()), 0o755); err != nil {
				t.Fatal(err)
			}
			linked := filepath.Join(t.TempDir(), "live linked checkout")
			stale := filepath.Join(t.TempDir(), "stale checkout")
			cliGit(t, main, "worktree", "add", "-b", "linked", linked)
			cliGit(t, main, "worktree", "add", "-b", "stale", stale)
			cliGit(t, main, "config", "extensions.worktreeConfig", "true")
			if err := os.RemoveAll(stale); err != nil {
				t.Fatal(err)
			}
			listing := cliGit(t, main, "worktree", "list", "--porcelain", "-z")
			entries, prunable := 0, 0
			for _, entry := range strings.Split(listing, "\x00\x00") {
				if entry == "" {
					continue
				}
				entries++
				for _, field := range strings.Split(entry, "\x00") {
					if field == "prunable" || strings.HasPrefix(field, "prunable ") {
						prunable++
					}
				}
			}
			if entries != 3 || prunable != 1 {
				t.Fatalf("fixture must contain main, live linked, and prunable checkouts: entries=%d prunable=%d", entries, prunable)
			}
			want := git.CommitIdentity{AuthorName: "Test", AuthorEmail: "test@example.com", CommitterName: "Test", CommitterEmail: "test@example.com"}
			switch mode {
			case "different author":
				cliGit(t, linked, "config", "--worktree", "author.name", "Linked Author")
			case "different committer":
				cliGit(t, linked, "config", "--worktree", "committer.email", "linked@example.test")
			case "missing identity":
				cliGit(t, main, "config", "--local", "--unset", "user.name")
				cliGit(t, main, "config", "--local", "--unset", "user.email")
				cliGit(t, main, "config", "--worktree", "user.name", want.AuthorName)
				cliGit(t, main, "config", "--worktree", "user.email", want.AuthorEmail)
				if _, err := git.CaptureCommitIdentity(context.Background(), linked); err == nil {
					t.Fatal("missing-identity fixture still resolves an identity")
				}
			case "unreadable identity":
				configPath := cliGit(t, linked, "rev-parse", "--git-path", "config.worktree")
				if !filepath.IsAbs(configPath) {
					configPath = filepath.Join(linked, configPath)
				}
				if err := os.WriteFile(configPath, []byte("[invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "explicit roles":
				cliGit(t, linked, "config", "--worktree", "user.name", "Different Config")
				cliGit(t, linked, "config", "--worktree", "user.email", "different@example.test")
				t.Setenv("GIT_AUTHOR_NAME", "Submitting Author")
				t.Setenv("GIT_AUTHOR_EMAIL", "author@example.test")
				t.Setenv("GIT_COMMITTER_NAME", "Submitting Committer")
				t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.test")
				want = git.CommitIdentity{AuthorName: "Submitting Author", AuthorEmail: "author@example.test", CommitterName: "Submitting Committer", CommitterEmail: "committer@example.test"}
			}
			cliGit(t, main, "commit", "--allow-empty", "-m", "plain submission")
			head := cliGit(t, main, "rev-parse", "HEAD")
			if _, err := git.Run(context.Background(), main, "push", gate.RemoteName, "HEAD:refs/heads/plain-submission"); err != nil {
				t.Fatalf("plain push transport failed: %v", err)
			}
			if got := cliGit(t, gateDir, "rev-parse", "refs/heads/plain-submission"); got != head {
				t.Fatalf("plain push landed %q, want %q", got, head)
			}
			if mode != "agreement" && mode != "explicit roles" {
				select {
				case got := <-requests:
					t.Fatalf("unprovable identity launched a run: %+v", got)
				default:
				}
				return
			}
			select {
			case got := <-requests:
				if got.CommitIdentity == nil || *got.CommitIdentity != want || got.New != head || got.Ref != "refs/heads/plain-submission" {
					t.Fatalf("plain push launch snapshot = %+v, want identity %+v at %s", got, want, head)
				}
			default:
				t.Fatal("plain push did not launch a run despite identity agreement")
			}
		})
	}
}
