package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
