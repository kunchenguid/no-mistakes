package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestFreshRunBranchOwnershipDistinguishesMissingTerminalHead reproduces the
// custody deadlock caused by a terminal run whose moved head is no longer
// available. That state must remain manual-only for recovery, while it must
// not keep unrelated fresh work behind a custody claim for commits that no
// longer exist.
func TestFreshRunBranchOwnershipDistinguishesMissingTerminalHead(t *testing.T) {
	for _, tc := range []struct {
		name             string
		recordedHead     func(t *testing.T, submitted string) string
		advanceWorktree  bool
		gatePresent      bool
		survivingAnchor  bool
		verifiedHead     bool
		objectReadError  bool
		wantFreshBlocked bool
		wantSafety       string
	}{
		{
			name: "terminal unmoved releases branch",
			recordedHead: func(_ *testing.T, submitted string) string {
				return submitted
			},
			verifiedHead:     true,
			wantFreshBlocked: false,
			wantSafety:       "user_owned",
		},
		{
			name: "terminal recoverable moved head keeps custody",
			recordedHead: func(_ *testing.T, submitted string) string {
				return submitted
			},
			advanceWorktree:  true,
			verifiedHead:     true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_pipeline_owned_recoverable",
		},
		{
			name: "terminal missing moved head releases fresh path",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			wantFreshBlocked: false,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "missing head with recovery evidence keeps custody",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			survivingAnchor:  true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "object read failure keeps custody",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			objectReadError:  true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir, paths, database, repo := setupAxiQueryRepo(t)
			cliGit(t, repoDir, "checkout", "-b", "feature/missing-head")
			chdir(t, repoDir)

			submitted := cliGit(t, repoDir, "rev-parse", "HEAD")
			recorded := tc.recordedHead(t, submitted)
			if tc.advanceWorktree {
				cliGit(t, repoDir, "commit", "--allow-empty", "-m", "pipeline fix")
				recorded = cliGit(t, repoDir, "rev-parse", "HEAD")
			}
			if tc.gatePresent {
				gateDir := paths.RepoDir(repo.ID)
				if err := os.MkdirAll(gateDir, 0o755); err != nil {
					t.Fatalf("create gate directory: %v", err)
				}
				cliGit(t, gateDir, "init", "--bare")
				cliGit(t, repoDir, "push", gateDir, "HEAD:refs/heads/feature/missing-head")
				if tc.objectReadError {
					objectPath := filepath.Join(gateDir, "objects", recorded[:2], recorded[2:])
					if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
						t.Fatalf("create corrupt object directory: %v", err)
					}
					if err := os.WriteFile(objectPath, []byte("corrupt object"), 0o644); err != nil {
						t.Fatalf("write corrupt object: %v", err)
					}
				}
			}

			pipelineRun, err := database.InsertRun(repo.ID, "feature/missing-head", submitted, submitted)
			if err != nil {
				t.Fatalf("insert pipeline run: %v", err)
			}
			if err := database.UpdateRunHeadSHA(pipelineRun.ID, recorded); err != nil {
				t.Fatalf("record pipeline head: %v", err)
			}
			if tc.verifiedHead {
				if err := database.UpdateRunStatusWithVerifiedHead(pipelineRun.ID, types.RunCancelled, recorded); err != nil {
					t.Fatalf("terminalize pipeline run: %v", err)
				}
			} else if err := database.UpdateRunStatus(pipelineRun.ID, types.RunCancelled); err != nil {
				t.Fatalf("terminalize pipeline run: %v", err)
			}
			if tc.survivingAnchor {
				cliGit(t, paths.RepoDir(repo.ID), "update-ref", custody.RecoveryRef(pipelineRun.ID), submitted)
			}

			env := &axiEnv{p: paths, d: database, repo: repo, cfg: config.DefaultGlobalConfig()}
			state := inspectAxiBranchSync(context.Background(), env)
			if state.Safety != tc.wantSafety {
				t.Fatalf("branch ownership state = %s, want %s: %#v", state.Safety, tc.wantSafety, state)
			}
			blocked := freshRunBranchOwnershipState(context.Background(), env)
			if (blocked != nil) != tc.wantFreshBlocked {
				t.Fatalf("fresh-run ownership = %#v, blocked = %t, want blocked = %t", blocked, blocked != nil, tc.wantFreshBlocked)
			}
			if tc.wantFreshBlocked && blocked.NextAction == nil {
				t.Fatal("recoverable terminal head lost its custody guidance")
			}
			if tc.wantSafety == "blocked_recover_preserved_head_missing" &&
				(state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually") {
				t.Fatalf("missing-head state lost manual reconciliation guidance: %#v", state)
			}
		})
	}
}
