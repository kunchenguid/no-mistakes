/*
@overview Loads one run's repository configuration from its immutable submitted commit.

	READING GUIDE
	-------------
	1. Start at loadSubmittedRepoConfig  <- run-level snapshot entry point
	2. submittedConfigSHA               <- new-run and legacy SHA selection

	MAIN FLOW
	---------
	run metadata -> submitted SHA -> git tree lookup -> parsed RepoConfig

	PUBLIC API
	----------
	None; this file is internal to package daemon.

	INTERNALS
	---------
	loadSubmittedRepoConfig, loadRepoConfigAtCommit, submittedConfigSHA

@exports
@deps internal/config, internal/db, internal/git
*/
package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// -- 1/2 CORE · loadSubmittedRepoConfig <- START HERE --

// loadSubmittedRepoConfig reconstructs the repository config snapshot for a
// run from the immutable commit it originally submitted. It never reads the
// mutable worktree file or a remote-tracking branch.
func loadSubmittedRepoConfig(ctx context.Context, workDir string, run *db.Run) (*config.RepoConfig, error) {
	return loadRepoConfigAtCommit(ctx, workDir, submittedConfigSHA(run))
}

// loadRepoConfigAtCommit loads .no-mistakes.yaml from one exact commit. A
// readable tree without the file is a valid empty repo config; it never falls
// back to another clone or the trusted default branch.
func loadRepoConfigAtCommit(ctx context.Context, workDir, commitSHA string) (*config.RepoConfig, error) {
	commitSHA = strings.TrimSpace(commitSHA)
	if commitSHA == "" {
		return nil, fmt.Errorf("load repo config snapshot: submitted commit is empty")
	}
	entry, err := git.Run(ctx, workDir, "ls-tree", commitSHA, "--", ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("load repo config snapshot at %s: %w", commitSHA, err)
	}
	if strings.TrimSpace(entry) == "" {
		return &config.RepoConfig{}, nil
	}
	content, err := git.ShowFile(ctx, workDir, commitSHA, ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("load repo config snapshot at %s: %w", commitSHA, err)
	}
	repoCfg, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("load repo config snapshot at %s: %w", commitSHA, err)
	}
	return repoCfg, nil
}

// -/ 1/2

// -- 2/2 HELPER · submittedConfigSHA --

// submittedConfigSHA uses the immutable submitted head for all current runs.
// Legacy rows created before submitted_head_sha existed fall back to head_sha,
// which is the only commit identity those rows retained.
func submittedConfigSHA(run *db.Run) string {
	if run == nil {
		return ""
	}
	if run.SubmittedHeadSHA != nil && strings.TrimSpace(*run.SubmittedHeadSHA) != "" {
		return strings.TrimSpace(*run.SubmittedHeadSHA)
	}
	return strings.TrimSpace(run.HeadSHA)
}

// -/ 2/2
