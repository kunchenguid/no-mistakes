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
