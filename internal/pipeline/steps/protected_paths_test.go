package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_ProtectedPathRetryFinishesRetainedRepairWithGreenChecks(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		name := "publish"
		if revalidate {
			name = "revalidate"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			f := newCIRepairFixture(t, revalidate, func(dir string) {
				calls++
				for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
					if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			})
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
			}
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			outcome, err = f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) || calls != 1 || f.localHead(t) != f.headSHA {
				t.Fatalf("unresolved retry bypassed refusal or reran fixer: %+v, %v calls=%d", outcome, err, calls)
			}
			if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
				t.Fatal(err)
			}
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			outcome, err = f.run(t)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("retry: %+v, %v\n%s", outcome, err, f.log())
			}
			if f.localHead(t) == f.headSHA {
				t.Fatalf("green checks bypassed retained repair: remote=%s dirty=%q\n%s", f.remoteHead(t), gitStatusPorcelain(t, f.dir), f.log())
			}
			if calls != 1 {
				t.Fatalf("retry ran another fixer over retained work: %d calls", calls)
			}
			if got := gitCmd(t, f.dir, "show", "HEAD:fix.go"); got != "retained repair" {
				t.Fatalf("commit lost retained repair: %q", got)
			}
			if revalidate {
				if outcome == nil || outcome.RestartFrom != types.StepReview || f.remoteHead(t) != f.headSHA {
					t.Fatalf("retry skipped required pipeline revalidation: %+v remote=%s", outcome, f.remoteHead(t))
				}
				if strings.Contains(f.log(), ciChecksPassedMsg) {
					t.Fatal("reported checks passed before revalidation/publication")
				}
			} else if f.remoteHead(t) != f.localHead(t) || !strings.Contains(f.log(), ciChecksPassedMsg) {
				t.Fatalf("retry did not publish before monitoring: local=%s remote=%s\n%s", f.localHead(t), f.remoteHead(t), f.log())
			}
		})
	}
}

func TestCIStep_ProtectedPathRetryPublicationFailureKeepsRefusal(t *testing.T) {
	f := newCIRepairFixture(t, false, func(dir string) {
		for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	f.sctx.Config.ProtectedPaths = []string{"*.lock"}
	outcome, err := f.run(t)
	if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
		t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
	}
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = outcome.Findings
	if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
		t.Fatal(err)
	}
	f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
	hooks := t.TempDir()
	gitCmd(t, f.upstream, "config", "core.hooksPath", hooks)
	rejectPush := filepath.Join(hooks, "pre-receive")
	if err := os.WriteFile(rejectPush, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.run(t)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
		t.Fatalf("unfinished publication lost refusal: %+v, %v\n%s", outcome, err, f.log())
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].File != "package.lock" || !strings.Contains(findings.Items[0].Description, `rule "*.lock"`) {
		t.Fatalf("retry lost original path or rule: %+v, %v", findings, err)
	}
	if strings.Contains(f.log(), ciChecksPassedMsg) || f.remoteHead(t) != f.headSHA {
		t.Fatal("unfinished publication advanced remote or reported checks passed")
	}
	if got, err := os.ReadFile(filepath.Join(f.dir, "fix.go")); err != nil || string(got) != "retained repair\n" {
		t.Fatalf("lost retained work: %q, %v", got, err)
	}
	f.sctx.PreviousFindings = outcome.Findings
	if err := os.Remove(rejectPush); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.run(t)
	if !errors.Is(err, context.Canceled) || f.remoteHead(t) == f.headSHA || f.remoteHead(t) != f.localHead(t) {
		t.Fatalf("second explicit retry did not finish publication: %+v, %v\n%s", outcome, err, f.log())
	}
}

