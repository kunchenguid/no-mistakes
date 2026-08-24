package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunManagerTrustedNoCISchedulingControlsForgeActivity(t *testing.T) {
	tests := []struct {
		name          string
		trustedNoCI   *bool
		branchNoCI    *bool
		wantCI        bool
		wantForgeCall bool
	}{
		{name: "trusted true", trustedNoCI: boolPtr(true), wantCI: false, wantForgeCall: false},
		{name: "trusted false", trustedNoCI: boolPtr(false), wantCI: true, wantForgeCall: true},
		{name: "trusted absent", wantCI: true, wantForgeCall: true},
		{name: "branch only", branchNoCI: boolPtr(true), wantCI: true, wantForgeCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, database := newNoCIRunFixture(t)
			repo, _ := setupTestGitRepo(t, p, database, "no-ci-scheduling")
			mainHead, featureHead := configureNoCIBranches(t, repo, tt.trustedNoCI, tt.branchNoCI)

			fakeDir, forgeLog := writeMockGHState(t, t.TempDir(), "OPEN")
			t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			manager := NewRunManager(database, p, nil)
			t.Cleanup(manager.Shutdown)
			runID, err := manager.startRun(
				context.Background(), repo, "feature/no-ci", featureHead, mainHead,
				"test", types.AllSteps()[:len(types.AllSteps())-1], "verify no_ci scheduling",
			)
			if err != nil {
				t.Fatalf("start run: %v", err)
			}
			if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
			}

			results, err := database.GetStepsByRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			hasCI := false
			for _, result := range results {
				if result.StepName == types.StepCI {
					hasCI = true
				}
			}
			if hasCI != tt.wantCI {
				t.Fatalf("CI scheduled = %v, want %v; steps = %v", hasCI, tt.wantCI, stepNames(results))
			}

			forgeCalls, err := os.ReadFile(forgeLog)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			calledForge := len(strings.TrimSpace(string(forgeCalls))) > 0
			if calledForge != tt.wantForgeCall {
				t.Fatalf("forge called = %v, want %v; calls = %q", calledForge, tt.wantForgeCall, forgeCalls)
			}
			forgeTrace := strings.TrimSpace(string(forgeCalls))
			if forgeTrace == "" {
				forgeTrace = "<none>"
			}
			t.Logf("resolved production schedule: steps=%v; forge calls=%s", stepNames(results), forgeTrace)
		})
	}
}

func newNoCIRunFixture(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	global := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return p, database
}

func configureNoCIBranches(t *testing.T, repo *db.Repo, trustedNoCI, branchNoCI *bool) (string, string) {
	t.Helper()
	writeNoCIRepoConfig(t, repo.WorkingPath, trustedNoCI)
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "configure trusted policy")
	mainHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature/no-ci")
	writeNoCIRepoConfig(t, repo.WorkingPath, branchNoCI)
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "feature")
	featureHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature/no-ci")
	return mainHead, featureHead
}

func writeNoCIRepoConfig(t *testing.T, dir string, noCI *bool) {
	t.Helper()
	content := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\n"
	if noCI != nil {
		content += "no_ci: " + strings.ToLower(boolString(*noCI)) + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func boolPtr(value bool) *bool { return &value }

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func stepNames(results []*db.StepResult) []types.StepName {
	names := make([]types.StepName, 0, len(results))
	for _, result := range results {
		names = append(names, result.StepName)
	}
	return names
}
