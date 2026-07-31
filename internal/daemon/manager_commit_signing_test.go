package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// commitInWorktreeStep makes the same kind of commit the fix, document, and
// CI-fix steps make, so the run's terminal state is real evidence about whether
// the host's signing configuration blocked an unattended commit.
type commitInWorktreeStep struct{}

func (s *commitInWorktreeStep) Name() types.StepName { return types.StepReview }
func (s *commitInWorktreeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "fix.txt"), []byte("agent fix\n"), 0o644); err != nil {
		return nil, err
	}
	if err := git.CommitAll(context.Background(), sctx.WorkDir, "no-mistakes: apply agent fixes"); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

// An interactive signer (1Password's op-ssh-sign waits on a biometric unlock)
// never completes for the unattended daemon, so sign_commits: false must let
// the run's commits through unsigned. The default must leave signing alone.
func TestRunStart_SignCommitsFalseLetsUnattendedCommitsSucceed(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		want       types.RunStatus
	}{
		{"default preserves host signing", "log_level: info\n", types.RunFailed},
		{"sign_commits false unblocks the commit", "sign_commits: false\n", types.RunCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NM_DEMO", "1")
			p, database := newRefreshRunFixture(t)
			repo, head := setupTestGitRepo(t, p, database, "signing")
			if err := os.WriteFile(p.ConfigFile(), []byte(tt.configYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			// Force signing with a signer binary that does not exist, the
			// unattended stand-in for a signer that blocks on a human. The
			// write is on the gate's shared config, which every carved run
			// worktree inherits.
			gateDir := p.RepoDir(repo.ID)
			gitCmd(t, gateDir, "config", "commit.gpgsign", "true")
			gitCmd(t, gateDir, "config", "gpg.format", "ssh")
			gitCmd(t, gateDir, "config", "gpg.ssh.program", filepath.Join(gateDir, "no-such-signer"))
			gitCmd(t, gateDir, "config", "user.signingkey", "ssh-ed25519 AAAAfake")

			manager := NewRunManager(database, p, func() []pipeline.Step {
				return []pipeline.Step{&commitInWorktreeStep{}}
			})
			t.Cleanup(manager.Shutdown)
			runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "commit signing")
			if err != nil {
				t.Fatalf("start run: %v", err)
			}

			run := waitForRunTerminalState(t, database, runID)
			if run.Status != tt.want {
				t.Fatalf("run status = %s (want %s), error = %v", run.Status, tt.want, run.Error)
			}
			t.Logf("observable result: config=%q status=%s", tt.configYAML, run.Status)
		})
	}
}