func TestCIStep_ProtectedPathRefusalStopsAutomaticAndManualRepair(t *testing.T) {
	t.Parallel()
	for _, manual := range []bool{false, true} {
		name := "automatic"
		if manual {
			name = "manual"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			invocations := 0
			var indexBefore string
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				invocations++
				if err := os.WriteFile(filepath.Join(dir, "package.lock"), []byte("staged lock\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "package.lock")
				indexBefore = gitCmd(t, dir, "diff", "--cached")
				for file, content := range map[string]string{"package.lock": "rejected edit\n", "fix.txt": "CI repair\n"} {
					if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair checks","code_change_needed":true}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.ProtectedPaths = []string{"*.lock"}
			sctx.Config.AutoFix.CI = 3
			sctx.Config.CITimeout = time.Minute
			sctx.Fixing = manual
			polls := 0
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				polls++
				return nil
			}}
			outcome, err := step.Execute(sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("refusal must park for an operator: outcome=%+v err=%v", outcome, err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("refusal findings=%+v err=%v", findings, err)
			}
			finding := findings.Items[0]
			if finding.File != "package.lock" || finding.Action != types.ActionAskUser || !strings.Contains(finding.Description, `rule "*.lock"`) {
				t.Errorf("refusal lost the path, rule, or decision: %+v", finding)
			}
			if invocations != 1 || polls != 0 {
				t.Errorf("refusal retried: invocations=%d polls=%d", invocations, polls)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("refusal committed: HEAD=%s want %s", got, headSHA)
			}
			if got := gitCmd(t, dir, "diff", "--cached"); got != indexBefore {
				t.Errorf("refusal changed the index: %q", got)
			}
			for file, want := range map[string]string{"package.lock": "rejected edit\n", "fix.txt": "CI repair\n"} {
				if got, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(got) != want {
					t.Errorf("refusal discarded %s: %q err=%v", file, got, err)
				}
			}
		})
	}
}

func TestTestStep_FixMode_ProtectedPathDoesNotReachCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	const ledger = "generated-ledger.json"
	if err := os.WriteFile(filepath.Join(dir, ledger), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ledger)
	gitCmd(t, dir, "commit", "-m", "add tool-owned ledger")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		for file, content := range map[string]string{ledger: "unrelated agent edit\n", "fix.txt": "test repair\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair test failure"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	repo, err := config.LoadRepoFromBytes([]byte("commands:\n  test: exit 0\nprotected_paths:\n  - generated-ledger.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	sctx.Config = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(repo, repo, false))
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","file":"fix.txt","description":"test failed"}]}`
	_, err = (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), ledger) {
		t.Errorf("expected a surfaced protected-path error naming %s, got %v", ledger, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Errorf("fix committed an out-of-scope edit: HEAD = %s, want %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("refusal staged changes: %q", got)
	}
	if got := gitStatusPorcelain(t, dir); !strings.Contains(got, ledger) || !strings.Contains(got, "fix.txt") {
		t.Errorf("refusal must preserve both edits for inspection, status = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, ledger)); err != nil || string(got) != "unrelated agent edit\n" {
		t.Errorf("protected edit was discarded: content = %q, err = %v", got, err)
	}
}

func TestProtectedPaths_AllAutomaticCommitPathsRefuseWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, caller := range []struct {
		name string
		run  func(*pipeline.StepContext) error
	}{
		{"fix", func(sctx *pipeline.StepContext) error {
			return commitAgentFixes(sctx, types.StepDocument, "update docs", "")
		}},
		{"push", func(sctx *pipeline.StepContext) error {
			_, err := (&PushStep{}).Execute(sctx)
			return err
		}},
		{"ci", func(sctx *pipeline.StepContext) error {
			_, err := (&CIStep{}).commitRepair(sctx, "repair checks")
			return err
		}},
	} {
		t.Run(caller.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.ProtectedPaths = []string{"feature.txt"}
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("unrelated residue"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "feature.txt")
			before := gitCmd(t, dir, "diff", "--cached")
			err := caller.run(sctx)
			if err == nil || !strings.Contains(err.Error(), "feature.txt") {
				t.Fatalf("expected protected-path refusal, got %v", err)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("HEAD advanced from %s to %s", headSHA, got)
			}
			if got := gitCmd(t, dir, "diff", "--cached"); got != before {
				t.Errorf("refusal altered the index: %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "feature.txt")); err != nil || string(got) != "unrelated residue" {
				t.Errorf("refusal altered the worktree: %q, %v", got, err)
			}
		})
	}
}

func TestProtectedPaths_Staging(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pattern, edit, file string
		blocked                   bool
	}{
		{"unstaged", "feature.txt", "write", "feature.txt", true},
		{"deleted", "feature.txt", "delete", "feature.txt", true},
		{"rename_source", "feature.txt", "rename", "renamed.txt", true},
		{"rename_destination", "renamed.txt", "rename", "renamed.txt", true},
		{"new_directory", "*.lock", "write", "new/nested/package.lock", true},
		{"subtree", "new/**", "write", "new/nested/package.lock", true},
		{"literal_path", "new/nested/package.lock", "write", "new/nested/package.lock", true},
		{"spaces_and_unicode", "*.lock", "write", "new/ ledger ü.lock", true},
		{"glob_does_not_cross_slash", "new/*.lock", "write", "new/nested/package.lock", false},
		{"clean_protected_file", "feature.txt", "write", "fix.txt", false},
		{"default_opt_out", "", "write", "feature.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			if tc.pattern != "" {
				sctx.Config.ProtectedPaths = []string{tc.pattern}
			}
			file := filepath.Join(dir, filepath.FromSlash(tc.file))
			switch tc.edit {
			case "write":
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("agent edit"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "rename":
				gitCmd(t, dir, "mv", "feature.txt", tc.file)
			}
			before := gitCmd(t, dir, "diff", "--cached")
			err := stagePipelineChanges(sctx)
			if tc.blocked {
				if err == nil || !strings.Contains(err.Error(), "protected") {
					t.Fatalf("expected protected-path error, got %v", err)
				}
				if got := gitCmd(t, dir, "diff", "--cached"); got != before {
					t.Errorf("refusal changed index: %q", got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); !strings.Contains(got, tc.file) {
					t.Errorf("allowed edit not staged: %q", got)
				}
			}
		})
	}
}

func TestProtectedPaths_UnreadableStatusFailsClosed(t *testing.T) {
	t.Parallel()
	sctx := newTestContext(t, &mockAgent{}, t.TempDir(), "", "", config.Commands{})
	sctx.Config.ProtectedPaths = []string{"*.lock"}
	if err := stagePipelineChanges(sctx); err == nil || !strings.Contains(err.Error(), "check protected_paths") {
		t.Fatalf("unreadable git status did not fail closed: %v", err)
	}
}
