package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func captureSubmissionIdentity(ctx context.Context, client *ipc.Client, dir string) (*git.CommitIdentity, error) {
	if err := client.RequireCommitIdentity(); err != nil {
		return nil, err
	}
	identity, err := git.CaptureCommitIdentity(ctx, dir)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

// A receive hook runs inside the bare gate. Resolve the registered submitting
// checkout, and discard receive-pack's repository-routing variables only. The
// pushing process's identity and config environment must still reach Git.
func captureHookIdentity(ctx context.Context, client *ipc.Client, p *paths.Paths, gateDir string) (*git.CommitIdentity, error) {
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		return nil, err
	}
	defer database.Close()
	repo, err := database.GetRepo(strings.TrimSuffix(filepath.Base(gateDir), ".git"))
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("cannot bind identity for an unregistered gate")
	}
	ctx = git.WithEnvironment(ctx, runenv.Overlay{Unset: []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"}})
	return captureSubmissionIdentity(ctx, client, repo.WorkingPath)
}
